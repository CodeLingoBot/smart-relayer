package redis

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/gallir/smart-relayer/lib"
)

var noDeadline = time.Time{}

// Conn keeps the status of the connection to a server
type Conn struct {
	NetConn net.Conn
	// Parser  func(*Conn) ([]byte, error)
	Rd          *bufio.Reader
	Buf         []byte
	ReadTimeout time.Duration
	bufCount    int

	Inited bool
	UsedAt time.Time

	Database int
}

const (
	maxBufCount = 1000 // To protect for very large buffer consuming lot of memory
)

func NewConn(netConn net.Conn, readTimeout time.Duration) *Conn {
	cn := &Conn{
		NetConn:     netConn,
		UsedAt:      time.Now(),
		ReadTimeout: readTimeout, // We use different read timeouts for the server and local client
	}
	cn.Rd = bufio.NewReader(cn)
	return cn
}

func (cn *Conn) isStale(timeout time.Duration) bool {
	return timeout > 0 && time.Since(cn.UsedAt) > timeout
}

// Read complies with io.Reader interface
func (cn *Conn) Read(b []byte) (int, error) {
	cn.UsedAt = time.Now()
	if cn.ReadTimeout != 0 {
		cn.NetConn.SetReadDeadline(cn.UsedAt.Add(cn.ReadTimeout))
	} else {
		cn.NetConn.SetReadDeadline(noDeadline)
	}
	return cn.NetConn.Read(b)
}

// Write complies with io.Writer interface
func (cn *Conn) Write(b []byte) (int, error) {
	cn.UsedAt = time.Now()
	if writeTimeout != 0 {
		cn.NetConn.SetWriteDeadline(cn.UsedAt.Add(writeTimeout))
	} else {
		cn.NetConn.SetWriteDeadline(noDeadline)
	}
	return cn.NetConn.Write(b)
}

func (cn *Conn) remoteAddr() net.Addr {
	return cn.NetConn.RemoteAddr()
}

func (cn *Conn) readLine() ([]byte, error) {
	line, err := cn.Rd.ReadBytes('\n')
	if err == nil {
		return line, nil
	}
	return nil, err
}

func (cn *Conn) readN(n int) ([]byte, error) {
	if cn.bufCount > maxBufCount || cap(cn.Buf) < n {
		cn.Buf = make([]byte, n)
		cn.bufCount = 0
	} else {
		cn.Buf = cn.Buf[:n]
		cn.bufCount++
	}
	_, err := io.ReadFull(cn.Rd, cn.Buf)
	return cn.Buf, err
}

func (cn *Conn) close() error {
	return cn.NetConn.Close()
}

func (cn *Conn) parse(r *Request, parseCommand bool) ([]byte, error) {
	line, err := cn.readLine()
	if err != nil {
		return nil, err
	}
	if len(line) == 0 {
		lib.Debugf("Empty line")
		return nil, malformed("short response line", string(line))
	}

	if r.Buffer == nil {
		r.Buffer = new(bytes.Buffer)
	}

	switch line[0] {
	case '+', '-', ':':
		r.Buffer.Write(line)
		return line, nil
	case '$':
		n, err := strconv.Atoi(string(line[1 : len(line)-2]))
		if err != nil {
			return nil, err
		}
		r.Buffer.Write(line)
		if n > 0 {
			b, err := cn.readN(n + 2)
			if err != nil {
				return nil, err
			}
			// Now check for trailing CR
			if b[len(b)-2] != '\r' || b[len(b)-1] != '\n' {
				return nil, malformedMissingCRLF()
			}
			if parseCommand {
				if len(r.Command) == 0 {
					r.Command = bytes.ToUpper(b[:len(b)-2])
				} else {
					if bytes.Compare(r.Command, selectCommand) == 0 {
						n, err = strconv.Atoi(string(b[0 : len(b)-2]))
						if err == nil {
							cn.Database = n
						}
					}
				}
			}
			r.Buffer.Write(b)
		}
		return r.Command, nil
	case '*':
		n, err := strconv.Atoi(string(line[1 : len(line)-2]))
		if n < 0 || err != nil {
			return nil, err
		}
		r.Buffer.Write(line)
		for i := 0; i < n; i++ {
			_, err := cn.parse(r, parseCommand)
			if err != nil {
				return nil, malformed("*<numberOfArguments>", string(line))
			}
		}
		return r.Command, nil
	default:
		// Inline request
		r.Buffer.Write(line)
		parts := bytes.Split(line, []byte(" "))
		if len(parts) > 0 {
			r.Command = bytes.ToUpper(bytes.TrimSpace(parts[0]))
		}
		return line, nil
	}
}

func malformed(expected string, got string) error {
	lib.Debugf("Mailformed request:'%s does not match %s\\r\\n'", got, expected)
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
