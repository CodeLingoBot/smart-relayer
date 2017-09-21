package rsqs

import (
	"errors"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/gallir/radix.improved/redis"
	"github.com/gallir/smart-relayer/lib"
)

// Server is the thread that listen for clients' connections
type Server struct {
	sync.Mutex
	config   lib.RelayerConfig
	done     chan bool
	exiting  bool
	reseting bool
	failing  bool
	listener net.Listener
	tries    int

	clients        []*Client
	recordsCh      chan *lib.InterRecord
	awsSvc         *sqs.SQS
	lastConnection time.Time
	lastError      time.Time
	errors         int64
	fifo           bool
}

const (
	maxConnections      = 1
	requestBufferSize   = 10 * 2
	maxConnectionsTries = 3
	connectionRetry     = 5 * time.Second
	errorsFrame         = 10 * time.Second
	maxErrors           = 10 // Limit of errors to restart the connection
	connectTimeout      = 5 * time.Second
)

var (
	errBadCmd      = errors.New("ERR bad command")
	errKO          = errors.New("fatal error")
	errOverloaded  = errors.New("Redis overloaded")
	respOK         = redis.NewRespSimple("OK")
	respTrue       = redis.NewResp(1)
	respBadCommand = redis.NewResp(errBadCmd)
	respKO         = redis.NewResp(errKO)
	commands       map[string]*redis.Resp
)

func init() {
	commands = map[string]*redis.Resp{
		"PING":   respOK,
		"MULTI":  respOK,
		"EXEC":   respOK,
		"SET":    respOK,
		"SADD":   respOK,
		"HMSET":  respOK,
		"RAWSET": respOK,
	}
}

// New creates a new Redis local server
func New(c lib.RelayerConfig, done chan bool) (*Server, error) {
	srv := &Server{
		done:      done,
		errors:    0,
		recordsCh: make(chan *lib.InterRecord, requestBufferSize),
	}

	srv.Reload(&c)

	return srv, nil
}

// Reload the configuration
func (srv *Server) Reload(c *lib.RelayerConfig) (err error) {
	srv.Lock()
	defer srv.Unlock()

	srv.config = *c

	if srv.config.MaxConnections <= 0 {
		srv.config.MaxConnections = maxConnections
	}

	if srv.config.MaxRecords <= 0 {
		srv.config.MaxRecords = maxBatchRecords
	}

	if strings.HasSuffix(srv.config.URL, "fifo") {
		srv.fifo = true
		if srv.config.GroupID == "" {
			srv.config.GroupID = srv.config.ListenHost()
		}
	} else {
		srv.fifo = false
		srv.config.GroupID = ""
	}

	go srv.retry()

	return nil
}

// Start accepts incoming connections on the Listener
func (srv *Server) Start() (e error) {
	srv.Lock()
	defer srv.Unlock()

	srv.listener, e = lib.NewListener(srv.config)
	if e != nil {
		return e
	}

	// Serve clients
	go func(l net.Listener) {
		defer srv.listener.Close()
		for {
			netConn, e := l.Accept()
			if e != nil {
				if netErr, ok := e.(net.Error); ok && netErr.Timeout() {
					// Paranoid, ignore timeout errors
					log.Println("SQS ERROR: timeout at local listener", srv.config.ListenHost(), e)
					continue
				}
				if srv.exiting {
					log.Println("SQS: exiting local listener", srv.config.ListenHost())
					return
				}
				log.Fatalln("SQS ERROR: emergency error in local listener", srv.config.ListenHost(), e)
				return
			}
			go srv.handleConnection(netConn)
		}
	}(srv.listener)

	return
}

// Exit closes the listener and send done to main
func (srv *Server) Exit() {
	srv.exiting = true

	if srv.listener != nil {
		srv.listener.Close()
	}

	for _, c := range srv.clients {
		c.Exit()
	}

	if len(srv.recordsCh) > 0 {
		log.Printf("SQS: messages lost %d", len(srv.recordsCh))
	}

	// finishing the server
	srv.done <- true
}

func (srv *Server) canSend() bool {
	if srv.reseting || srv.exiting || srv.failing {
		return false
	}

	return true
}

func (srv *Server) sendRecord(r *lib.InterRecord) {
	srv.recordsCh <- r
}

func (srv *Server) sendBytes(b []byte) {
	r := &lib.InterRecord{
		Types: 1,
		Raw:   b,
	}
	srv.sendRecord(r)
}

func (srv *Server) handleConnection(netCon net.Conn) {

	defer netCon.Close()

	reader := redis.NewRespReader(netCon)

	// Active transaction
	multi := false

	var row *lib.InterRecord
	defer func() {
		if multi {
			log.Println("SQS ERROR: MULTI closed before ending with EXEC")
		}
	}()

	for {

		r := reader.Read()

		if r.IsType(redis.IOErr) {
			if redis.IsTimeout(r) {
				// Paranoid, don't close it just log it
				log.Println("SQS: Local client listen timeout at", srv.config.Listen)
				continue
			}
			// Connection was closed
			return
		}

		if !srv.canSend() {
			respKO.WriteTo(netCon)
			return
		}

		req := lib.NewRequest(r, &srv.config)
		if req == nil {
			respBadCommand.WriteTo(netCon)
			continue
		}

		fastResponse, ok := commands[req.Command]
		if !ok {
			respBadCommand.WriteTo(netCon)
			continue
		}

		switch req.Command {
		case "RAWSET":
			if multi || len(req.Items) > 2 {
				respKO.WriteTo(netCon)
				continue
			}
			src, _ := req.Items[1].Bytes()
			srv.sendBytes(src)
		case "MULTI":
			multi = true
			row = &lib.InterRecord{}
		case "EXEC":
			multi = false
			srv.sendRecord(row)
		case "SET":
			k, _ := req.Items[1].Str()
			v, _ := req.Items[2].Str()
			if multi {
				row.Add(k, v)
			} else {
				row = &lib.InterRecord{}
				row.Add(k, v)
				srv.sendRecord(row)
			}
		case "SADD":
			k, _ := req.Items[1].Str()
			v, _ := req.Items[2].Str()
			if multi {
				row.Sadd(k, v)
			} else {
				row = &lib.InterRecord{}
				row.Sadd(k, v)
				srv.sendRecord(row)
			}
		case "HMSET":
			var key string
			var k string
			var v string

			if !multi {
				row = &lib.InterRecord{
					Types: 0,
				}
			}

			for i, o := range req.Items[1:] {
				if i == 0 {
					key, _ = o.Str()
					continue
				}

				// Now odd elements are the keys
				if i%2 != 0 {
					k, _ = o.Str()
				} else {
					v, _ = o.Str()
					row.Mhset(key, k, v)
				}
			}

			if !multi {
				srv.sendRecord(row)
			}

		}

		fastResponse.WriteTo(netCon)
		continue

	}
}
