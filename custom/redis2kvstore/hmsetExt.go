package redis2kvstore

import (
	"sync"

	"github.com/gallir/smart-relayer/redis/radix.improved/redis"
)

var (
	hmsetPool sync.Pool
	fieldPool sync.Pool
)

func getPoolHMSet() *Hmset {
	m := hmsetPool.Get()
	if m == nil {
		return &Hmset{}
	}
	h := m.(*Hmset)
	h.Reset()
	return h
}

func putPoolHMSet(m *Hmset) {
	for _, f := range m.Fields {
		putPoolField(f)
	}
	m.Reset()
	hmsetPool.Put(m)
}

func getPoolField() *Field {
	m := fieldPool.Get()
	if m == nil {
		return &Field{}
	}
	m.(*Field).Reset()
	return m.(*Field)
}

func putPoolField(m *Field) {
	fieldPool.Put(m)
}

func (h *Hmset) processItems(items []*redis.Resp) {
	for i := 0; i < len(items); i++ {
		f := getPoolField()

		s, _ := items[i].Str()
		f.Name = s

		i++
		b, _ := items[i].Bytes()
		f.Value = append(f.Value[:0], b...)
		h.Fields = append(h.Fields, f)
	}
}

func (h *Hmset) getAllAsRedis() (*redis.Resp, error) {
	t := make(map[string][]byte, 0)
	for _, f := range h.Fields {
		t[f.Name] = append(t[f.Name][:0], f.Value...)
	}

	r := redis.NewResp(t)
	if r == nil {
		return nil, errBadCmd
	}
	return r, nil
}

func (h *Hmset) getOneAsRedis(field string) (*redis.Resp, error) {
	for _, f := range h.Fields {
		if f.Name == field {
			return redis.NewResp(append([]byte{}, f.Value...)), nil
		}
	}
	return nil, errNotFound
}

func (h *Hmset) clone() *Hmset {
	nh := getPoolHMSet()
	nh.Key = h.Key
	nh.Sent = h.Sent

	for _, f := range h.Fields {
		nf := getPoolField()
		nf.Name = f.Name
		nf.Value = append(nf.Value[:0], f.Value...)
		nh.Fields = append(nh.Fields, nf)
	}
	return nh
}
