package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

// maxFaults bounds the faults the status keeps, newest last.
const maxFaults = 32

// Status is what the agent's health path reports.
type Status struct {
	City     string     `json:"city"`
	Place    int64      `json:"place"`
	Degraded string     `json:"degraded,omitempty"`
	LastAsk  *AskRecord `json:"last_ask,omitempty"`
	Faults   []Fault    `json:"faults"`
}

// AskRecord is the last relay read and how it ended.
type AskRecord struct {
	Began   time.Time `json:"began"`
	Ended   time.Time `json:"ended"`
	Outcome string    `json:"outcome"`
}

// Fault is one mail the agent could not file or tell about, or a reuse of
// an id with other content. It never carries a subject or body.
type Fault struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Number int64     `json:"number,omitempty"`
	From   string    `json:"from,omitempty"`
	ID     string    `json:"id,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// board holds the status the health path serves.
type board struct {
	mu sync.Mutex
	s  Status
}

func newBoard(city string, place int64) *board {
	return &board{s: Status{City: city, Place: place, Faults: []Fault{}}}
}

func (b *board) update(fn func(*Status)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fn(&b.s)
}

func (b *board) snapshot() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.s
	s.Faults = append([]Fault{}, b.s.Faults...)
	if b.s.LastAsk != nil {
		last := *b.s.LastAsk
		s.LastAsk = &last
	}
	return s
}

func (b *board) fault(f Fault) {
	b.update(func(s *Status) {
		s.Faults = append(s.Faults, f)
		if len(s.Faults) > maxFaults {
			s.Faults = s.Faults[len(s.Faults)-maxFaults:]
		}
	})
}

func (b *board) degrade(reason string) { b.update(func(s *Status) { s.Degraded = reason }) }

// serveHealth answers gc's health probe on a Unix socket. The answer is 200
// with the status; while the agent cannot reach the relay or file mail it
// adds gc's advisory degraded headers. Nothing else listens.
func serveHealth(path string, b *board) (func() error, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("removing stale health socket: %w", err)
		}
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("health socket: %w", err)
	}
	if err := os.Chmod(path, privateFileMode); err != nil {
		return nil, errors.Join(fmt.Errorf("health socket: %w", err), l.Close())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		s := b.snapshot()
		if s.Degraded != "" {
			w.Header().Set("X-GC-Health", "degraded")
			w.Header().Set("X-GC-Health-Reason", s.Degraded)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(s)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := srv.Shutdown(ctx)
		if serveErr := <-served; !errors.Is(serveErr, http.ErrServerClosed) {
			err = errors.Join(err, serveErr)
		}
		return err
	}, nil
}
