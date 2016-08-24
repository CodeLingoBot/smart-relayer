package redis

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/gallir/smart-relayer/lib"
)

// Request stores the data for each client request
type Request struct {
	command         []byte
	buffer          *bytes.Buffer
	responseChannel chan []byte // Channel to send the response to the original client
	database        int         // The current database at the time the request was issued
}

// Server is the thread that listen for clients' connections
type Server struct {
	sync.Mutex
	config   lib.RelayerConfig
	pool     *pool
	mode     int
	done     chan bool
	listener net.Listener
}

const (
	connectionRetries = 3
	pipelineCommands  = 1000
	requestBufferSize = 1024
	modeSync          = 0
	modeSmart         = 1
	connectTimeout    = 5 * time.Second
	serverReadTimeout = 5 * time.Second
	writeTimeout      = 5 * time.Second
	maxIdle           = 10 * time.Second
	responseTimeout   = 20 * time.Second
	localReadTimeout  = 600 * time.Second
)

var (
	selectCommand          = []byte("SELECT")
	quitCommand            = []byte("QUIT")
	closeConnectionCommand = []byte("CLOSE")
	reloadCommand          = []byte("RELOAD")
	exitCommand            = []byte("EXIT")

	protoOK                    = []byte("+OK\r\n")
	protoTrue                  = []byte(":1\r\n")
	protoPing                  = []byte("PING\r\n")
	protoPong                  = []byte("+PONG\r\n")
	protoKO                    = []byte("-Error\r\n")
	protoClientCloseConnection = Request{command: closeConnectionCommand}
)

var commands map[string][]byte

func getSelect(n int) []byte {
	str := fmt.Sprintf("%d", n)
	return []byte(fmt.Sprintf("*2\r\n$6\r\n%s\r\n$%d\r\n%s\r\n", selectCommand, len(str), str))
}

func init() {
	// These are the commands that can be sent in "background" when in smart mode
	// The values are the immediate responses to the clients
	commands = map[string][]byte{
		"PING":   protoPong,
		"SET":    protoOK,
		"SETEX":  protoOK,
		"PSETEX": protoOK,
		"MSET":   protoOK,
		"HMSET":  protoOK,

		"SELECT": protoOK,

		"DEL":       protoTrue,
		"HSET":      protoTrue,
		"HDEL":      protoTrue,
		"EXPIRE":    protoTrue,
		"EXPIREAT":  protoTrue,
		"PEXPIRE":   protoTrue,
		"PEXPIREAT": protoTrue,
	}
}

// New creates a new Redis local server
func New(c lib.RelayerConfig, done chan bool) (*Server, error) {
	srv := &Server{
		done: done,
	}
	srv.pool = newPool(srv, &c)
	srv.Reload(&c)
	return srv, nil
}

func (srv *Server) Protocol() string {
	return "redis"
}

func (srv *Server) Listen() string {
	return srv.config.Listen
}

// Start accepts incoming connections on the Listener l
func (srv *Server) Start() (e error) {
	srv.Lock()
	defer srv.Unlock()

	connType := srv.config.ListenScheme()
	addr := srv.config.ListenHost()

	// Check that the socket does not exist
	if connType == "unix" {
		if s, err := os.Stat(addr); err == nil {
			if (s.Mode() & os.ModeSocket) > 0 {
				// Remove existing socket
				// log.Println("Warning, removing existing socket", addr)
				os.Remove(addr)
			} else {
				log.Println("socket", addr, s.Mode(), os.ModeSocket)
				log.Fatalf("Socket %s exists and it's not a Unix socket", addr)
			}
		}
	}

	srv.listener, e = net.Listen(connType, addr)
	if e != nil {
		log.Println("Error listening to", addr, e)
		return e
	}

	if connType == "unix" {
		// Make sure is accesible for everyone
		os.Chmod(addr, 0777)
	}

	log.Printf("Starting redis server at %s for target %s", addr, srv.config.Host())
	// Serve a client
	go func() {
		defer func() {
			srv.listener.Close()
			srv.done <- true
		}()

		for {
			netConn, e := srv.listener.Accept()
			if e != nil {
				log.Println("Exiting", addr)
				return
			}
			go srv.serveClient(netConn)
		}
	}()

	return nil
}

