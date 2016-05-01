package redis

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"time"

	"github.com/gallir/go-bulk-relayer/tools"
)

var noDeadline = time.Time{}

type Conn struct {
	NetConn net.Conn
	// Parser  func(*Conn) ([]byte, error)
	Rd  *bufio.Reader
	Buf []byte

	Inited bool
	UsedAt time.Time

	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	Database int
}

func NewConn(netConn net.Conn) *Conn {
	cn := &Conn{
		NetConn: netConn,
		UsedAt:  time.Now(),
	}
	cn.Rd = bufio.NewReader(cn)
	return cn
}

func (cn *Conn) IsStale(timeout time.Duration) bool {
	return timeout > 0 && time.Since(cn.UsedAt) > timeout
}

func (cn *Conn) Read(b []byte) (int, error) {
	cn.UsedAt = time.Now()
	if cn.ReadTimeout != 0 {
		cn.NetConn.SetReadDeadline(cn.UsedAt.Add(cn.ReadTimeout))
	} else {
		cn.NetConn.SetReadDeadline(noDeadline)
	}
	return cn.NetConn.Read(b)
}

func (cn *Conn) Write(b []byte) (int, error) {
	cn.UsedAt = time.Now()
	if cn.WriteTimeout != 0 {
		cn.NetConn.SetWriteDeadline(cn.UsedAt.Add(cn.WriteTimeout))
	} else {
		cn.NetConn.SetWriteDeadline(noDeadline)
	}
	return cn.NetConn.Write(b)
}

func (cn *Conn) RemoteAddr() net.Addr {
	return cn.NetConn.RemoteAddr()
}

func (cn *Conn) ReadLine() ([]byte, error) {
	line, err := cn.Rd.ReadString('\n')
	if err == nil {
		return []byte(line), nil
	}
	return nil, err
}

func (cn *Conn) ReadN(n int) ([]byte, error) {
	if d := n - cap(cn.Buf); d > 0 {
		cn.Buf = cn.Buf[:cap(cn.Buf)]
		cn.Buf = append(cn.Buf, make([]byte, d)...)
	} else {
		cn.Buf = cn.Buf[:n]
	}
	_, err := io.ReadFull(cn.Rd, cn.Buf)
	return cn.Buf, err
}

func (cn *Conn) Close() error {
	return cn.NetConn.Close()
}

func (cn *Conn) Parse(r *Request, parseCommand bool) ([]byte, error) {
	line, err := cn.ReadLine()
	if err != nil {
		return nil, err
	}
	if len(line) == 0 {
		return nil, malformed("short response line", string(line))
	}

	switch line[0] {
	case '+', '-', ':':
		r.Bytes = line
		return line, nil
	case '$':
		n, err := strconv.Atoi(string(line[1 : len(line)-2]))
		if err != nil {
			return nil, err
		}
		r.Bytes = append(r.Bytes, line...)
		if n > 0 {
			b, err := cn.ReadN(n + 2)
			if err != nil {
				return nil, err
			}
			// Now check for trailing CR
			if b[len(b)-2] != '\r' || b[len(b)-1] != '\n' {
				return nil, malformedMissingCRLF()
			}
			if parseCommand {
				if r.Command == "" {
					r.Command = string(b[:len(b)-2])
				} else {
					if r.Command == "SELECT" {
						n, err = strconv.Atoi(string(b[0 : len(b)-2]))
						if err == nil {
							cn.Database = n
						}
					}
				}
			}
			r.Bytes = append(r.Bytes, b...)
		}
		return r.Bytes, nil
	case '*':
		n, err := strconv.Atoi(string(line[1 : len(line)-2]))
		if n < 0 || err != nil {
			return nil, err
		}
		r.Bytes = append(r.Bytes, line...)
		for i := 0; i < n; i++ {
			_, err := cn.Parse(r, parseCommand)
			if err != nil {
				return nil, malformed("*<numberOfArguments>", string(line))
			}
		}
		return r.Bytes, nil
	default:
		if len(line) > 0 {
			return line, nil
		}
	}
	log.Println("Empty line", string(line))
	return nil, malformed("Empty line", string(line))

}

func malformed(expected string, got string) error {
	tools.Debugf("Mailformed request:'%s does not match %s\\r\\n'", got, expected)
	return fmt.Errorf("Mailformed request:'%s does not match %s\\r\\n'", got, expected)
}

func malformedLength(expected int, got int) error {
	return fmt.Errorf(
		"Mailformed request: argument length '%d does not match %d\\r\\n'",
		got, expected)
}

func malformedMissingCRLF() error {
	return fmt.Errorf("Mailformed request: line should end with \\r\\n")
}
