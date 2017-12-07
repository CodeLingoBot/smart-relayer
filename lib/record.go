package lib

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"sync"
	"time"

	"github.com/gallir/bytebufferpool"
	gzip "github.com/klauspost/pgzip"
	"github.com/pquerna/ffjson/ffjson"
	"github.com/spaolacci/murmur3"
)

var compressPool = &bytebufferpool.Pool{}

// Pool for gzip
var gzipPool = sync.Pool{
	New: func() interface{} {
		return gzip.NewWriter(ioutil.Discard)
	},
}

func gzipGet(b io.Writer) *gzip.Writer {
	w := gzipPool.Get().(*gzip.Writer)
	// Reset the compressor linking with the new buffer, ready to use
	w.Reset(b)
	return w
}

func gzipPut(g *gzip.Writer) {
	g.Close()
	// Unlink the buffer for the gzip, just in case
	g.Reset(ioutil.Discard)
	gzipPool.Put(g)
}

// // Pool for InterRecord
// var interRecordPool = sync.Pool{
// 	New: func() interface{} {
// 		return NewInterRecord()
// 	},
// }

// func InterRecordGet() *InterRecord {
// 	return interRecordPool.Get().(*InterRecord)
// }

// func InterRecordPut(r *InterRecord) {
// 	interRecordPool.Put(r)
// }

type InterRecord struct {
	Types          int                    `json:"type,omitempty"`
	Ts             int64                  `json:"ts,number"`
	Data           map[string]interface{} `json:"data,omitempty"`
	Raw            []byte                 `json:"raw,omitempty"` // 0 json, 1 Raw bytes
	compressFields bool
}

// NewInterRecord create a InterRecord struct to the connection
func NewInterRecord() *InterRecord {
	return &InterRecord{
		Ts:   time.Now().UnixNano() / 1000000,
		Data: make(map[string]interface{}, 0),
	}
}

func (r *InterRecord) isBytes(value interface{}) {
	switch value.(type) {
	case []byte:
		r.compressFields = true
	}
}

func (r *InterRecord) Add(key string, value interface{}) {
	if _, ok := r.Data[key]; !ok {
		r.isBytes(value)
		r.Data[key] = value
	}
}

func (r *InterRecord) Sadd(key string, value interface{}) {
	if _, ok := r.Data[key]; !ok {
		r.Data[key] = make([]interface{}, 0)
	}

	r.isBytes(value)
	r.Data[key] = append(r.Data[key].([]interface{}), value)
}

func (r *InterRecord) Mhset(key, k string, v interface{}) {
	if _, ok := r.Data[key]; !ok {
		r.Data[key] = make(map[string]interface{})
	}

	r.isBytes(v)
	r.Data[key].(map[string]interface{})[k] = v
}

// Bytes return the record in bytes
func (r *InterRecord) Bytes() []byte {
	if r.Types != 0 {
		return r.Raw
	}

	// Collect the buffers to be recycled after generate the JSON
	var buffs []*bytebufferpool.ByteBuffer
	defer func() {
		if len(buffs) > 0 {
			for _, b := range buffs {
				compressPool.Put(b)
			}
		}
	}()

	if r.compressFields {
		for k, v := range r.Data {
			switch o := v.(type) {
			case []byte:

				// Get a buffer from the pool and append to the buffs to be recycled (check the defer some upper lines)
				b := compressPool.Get()
				buffs = append(buffs, b)

				// Get a gzip from the pull, write in the buffer and return to the pool
				// NOTE: the Reset and Close instruction is called in the gzipGet and gzipPut functions
				w := gzipGet(b)
				w.Write(o)
				gzipPut(w)

				r.Data[k] = b.B

			default:
				// No changes
			}
		}
	}

	buf, err := ffjson.Marshal(r)
	if err != nil {
		ffjson.Pool(buf)
		log.Printf("Error in ffjson: %s", err)
		return nil
	}

	return buf
}

// BytesUniqID return the record content in bytes and the uniq ID
func (r *InterRecord) BytesUniqID() ([]byte, string) {
	buf := r.Bytes()
	h := r.uniqID(buf)
	return buf, h
}

// String return the record in string format
func (r *InterRecord) String() string {
	buf := r.Bytes()
	defer ffjson.Pool(buf)

	return string(buf)
}

// StringUniqID build a uniq ID based on the content in string format
func (r *InterRecord) StringUniqID() (string, string) {
	buf := r.Bytes()
	defer ffjson.Pool(buf)

	h := r.uniqID(buf)
	return string(buf), h
}

// Len return the bytes used in Raw or the len of the map data
func (r *InterRecord) Len() int {
	if r.Types != 0 {
		return len(r.Raw)
	}

	return len(r.Bytes())
}

func (r *InterRecord) uniqID(b []byte) string {
	h := murmur3.New64()
	h.Write(b)
	return fmt.Sprintf("%d%0X", time.Now().UnixNano(), h.Sum64())
}
