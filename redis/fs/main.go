package fs

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

	shardsWriters int
	shardServer   *ShardsServer
	shards        uint32

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
	errClosing     = errors.New("ERR - Daemon closing")
	respOK         = redis.NewRespSimple("OK")
	respTrue       = redis.NewResp(1)
	respBadCommand = redis.NewResp(errBadCmd)
	respKO         = redis.NewResp(errKO)
	respBadSet     = redis.NewResp(errSet)
	respBadGet     = redis.NewResp(errGet)
	respChanFull   = redis.NewResp(errChanFull)
	respNotFound   = redis.NewResp(errNotFound)
	respClosing    = redis.NewResp(errClosing)
	commands       map[string]*redis.Resp

	defaultWritersByShard        = 16
	defaultShards                = 64
	defaultMinWriters            = 2
	defaultMaxWriters            = 256
	defaultInterval              = 500 * time.Millisecond
	defaultWriterCooldown        = 15 * time.Second
	defaultWriterThresholdWarmUp = 50.0 // percent
	defaultBuffer                = 20000
	defaultPath                  = "/tmp"
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
		done:   done,
		errors: 0,
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

	// If Shards is 0 we manage it as undefined so we apply the default value
	if srv.config.Shards == 0 {
		srv.config.Shards = defaultShards
	}
	// If the Shards are higher than 0 (could be by default or defined by the config file)
	// In case the shards are defined with a value less than 0 we will manage it as disabled
	// definding srv.shards as 0, look in function srv.path
	if srv.config.Shards > 0 {
		srv.shards = uint32(srv.config.Shards)
	}

	srv.shardsWriters = defaultWritersByShard

	// Shards
	if srv.shardServer == nil {
		srv.shardServer = NewShardsServer(srv, srv.config.Shards)
	} else {
		srv.shardServer.update(srv.config.Shards)
	}

	awsOpt := session.Options{
		Profile: srv.config.Profile,
		Config: aws.Config{
			Region: &srv.config.Region,
		},
	}

	if sess, err := session.NewSessionWithOptions(awsOpt); err == nil {
		srv.s3sess = sess
	} else {
		log.Printf("FS ERROR: invalid S3 session: %s", err)
		srv.s3sess = nil
	}

	log.Printf("FS %s config Buffer %d/%d Shard %d Writers by shard %d",
		srv.config.Listen, srv.shardServer.Len(), srv.config.Buffer, srv.shards, srv.shardsWriters)

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
					log.Println("FS ERROR: timeout at local listener", srv.config.ListenHost(), e)
					continue
				}
				if srv.exiting {
					log.Println("FS: exiting local listener", srv.config.ListenHost())
					return
				}
				log.Fatalln("FS ERROR: emergency error in local listener", srv.config.ListenHost(), e)
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

	srv.shardServer.Exit()

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
				log.Println("FS ERROR: Local client listen timeout at", srv.config.Listen)
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
				case errChanFull:
					log.Printf("FS ERROR SET: channel full")
					respChanFull.WriteTo(netCon)
				default:
					log.Printf("FS ERROR SET: %s", err)
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
					log.Printf("FS ERROR GET: %s", err)
					redis.NewResp(err).WriteTo(netCon)
				}
			}
		default:
			log.Panicf("FS ERROR: Invalid command: This never should happen, check the cases or the list of valid command")

		}

	}
}

func (srv *Server) set(netCon net.Conn, items []*redis.Resp) (err error) {

	// Get a message struct from the pool
	msg := getMsg(srv)

	// Find the project name
	msg.project, err = items[1].Str()
	if err != nil {
		return err
	}

	// Find the key name of the message
	msg.k, err = items[2].Str()
	if err != nil {
		return err
	}

	if len(items) == 4 {
		// If have 4 items means that the client sent the content without timestamp
		// in this case the message will have the current timestamp
		if b, err := items[3].Bytes(); err == nil {
			msg.b.Write(b)
		} else {
			return err
		}
		// Define the current timestamp
		msg.t = time.Now()
	} else {
		// If have more than 4 items we read timestamp
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

	// If the server is configured in sync mode we will try to write
	// the file directly and respons to the client with the result
	if srv.config.Mode == "sync" {
		defer putMsg(msg)

		w := writer{srv: srv}
		if err := w.writeTo(msg); err != nil {
			redis.NewResp(err).WriteTo(netCon)
			return err
		}
		redis.NewResp(r).WriteTo(netCon)
		return nil
	}

	// When the process have to close the chan srv.C can be closed
	// before arrive to this part of the code. For this reason we
	// check if the server is closing and respond an error message
	// to avoild a race condition trying to send a message to closed
	// channel
	if srv.exiting {
		respClosing.WriteTo(netCon)
		return nil
	}

	// Find a shard based on the message
	ss, err := srv.shardServer.get(msg.shard)
	if err != nil {
		return err
	}

	// Send message to the assigned shard
	select {
	case ss.C <- msg:
		redis.NewResp(r).WriteTo(netCon)
		return nil
	default:
		return errChanFull
	}
}

func (srv *Server) get(netCon net.Conn, items []*redis.Resp) (err error) {
	msg := getMsg(srv)
	defer putMsg(msg)

	// Find the project name
	msg.project, err = items[1].Str()
	if err != nil {
		return err
	}

	// Find the key name
	msg.k, err = items[2].Str()
	if err != nil {
		return err
	}

	// Verify if the 3th item is a int64 value to be converted in time
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
