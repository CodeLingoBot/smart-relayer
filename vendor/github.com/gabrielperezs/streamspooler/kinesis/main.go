package kinesisPool

import (
	"log"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/service/kinesis"
	"github.com/gabrielperezs/monad"
)

const (
	defaultBufferSize      = 1024
	defaultWorkers         = 1
	defaultMaxWorkers      = 10
	defaultMaxRecords      = 500
	defaultThresholdWarmUp = 0.6
	defaultCoolDownPeriod  = 15 * time.Second
)

// Config is the general configuration for the server
type Config struct {
	// Internal clients details
	MaxWorkers      int
	ThresholdWarmUp float64
	Interval        time.Duration
	CoolDownPeriod  time.Duration
	Critical        bool // Handle this stream as critical
	Serializer      func(i interface{}) ([]byte, error)

	// Limits
	Buffer        int
	ConcatRecords bool // Contact many rows in one Kinesis record
	MaxRecords    int  // To send in batch to Kinesis
	Compress      bool // Compress records with snappy

	// Authentication and enpoints
	StreamName string // Kinesis/Kinesis stream name
	Region     string // AWS region
	Profile    string // AWS Profile name
}

type Server struct {
	sync.Mutex

	cfg        Config
	C          chan interface{}
	clients    []*Client
	cliDesired int

	monad *monad.Monad

	chReload chan bool
	chDone   chan bool
	exiting  bool

	awsSvc         *kinesis.Kinesis
	lastConnection time.Time
	lastError      time.Time
	errors         int64
}

// New create a pool of workers
func New(cfg Config) *Server {

	srv := &Server{
		chDone:   make(chan bool),
		chReload: make(chan bool),
		C:        make(chan interface{}, cfg.Buffer),
	}

	go srv._reload()

	srv.Reload(&cfg)

	return srv
}

// Reload the configuration
func (srv *Server) Reload(cfg *Config) (err error) {
	srv.Lock()
	defer srv.Unlock()

	srv.cfg = *cfg

	if srv.cfg.Buffer == 0 {
		srv.cfg.Buffer = defaultBufferSize
	}

	if srv.cfg.MaxWorkers == 0 {
		srv.cfg.MaxWorkers = defaultMaxWorkers
	}

	if srv.cfg.ThresholdWarmUp == 0 {
		srv.cfg.ThresholdWarmUp = defaultThresholdWarmUp
	}

	if srv.cfg.CoolDownPeriod.Nanoseconds() == 0 {
		srv.cfg.CoolDownPeriod = defaultCoolDownPeriod
	}

	if srv.cfg.MaxRecords == 0 {
		srv.cfg.MaxRecords = defaultMaxRecords
	}

	monadCfg := &monad.Config{
		Min:            uint64(1),
		Max:            uint64(srv.cfg.MaxWorkers),
		Interval:       srv.cfg.Interval,
		CoolDownPeriod: srv.cfg.CoolDownPeriod,
		WarmFn: func() bool {
			if srv.cliDesired == 0 {
				return true
			}

			l := float64(len(srv.C))
			if l == 0 {
				return false
			}

			currPtc := (l / float64(cap(srv.C))) * 100

			//log.Printf("Debug: Queue %v, Chan %v%%/%v, Limit: %v%%", l, currPtc, cap(srv.C), srv.cfg.ThresholdWarmUp*100)

			if currPtc > srv.cfg.ThresholdWarmUp*100 {
				return true
			}
			return false
		},
		DesireFn: func(n uint64) {
			srv.cliDesired = int(n)
			select {
			case srv.chReload <- true:
			default:
			}
		},
	}

	if srv.monad == nil {
		srv.monad = monad.New(monadCfg)
	} else {
		go srv.monad.Reload(monadCfg)
	}

	log.Printf("Kinesis config: %#v", srv.cfg)

	select {
	case srv.chReload <- true:
	default:
	}

	return nil
}

// Exit terminate all clients and close the channels
func (srv *Server) Exit() {

	srv.Lock()
	srv.exiting = true
	srv.Unlock()

	if srv.monad != nil {
		srv.monad.Exit()
	}

	close(srv.chReload)

	for _, c := range srv.clients {
		c.Exit()
	}

	if len(srv.C) > 0 {
		log.Printf("Kinesis: messages lost %d", len(srv.C))
	}

	close(srv.C)

	// finishing the server
	srv.chDone <- true
}

func (srv *Server) isExiting() bool {
	srv.Lock()
	defer srv.Unlock()

	if srv.exiting {
		return true
	}

	return false
}

// Waiting to the server if is running
func (srv *Server) Waiting() {
	if srv.chDone == nil {
		return
	}

	<-srv.chDone
	close(srv.chDone)
}