func (srv *Server) Reload(c *lib.RelayerConfig) {
	srv.Lock()
	defer srv.Unlock()

	reset := false
	if srv.config.Url != "" && srv.config.Url != c.Url {
		reset = true
	}
	srv.config = *c // Save a copy
	if c.Mode == "smart" {
		srv.mode = modeSmart
	} else {
		srv.mode = modeSync
	}
	if reset {
		log.Printf("Reload and reset redis server at port %s for target %s", srv.config.Listen, srv.config.Host())
		srv.pool.reset(c)
		srv.pool = newPool(srv, c)
	} else {
		log.Printf("Reload redis config at port %s for target %s", srv.config.Listen, srv.config.Host())
		srv.pool.readConfig(c)
	}
}

func (srv *Server) Config() *lib.RelayerConfig {
	return &srv.config
}

func (srv *Server) Exit() {
	srv.listener.Close()
}

func (srv *Server) serveClient(netConn net.Conn) {
	conn := lib.NewNetbuf(netConn, localReadTimeout, writeTimeout)
	defer netConn.Close()

	parser := newParser(conn)

	pooled, ok := srv.pool.get()
	if !ok {
		log.Println("Redis server, no clients available from pool")
		return
	}
	defer srv.pool.close(pooled)
	client := pooled.client
	responseCh := make(chan []byte, 1)
	defer close(responseCh)

	for {
		req := Request{}
		_, err := parser.read(&req, true)
		if err != nil {
			return
		}

		// QUIT received from client
		if bytes.Compare(req.command, quitCommand) == 0 {
			conn.Write(protoOK)
			return
		}

		req.database = parser.database

		// Smart mode, answer immediately and forget
		if srv.mode == modeSmart {
			fastResponse, ok := commands[string(req.command)]
			if ok {
				ok = sendRequest(client.requestChan, &req)
				if !ok {
					log.Printf("Error sending request to redis client, exiting")
					return
				}
				conn.Write(fastResponse)
				continue
			}
		}

		// Synchronized mode
		req.responseChannel = responseCh

		ok := sendRequest(client.requestChan, &req)
		if !ok {
			log.Printf("Error sending request to redis client, exiting")
			conn.Write(protoKO)
			return
		}

		select {
		case response, more := <-responseCh:
			if !more {
				// The client has closed the channel
				lib.Debugf("Redis client has closed channel, exiting")
				return
			}
			conn.Write(response)
		case <-time.After(responseTimeout):
			// Something very wrong happened in the client
			log.Println("Timeout waiting a response, closing client connection")
			conn.Write(protoKO)
			return
		}
	}
}

func sendRequest(c chan *Request, r *Request) (ok bool) {
	defer func() {
		e := recover() // To avoid panic due to closed channels
		if e != nil {
			log.Printf("sendRequest: Recovered from error %s, channel length %d", e, len(c))
			ok = false
		}
	}()

	if c == nil {
		lib.Debugf("Nil channel in send request")
		return false
	}

	c <- r
	return true
}

func sendAsyncRequest(c chan *Request, r *Request) (ok bool) {
	defer func() {
		e := recover() // To avoid panic due to closed channels
		if e != nil {
			log.Printf("sendAsyncRequest: Recovered from error %s, channel length %d", e, len(c))
			ok = false
		}
	}()

	if c == nil {
		return
	}

	select {
	case c <- r:
		ok = true
	default:
		lib.Debugf("Error sending request, channel length %d", len(c))
		ok = false
	}
	return
}

func sendAsyncResponse(c chan []byte, b []byte) (ok bool) {
	defer func() {
		e := recover() // To avoid panic due to closed channels
		if e != nil {
			log.Printf("sendAssendAsyncResponseyncRequest: Recovered from error %s, channel length %d", e, len(c))
			ok = false
		}
	}()

	if c == nil {
		return
	}

	select {
	case c <- b:
		ok = true
	default:
		lib.Debugf("Error sending response %s channel length %d", string(b), len(c))
		ok = false
	}
	return
}
