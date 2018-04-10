package fs

import (
	"sync"

	"github.com/gallir/smart-relayer/lib"
)

type shard struct {
	srv *Server
	C   chan *Msg
	w   chan *writer
}

func newShard(srv *Server) *shard {
	s := &shard{
		srv: srv,
		C:   make(chan *Msg, srv.config.Buffer),
		w:   make(chan *writer, defaultShardLimitWriters),
	}
	s.reload()
	return s
}

func (s *shard) reload() {
	l := len(s.w)
	n := s.srv.config.Writers

	defer func() {
		lib.Debugf("Current writers in the shard %d", len(s.w))
	}()

	if l == n {
		return
	}

	if l < n {
		for i := l; i < n; i++ {
			s.w <- newWriter(s.srv, s.C)
		}
	} else {
		for i := n; i < l; i++ {
			w := <-s.w
			// Exit without blocking
			go w.exit()
		}
	}
}

// exit close the channel to recive messases and close the channels
// for the writers. And each writer is forced to exit but waiting until
// all messages in chan C are stored
func (s *shard) exit() {
	close(s.C)
	close(s.w)

	wg := &sync.WaitGroup{}
	for w := range s.w {
		wg.Add(1)
		go func(w *writer, wg *sync.WaitGroup) {
			defer wg.Done()
			lib.Debugf("Exiting shard: %d", len(w.C))
			w.exit()
		}(w, wg)
	}
	wg.Wait()
}
