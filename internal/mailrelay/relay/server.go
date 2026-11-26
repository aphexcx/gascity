// Package relay serves sealed mail and signed public keys for configured city pairs.
package relay

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/mailrelay/envelope"
	"github.com/gastownhall/gascity/internal/supervisor"
)

const (
	// MaxHeaderBytes bounds request metadata before a grant can be decoded.
	MaxHeaderBytes = 16 << 10
	grantHeader    = "X-GC-Relay-Grant"
	maxTokenBytes  = 4096
	maxWaitCeiling = 300 * time.Second
)

// Config describes one relay. All processes serving it must share the same store.
type Config struct {
	Hub, KeysDir, StorePath string
	MaxWait, ReloadInterval time.Duration
	PostsPerMinute          int
}

// DefaultConfig supplies operational limits. The hub, store and trust directory are required.
func DefaultConfig() Config {
	return Config{MaxWait: 25 * time.Second, ReloadInterval: time.Minute, PostsPerMinute: 60}
}

type trust struct {
	keys     map[string]ed25519.PublicKey
	verifier *envelope.GrantVerifier
}

// Server is a relay HTTP handler. Close it after its HTTP and maintenance callers exit.
type Server struct {
	config  Config
	store   *store
	trusted atomic.Pointer[trust]
	logger  *log.Logger
	now     func() time.Time
	mu      sync.Mutex
	waiting map[string]chan struct{}
}

// Open validates settings, opens the durable store and loads the initial trusted keys.
func Open(cfg Config, out io.Writer) (*Server, error) {
	switch {
	case !envelope.ValidCityID(cfg.Hub):
		return nil, errors.New("a valid hub city is required")
	case cfg.StorePath == "" || cfg.KeysDir == "":
		return nil, errors.New("store path and keys directory are required")
	case cfg.PostsPerMinute <= 0 || cfg.ReloadInterval <= 0 || cfg.MaxWait <= 0 || cfg.MaxWait > maxWaitCeiling:
		return nil, errors.New("post rate and reload interval must be positive and maximum wait must be in (0, 300s]")
	}
	st, err := openStore(cfg.StorePath)
	if err != nil {
		return nil, fmt.Errorf("open relay store: %w", err)
	}
	s := &Server{config: cfg, store: st, now: time.Now, logger: log.New(out, "", log.LstdFlags|log.LUTC), waiting: make(map[string]chan struct{})}
	if err := s.reloadKeys(); err != nil {
		return nil, errors.Join(err, st.db.Close())
	}
	if err := st.sweep(context.Background(), s.now()); err != nil {
		return nil, errors.Join(err, st.db.Close())
	}
	return s, nil
}

// Close releases the store.
func (s *Server) Close() error { return s.store.db.Close() }

func (s *Server) reloadKeys() error {
	keys, refused, err := supervisor.LoadTrustedKeys(s.config.KeysDir)
	if err != nil {
		return fmt.Errorf("read trusted keys: %w", err)
	}
	if len(refused) > 0 {
		return fmt.Errorf("trust directory contains %d invalid key files", len(refused))
	}
	set := &trust{keys: keys}
	if len(keys) > 0 {
		set.verifier, err = envelope.NewGrantVerifier(keys, s.store)
		if err != nil {
			return fmt.Errorf("build grant verifier: %w", err)
		}
	}
	s.trusted.Store(set)
	return nil
}

func (s *Server) refreshKeys() {
	if err := s.reloadKeys(); err != nil {
		s.logger.Print("trusted key reload failed; retaining last good key set")
	}
}

// Maintain reloads trust and removes expired records until ctx ends.
func (s *Server) Maintain(ctx context.Context) {
	reload := time.NewTicker(s.config.ReloadInterval)
	sweep := time.NewTicker(time.Minute)
	defer reload.Stop()
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-reload.C:
			s.refreshKeys()
		case <-sweep.C:
			if err := s.store.sweep(ctx, s.now()); err != nil && ctx.Err() == nil {
				s.logger.Print("store sweep failed")
			}
		}
	}
}

type reply struct {
	status      int
	body        []byte
	contentType string
}

func status(code int) reply { return reply{status: code} }

func requestSize(r *http.Request) int {
	n := len(r.Method) + len(r.RequestURI) + len(r.Proto) + len(r.Host) + 16
	for name, values := range r.Header {
		for _, v := range values {
			n += len(name) + len(v) + 4
		}
	}
	return n
}

// ServeHTTP authenticates requests before dispatching any city operation.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	caller, call := "-", "unknown"
	size := 0
	var result reply
	switch {
	case requestSize(r) > MaxHeaderBytes:
		result = status(http.StatusRequestHeaderFieldsTooLarge)
	case r.Method == "GET" && r.URL.EscapedPath() == "/healthz":
		call = "health"
		result = reply{200, []byte("ok"), "text/plain; charset=utf-8"}
	case r.ContentLength > envelope.MaxEnvelopeSize:
		result = status(http.StatusRequestEntityTooLarge)
	default:
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, envelope.MaxEnvelopeSize))
		size = len(body)
		if err != nil {
			result = status(http.StatusBadRequest)
			var limit *http.MaxBytesError
			if errors.As(err, &limit) {
				result = status(http.StatusRequestEntityTooLarge)
			}
		} else {
			caller, call, result = s.authorize(r, body)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	if result.contentType != "" {
		w.Header().Set("Content-Type", result.contentType)
	}
	w.WriteHeader(result.status)
	if len(result.body) > 0 {
		if _, err := w.Write(result.body); err != nil {
			s.logger.Printf("caller=%s call=%s status=%d size=%d write_failed=true", caller, call, result.status, size)
			return
		}
	}
	s.logger.Printf("caller=%s call=%s status=%d size=%d", caller, call, result.status, size)
}

