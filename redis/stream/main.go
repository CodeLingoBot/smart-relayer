package stream

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gallir/radix.improved/redis"
	"github.com/gallir/smart-relayer/lib"
	gzip "github.com/klauspost/pgzip"
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
}

var (
	sep = []byte("\t")
)

var (
	errBadCmd              = errors.New("ERR bad command")
	errKO                  = errors.New("fatal error")
	errOverloaded          = errors.New("Redis overloaded")
	errStreamSet           = errors.New("STSET [timestamp] [key] [value]")
	errStreamGet           = errors.New("STGET [timestamp] [key]")
	errStreamToBackend     = errors.New("ST2BACK [path]")
	errChanFull            = errors.New("The file can't be created")
	respOK                 = redis.NewRespSimple("OK")
	respTrue               = redis.NewResp(1)
	respBadCommand         = redis.NewResp(errBadCmd)
	respKO                 = redis.NewResp(errKO)
	respBadStreamSet       = redis.NewResp(errStreamSet)
	respBadStreamGet       = redis.NewResp(errStreamGet)
	respBadStreamToBackend = redis.NewResp(errStreamToBackend)
	respChanFull           = redis.NewResp(errChanFull)
	commands               map[string]*redis.Resp

	defaultMaxWriters   = 100
	defaultBuffer       = 1024
	defaultPath         = "/tmp"
	defaultLimitRecords = 9999
)

func init() {
	commands = map[string]*redis.Resp{
		"PING":    respOK,
		"STSET":   respOK,
		"STGET":   respOK,
		"ST2BACK": respOK,
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
		case "STSET":
			// This command require 4 elements
			if len(req.Items) != 4 {
				respBadStreamSet.WriteTo(netCon)
				continue
			}

			// Get message from the pool. This message will be return after write
			// in a file in one of the writers routines (writers.go). In case of error
			// will be returned to the pull immediately
			msg := getMsg(&srv.config.Path)
			// Read the key string and store in the Msg
			if err := msg.parse(req.Items); err != nil {
				// Response error to the client
				respBadStreamSet.WriteTo(netCon)
				// Return message to the pool just in errors
				putMsg(msg)
				continue
			}

			// Read bytes from the client and store in the message buffer (Msg.b)
			b, _ := req.Items[3].Bytes()
			msg.b.Write(b)

			select {
			case srv.C <- msg:
				fastResponse.WriteTo(netCon)
			default:
				respChanFull.WriteTo(netCon)
			}

		case "STGET":
			// This command require 3 items
			if len(req.Items) != 3 {
				respBadStreamGet.WriteTo(netCon)
				continue
			}

			// Get message from the pool.
			msg := getMsg(&srv.config.Path)
			// Read the key string and store in the Msg
			if err := msg.parse(req.Items); err != nil {
				// Response error to the client
				respBadStreamGet.WriteTo(netCon)
				// Return message to the pool
				putMsg(msg)
				continue
			}

			if b, err := msg.Bytes(); err != nil {
				redis.NewResp(err).WriteTo(netCon)
			} else {
				redis.NewResp(b).WriteTo(netCon)
			}

			putMsg(msg)

		case "ST2BACK":
			// This command require 3 items
			if len(req.Items) != 2 {
				respBadStreamToBackend.WriteTo(netCon)
				continue
			}

			path, _ := req.Items[1].Str()

			files, err := ioutil.ReadDir(path)
			if err != nil {
				redis.NewResp(err).WriteTo(netCon)
				continue
			}

			sw, err := NewSplitWriter()
			if err != nil {
				redis.NewResp(err).WriteTo(netCon)
				continue
			}

			for _, file := range files {

				// Split the timestamp and the index name
				sf := strings.SplitN(file.Name(), "-", 2)

				lf, err := os.Open(fmt.Sprintf("%s/%s", path, file.Name()))
				if err != nil {
					log.Printf("STREAM: Can't read the file %s: %s", file.Name(), err)
					continue
				}

				sw.Write([]byte(sf[0]))
				sw.Write(sep)
				sw.Write([]byte(sf[1][:len(sf[1])-len(ext)-1]))
				sw.Write(sep)

				encoder := base64.NewEncoder(base64.StdEncoding, sw)
				io.Copy(encoder, lf)
				encoder.Close()

				lf.Close()

				sw.Write(newLine)

				sw.counter()
			}

			sw.Close()

			fastResponse.WriteTo(netCon)

		default:
			log.Panicf("Invalid command: This never should happen, check the cases or the list of valid command")

		}

	}
}

func NewSplitWriter() (*SplitWriter, error) {
	s := &SplitWriter{}
	if err := s.start(); err != nil {
		return nil, err
	}

	return s, nil
}

type SplitWriter struct {
	tmp   *os.File
	gz    *gzip.Writer
	count int
}

func (s *SplitWriter) start() (err error) {

	s.count = 0

	s.tmp, err = ioutil.TempFile("", "example")
	if err != nil {
		return
	}

	s.gz = lib.GzipPool.Get().(*gzip.Writer)
	s.gz.Reset(s.tmp)

	return
}

func (s *SplitWriter) counter() {
	if s.count < defaultLimitRecords {
		s.count++
		return
	}

	s.Close()
	s.start()
}

func (s *SplitWriter) Write(b []byte) (int, error) {
	return s.gz.Write(b)
}

func (s *SplitWriter) Close() (err error) {

	if err = s.gz.Close(); err != nil {
		return
	}

	if err = s.tmp.Close(); err != nil {
		return
	}

	s.gz.Reset(ioutil.Discard)

	lib.GzipPool.Put(s.gz)

	return
}
