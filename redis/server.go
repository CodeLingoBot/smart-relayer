package redis

import (
	"fmt"
	"io"
	"net"

	"github.com/gallir/go-bulk-relayer"
)

type Request struct {
	Name  string
	Bytes []byte
	Host  string
	Body  io.ReadCloser
}

type Server struct {
	Proto string
	Addr  string // TCP address to listen on, ":6389" if empty
}

func (srv *Server) ListenAndServe() error {
	addr := srv.Addr
	if srv.Proto == "" {
		srv.Proto = "tcp"
	}
	if srv.Proto == "unix" && addr == "" {
		addr = "/tmp/redis.sock"
	} else if addr == "" {
		addr = ":6389"
	}
	l, e := net.Listen(srv.Proto, addr)
	if e != nil {
		return e
	}
	return srv.Serve(l)
}

// Serve accepts incoming connections on the Listener l
func (srv *Server) Serve(l net.Listener) error {
	defer l.Close()
	//clt, _ := NewClient()
	//clt.Connect()
	//go clt.Listen()
	for {
		netConn, err := l.Accept()
		conn := relayer.NewConn(netConn, parser)
		if err != nil {
			return err
		}
		go srv.ServeClient(conn)
	}
}

// Serve starts a new redis session, using `conn` as a transport.
func (srv *Server) ServeClient(conn *relayer.Conn) (err error) {
	defer func() {
		if err != nil {
			fmt.Fprintf(conn, "-%s\n", err)
		}
		conn.Close()
	}()

	fmt.Println("New connection from", conn.RemoteAddr())

	for {
		_, err := conn.Receive()
		if err != nil {
			return err
		}
		// fmt.Printf("Request %q\n", request)
		conn.Write([]byte("+OK\r\n"))
		/*
			var reply string
			relay := false
			switch request.Name {
			case
				"set",
				"hset",
				"del",
				"decr",
				"decrby",
				"expire",
				"expireat",
				"flushall",
				"flushdb",
				"geoadd",
				"hdel",
				"setbit",
				"setex",
				"setnx",
				"smove":
				reply = "+OK\r\n"
				relay = true
			case "ping":
				reply = "+PONG\r\n"
			default:
				reply = "-Error: Command not accepted\r\n"
				log.Printf("Error: command not accepted %q\n", request.Name)

			}

			//		request.Host = clientAddr

			if _, err = conn.Write([]byte(reply)); err != nil {
				return err
			}
			if relay {
				//			clt.Channel <- request
			}
		*/
	}
}

func NewServer(c *Config) (*Server, error) {
	srv := &Server{
		Proto: c.proto,
	}

	if srv.Proto == "unix" {
		srv.Addr = c.host
	} else {
		srv.Addr = fmt.Sprintf("%s:%d", c.host, c.port)
	}

	return srv, nil
}