func (s *Server) authorize(r *http.Request, body []byte) (string, string, reply) {
	set := s.trusted.Load()
	req := envelope.RequestFromURL(r.Method, r.URL, body)
	tokens := r.Header.Values(grantHeader)
	// Endpoints contain only literal ASCII city IDs and fixed path segments.
	// Reject escapes, including encoded dots or slashes, before interpreting the route.
	tail, ok := strings.CutPrefix(req.Path, "/v1/city/")
	city, route, slash := strings.Cut(tail, "/")
	if !ok || !slash || strings.ContainsAny(req.Path, "%.") || len(tokens) != 1 || len(tokens[0]) > maxTokenBytes || set.verifier == nil || set.verifier.Verify(tokens[0], req) != nil {
		return "-", "unknown", status(http.StatusUnauthorized)
	}
	ctx := r.Context()
	switch {
	case r.Method == "POST" && route == "envelopes" && r.URL.RawQuery == "":
		return city, "post", s.post(ctx, set, city, body)
	case r.Method == "PUT" && route == "key" && r.URL.RawQuery == "":
		stmt, err := envelope.ParseStatement(body)
		if err != nil {
			return city, "publish", status(400)
		}
		if stmt.Verify(city, set.keys[city]) != nil {
			return city, "publish", status(403)
		}
		return city, "publish", dbReply(s.store.publish(ctx, stmt, body))
	case r.Method == "GET" && route == "queue" && len(body) == 0:
		return city, "read", s.heldRead(ctx, r.URL, city, set.keys[city])
	case r.Method == "GET" && strings.HasPrefix(route, "keys/") && len(body) == 0 && r.URL.RawQuery == "":
		return city, "key", s.key(ctx, set, city, strings.TrimPrefix(route, "keys/"))
	default:
		return city, "unknown", status(404)
	}
}

func (s *Server) pair(set *trust, caller, peer string) bool {
	return caller != peer && (caller == s.config.Hub || peer == s.config.Hub) && set.keys[peer] != nil
}

func dbReply(err error) reply {
	switch {
	case err == nil:
		return status(204)
	case errors.Is(err, errConflict), errors.Is(err, envelope.ErrStaleKey):
		return status(409)
	case errors.Is(err, errRate), errors.Is(err, errQueueFull):
		return status(429)
	case errors.Is(err, errCursor):
		return status(400)
	case errors.Is(err, sql.ErrNoRows):
		return status(404)
	default:
		return status(500)
	}
}

func (s *Server) post(ctx context.Context, set *trust, city string, body []byte) reply {
	env, err := envelope.ParseEnvelope(body)
	if err != nil {
		return status(400)
	}
	if env.Label.From != city || !s.pair(set, city, env.Label.To) {
		return status(403)
	}
	if err := s.store.post(ctx, env.Label, body, s.config.PostsPerMinute, s.now()); err != nil {
		return dbReply(err)
	}
	s.mu.Lock()
	if wake := s.waiting[env.Label.To]; wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	s.mu.Unlock()
	return status(204)
}

func (s *Server) key(ctx context.Context, set *trust, city, peer string) reply {
	if !s.pair(set, city, peer) {
		return status(403)
	}
	var wire []byte
	if err := s.store.db.QueryRowContext(ctx, "SELECT wire FROM statements WHERE city=?", peer).Scan(&wire); err != nil {
		return dbReply(err)
	}
	stmt, err := envelope.ParseStatement(wire)
	if err != nil {
		return status(500)
	}
	if stmt.Verify(peer, set.keys[peer]) != nil {
		return status(404)
	}
	return reply{200, wire, "application/octet-stream"}
}

func readParams(u *url.URL, maxWait time.Duration) (int64, time.Duration, error) {
	if _, err := url.ParseQuery(u.RawQuery); err != nil {
		return 0, 0, err
	}
	values := u.Query()
	var after int64
	wait := maxWait
	var err error
	for name, vs := range values {
		if len(vs) != 1 {
			return 0, 0, errCursor
		}
		switch name {
		case "after":
			after, err = strconv.ParseInt(vs[0], 10, 64)
		case "wait":
			wait, err = time.ParseDuration(vs[0])
		default:
			return 0, 0, errCursor
		}
		if err != nil {
			return 0, 0, err
		}
	}
	if after < 0 || wait < 0 {
		return 0, 0, errCursor
	}
	return after, min(wait, maxWait), nil
}

func (s *Server) heldRead(ctx context.Context, u *url.URL, city string, key ed25519.PublicKey) reply {
	after, wait, err := readParams(u, s.config.MaxWait)
	if err != nil {
		return status(400)
	}
	wake := make(chan struct{}, 1)
	s.mu.Lock()
	if s.waiting[city] != nil {
		s.mu.Unlock()
		return status(409)
	}
	s.waiting[city] = wake
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.waiting, city); s.mu.Unlock() }()
	if err := s.store.acknowledge(ctx, city, after); err != nil {
		return dbReply(err)
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	// A second process can publish through the same store without a local wake.
	poll := time.NewTicker(250 * time.Millisecond)
	defer poll.Stop()
	for {
		if !bytes.Equal(key, s.trusted.Load().keys[city]) {
			return status(401)
		}
		batch, err := s.store.read(ctx, city, after)
		if err != nil {
			return dbReply(err)
		}
		if len(batch) > 0 || wait == 0 {
			b, err := json.Marshal(batch)
			if err != nil {
				return status(500)
			}
			return reply{200, b, "application/json"}
		}
		select {
		case <-deadline.C:
			wait = 0
		case <-ctx.Done():
			return status(408)
		case <-poll.C:
		case <-wake:
		}
	}
}
