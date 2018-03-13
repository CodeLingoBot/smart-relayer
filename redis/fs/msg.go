package fs

import (
	"fmt"
	"hash/crc32"
	"io"
	"io/ioutil"
	"os"
	"sync"
	"time"

	"github.com/gallir/bytebufferpool"
	"github.com/gallir/smart-relayer/lib"
	"github.com/gallir/smart-relayer/redis/fs/ifaceS3"
)

var (
	msgBytesPool = &bytebufferpool.Pool{}
	msgPool      = &sync.Pool{
		New: func() interface{} {
			return &Msg{}
		},
	}
)

var (
	extPlain = "log"
	extGz    = "log.gz"
)

func getMsg(srv *Server) *Msg {
	m := msgPool.Get().(*Msg)
	m.b = msgBytesPool.Get()
	m.shard = -1
	m.srv = srv
	m.disableShards = false
	return m
}

func putMsg(m *Msg) {

	msgBytesPool.Put(m.b)
	m.b = nil

	m.k = ""
	m.project = ""
	m.t = time.Now()

	msgPool.Put(m)
}

type Msg struct {
	project       string
	k             string
	t             time.Time
	b             *bytebufferpool.ByteBuffer
	shard         int
	srv           *Server
	disableShards bool
}

//
// The next lines have the functions to build the
// path and hourpath (relative) and the fullpath (absolute to filesystem)
//

// fullpath return the full path from the / directory in the minute of the file
func (m *Msg) fullpath() string {
	return fmt.Sprintf("%s/%s", m.srv.config.Path, m.path())
}

// hourpath return the full path as string until the "hour"
func (m *Msg) hourpath() string {
	return fmt.Sprintf("%s/%d/%.2d/%.2d/%.2d", m.project, m.t.UTC().Year(), m.t.UTC().Month(), m.t.UTC().Day(), m.t.UTC().Hour())
}

// path resolve the full path to read/write the file
// Here we resolve the shard based in the "key" (m.k) of the message
// based on crc32 algoritm and expresed as hexdecimal
func (m *Msg) path() string {
	if m.srv.shards == 0 || m.disableShards {
		// If shard is disabled
		return fmt.Sprintf("%s/%.2d", m.hourpath(), m.t.UTC().Minute())
	}

	return fmt.Sprintf("%s/%.2d/%02x", m.hourpath(), m.t.UTC().Minute(), m.getShard())
}

func (m *Msg) getShard() int {
	if m.shard >= 0 {
		return m.shard
	}

	if m.srv.shards == 0 || m.disableShards {
		return m.shard
	}

	h := crc32.ChecksumIEEE([]byte(m.k)) % m.srv.shards
	m.shard = int(h)
	return m.shard
}

func (m *Msg) filename() string {
	if m.srv.config.Compress {
		return m.filenameGz()
	}
	return m.filenamePlain()
}

func (m *Msg) filenameGz() string {
	return fmt.Sprintf("%s.%s", m.k, extGz)
}

func (m *Msg) filenamePlain() string {
	return fmt.Sprintf("%s.%s", m.k, extPlain)
}

func (m *Msg) Bytes() (b []byte, err error) {

	if m.srv.config.Compress {
		lib.Debugf("FS Read local file: %s/%s - %s", m.path(), m.filenameGz(), m.t.UTC())
		b, err = m.bytesFile(fmt.Sprintf("%s/%s", m.fullpath(), m.filenameGz()), true)
		if err == nil {
			return
		}
		// If the file is empty, that means the file exists so we return here
		if err == io.EOF {
			return
		}
	}

	lib.Debugf("FS Read local file: %s/%s - %s", m.path(), m.filenamePlain(), m.t.UTC())
	b, err = m.bytesFile(fmt.Sprintf("%s/%s", m.fullpath(), m.filenamePlain()), false)
	if err == nil {
		return
	}

	// check if the file exists disabling the shards just for this message
	if m.srv.shards > 0 && !m.disableShards {
		m.disableShards = true
		return m.Bytes()
	}

	lib.Debugf("FS Read S3: %s/%s - %s", m.path(), m.filenamePlain(), m.t.UTC())
	b, err = m.bytesS3()
	return
}

func (m *Msg) bytesFile(filename string, gz bool) ([]byte, error) {
	file, err := os.Open(filename)
	if err != nil {
		if err == io.EOF {
			file.Close()
		}
		return nil, err
	}
	defer file.Close()

	if !gz {
		return ioutil.ReadAll(file)
	}

	// Check if is possible to read the metadata of the file
	fi, err := file.Stat()
	if err != nil {
		return nil, err
	}

	// If the file is empty we return as EOF because can cause issues
	// to the gzipReader. Something to check with more detail...
	if fi.Size() == 0 {
		return nil, io.EOF
	}

	zr, err := lib.GetGzipReader(file)
	defer lib.PutGzipReader(zr)
	if err != nil {
		return nil, err
	}

	defer zr.Close()

	return ioutil.ReadAll(zr)
}

func (m *Msg) bytesS3() ([]byte, error) {
	r := ifaceS3.NewReaderUncompress(m.srv.s3sess, m.srv.config.S3Bucket)
	return r.Get(m.k, m.hourpath(), m.t)
}
