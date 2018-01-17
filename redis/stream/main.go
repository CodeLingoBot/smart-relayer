package stream

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"

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
	listener net.Listener
	C        chan *Msg

	writers chan *writer

	lastError time.Time
	errors    int64

	s3sess *session.Session
}

var (
	sep     = []byte("\t")
	newLine = []byte("\n")
)

var (
	errBadCmd      = errors.New("ERR bad command")
	errKO          = errors.New("fatal error")
	errSet         = errors.New("ERR - syntax: SET project key [timestamp] value")
	errGet         = errors.New("ERR - syntax: GET project key [timestamp]")
	errChanFull    = errors.New("ERR - The file can't be created")
	errNotFound    = errors.New("KO - Key not found")
	respOK         = redis.NewRespSimple("OK")
	respTrue       = redis.NewResp(1)
	respBadCommand = redis.NewResp(errBadCmd)
	respKO         = redis.NewResp(errKO)
	respBadSet     = redis.NewResp(errSet)
	respBadGet     = redis.NewResp(errGet)
	respChanFull   = redis.NewResp(errChanFull)
	respNotFound   = redis.NewResp(errNotFound)
	commands       map[string]*redis.Resp

	defaultMaxWriters = 100
	defaultBuffer     = 1024
	defaultPath       = "/tmp"
)

func init() {
	commands = map[string]*redis.Resp{
		"PING": respOK,
		"SET":  respOK,
		"GET":  respOK,
	}
}

// New creates a new Redis local server
func New(c lib.RelayerConfig, done chan bool) (*Server, error) {
	srv := &Server{
		done:    done,
		errors:  0,
		writers: make(chan *writer, defaultMaxWriters),
	}

	srv.Reload(&c)

	return srv, nil
}

// Reload the configuration
func (srv *Server) Reload(c *lib.RelayerConfig) (err error) {
	srv.Lock()
	defer srv.Unlock()

	srv.config = *c
	if srv.config.Buffer == 0 {
		srv.config.Buffer = defaultBuffer
	}

	if srv.config.MaxConnections == 0 {
		srv.config.MaxConnections = defaultMaxWriters
	}

	if srv.config.Path == "" {
		srv.config.Path = defaultPath
	}

	if srv.C == nil {
		srv.C = make(chan *Msg, srv.config.Buffer)
	}

	if sess, err := session.NewSessionWithOptions(session.Options{
		Profile: srv.config.Profile,
		Config: aws.Config{
			Region: &srv.config.Region,
		},
	}); err == nil {
		srv.s3sess = sess
	} else {
		log.Printf("ERROR: invalid S3 session: %s", err)
		srv.s3sess = nil
	}

	lw := len(srv.writers)

	if lw == srv.config.MaxConnections {
		return nil
	}

	if lw > srv.config.MaxConnections {
		for i := srv.config.MaxConnections; i < lw; i++ {
			w := <-srv.writers
			w.exit()
		}
		return nil
	}

	if lw < srv.config.MaxConnections {
		for i := lw; i < srv.config.MaxConnections; i++ {
			srv.writers <- newWriter(srv)
		}
		return nil
	}

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
					log.Println("File ERROR: timeout at local listener", srv.config.ListenHost(), e)
					continue
				}
				if srv.exiting {
					log.Println("File: exiting local listener", srv.config.ListenHost())
					return
				}
				log.Fatalln("File ERROR: emergency error in local listener", srv.config.ListenHost(), e)
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

	// Close the channel were we store the active writers
	close(srv.writers)
	for w := range srv.writers {
		go w.exit()
	}

	// Close the main channel, all writers will finish
	close(srv.C)

	// finishing the server
	srv.done <- true
}

func (srv *Server) handleConnection(netCon net.Conn) {

	defer netCon.Close()

	reader := redis.NewRespReader(netCon)

	for {

		r := reader.Read()

		if r.IsType(redis.IOErr) {
			if redis.IsTimeout(r) {
				// Paranoid, don't close it just log it
				log.Println("File: Local client listen timeout at", srv.config.Listen)
				continue
			}
			// Connection was closed
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
		case "PING":
			fastResponse.WriteTo(netCon)
		case "SET":
			// SET project key [timestamp] value
			if len(req.Items) <= 3 || len(req.Items) > 5 {
				respBadSet.WriteTo(netCon)
				continue
			}

			if err := srv.set(netCon, req.Items); err != nil {
				switch err {
				case err.(*os.PathError):
					respNotFound.WriteTo(netCon)
				default:
					redis.NewResp(err).WriteTo(netCon)
				}
			}

		case "GET":
			// GET project key [timestamp]
			if len(req.Items) <= 2 || len(req.Items) > 4 {
				respBadGet.WriteTo(netCon)
				continue
			}

			if err := srv.get(netCon, req.Items); err != nil {
				switch err {
				case err.(*os.PathError):
					respNotFound.WriteTo(netCon)
				default:
					redis.NewResp(err).WriteTo(netCon)
				}
			}

		default:
			log.Panicf("Invalid command: This never should happen, check the cases or the list of valid command")

		}

	}
}

func (srv *Server) fullpath(project string, t time.Time) string {
	return fmt.Sprintf("%s/%s/%d/%.2d/%.2d/%.2d/%.2d", srv.config.Path, project, t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute())
}

func (srv *Server) path(project string, t time.Time) string {
	return fmt.Sprintf("%s/%d/%.2d/%.2d/%.2d/%.2d", project, t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute())
}

func (srv *Server) set(netCon net.Conn, items []*redis.Resp) (err error) {

	// Get a message struct from the pool
	msg := getMsg(srv)

	msg.project, err = items[1].Str()
	if err != nil {
		return err
	}

	msg.k, err = items[2].Str()
	if err != nil {
		return err
	}

	if len(items) == 4 {
		if b, err := items[3].Bytes(); err == nil {
			msg.b.Write(b)
		} else {
			return err
		}

		// Current time
		msg.t = time.Now()
	} else {
		if i, err := items[3].Int64(); err == nil {
			msg.t = time.Unix(i, 0)
		} else {
			return err
		}

		if b, err := items[4].Bytes(); err == nil {
			msg.b.Write(b)
		} else {
			return err
		}
	}

	r := fmt.Sprintf("%s/%s", msg.fullpath(), msg.filename())

	select {
	case srv.C <- msg:
		redis.NewResp(r).WriteTo(netCon)
		return nil
	default:
	}

	return errors.New("Channel full")
}

func (srv *Server) get(netCon net.Conn, items []*redis.Resp) (err error) {
	msg := getMsg(srv)
	defer putMsg(msg)

	msg.project, err = items[1].Str()
	if err != nil {
		return err
	}

	msg.k, err = items[2].Str()
	if err != nil {
		return err
	}

	if len(items) > 3 {
		if i, err := items[3].Int64(); err == nil {
			msg.t = time.Unix(i, 0)
		} else {
			return err
		}
	}

	var b []byte
	b, err = msg.Bytes()
	if err != nil {
		return err
	}

	redis.NewResp(b).WriteTo(netCon)
	return nil
}
