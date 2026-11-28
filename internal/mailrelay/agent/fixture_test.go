package agent

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/mailrelay/envelope"
	"github.com/gastownhall/gascity/internal/mailrelay/relay"
)

const (
	subjectSentinel = "private subject sentinel"
	bodySentinel    = "private body sentinel"
	apiBase         = "http://127.0.0.1:8372"
	relayBase       = "https://relay.test"
	// testTimeout outlasts every wait the agent makes, since inside a
	// synctest bubble the deadline runs on the fake clock.
	testTimeout = 5 * time.Minute
)

// handlerTransport serves each request with a handler in the calling
// goroutine: a full HTTP round trip with no socket and no test server.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.URL = &url.URL{Path: req.URL.Path, RawPath: req.URL.RawPath, RawQuery: req.URL.RawQuery}
	r.RequestURI = r.URL.RequestURI()
	if r.Host == "" {
		r.Host = req.URL.Host
	}
	if r.Body == nil {
		r.Body = http.NoBody
	}
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, r)
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// problem answers the way the city API reports a problem.
func problem(r *http.Request, status int, code string) *http.Response {
	body := fmt.Sprintf(`{"code":%q,"status":%d,"title":"problem"}`, code, status)
	return &http.Response{
		StatusCode: status, Request: r, Body: io.NopCloser(strings.NewReader(body)),
		Header: http.Header{"Content-Type": {"application/problem+json"}},
	}
}

// refused is the error a dial to a closed port reports.
func refused() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
}

// syncBuffer is a log sink safe for concurrent writers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) lines(prefix string) []string {
	var out []string
	for _, line := range strings.Split(b.String(), "\n") {
		if _, rest, ok := strings.Cut(line, " "); ok && strings.HasPrefix(rest, prefix) {
			out = append(out, rest)
		}
	}
	return out
}

// recorder notes each request a transport sees and wakes waiters.
type recorder struct {
	mu       sync.Mutex
	requests []recorded
	changed  chan struct{}
}

type recorded struct {
	method, path, query string
	header              http.Header
	body                []byte
}

func newRecorder() *recorder { return &recorder{changed: make(chan struct{}, 1)} }

func (r *recorder) wrap(next http.RoundTripper) http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body []byte
		if req.Body != nil {
			body, _ = io.ReadAll(req.Body)
			req.Body = io.NopCloser(bytes.NewReader(body))
		}
		r.note(recorded{method: req.Method, path: req.URL.EscapedPath(), query: req.URL.RawQuery, header: req.Header.Clone(), body: body})
		defer r.poke()
		return next.RoundTrip(req)
	})
}

func (r *recorder) note(rec recorded) {
	r.mu.Lock()
	r.requests = append(r.requests, rec)
	r.mu.Unlock()
	r.poke()
}

func (r *recorder) poke() {
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

func (r *recorder) all() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.requests...)
}

func (r *recorder) matching(method, pathSuffix string) []recorded {
	var out []recorded
	for _, rec := range r.all() {
		if rec.method == method && strings.HasSuffix(rec.path, pathSuffix) {
			out = append(out, rec)
		}
	}
	return out
}

// reads returns the place sent with each queue read, in order.
func (r *recorder) reads() []int64 {
	var out []int64
	for _, rec := range r.matching("GET", "/queue") {
		q, _ := url.ParseQuery(rec.query)
		n, _ := strconv.ParseInt(q.Get("after"), 10, 64)
		out = append(out, n)
	}
	return out
}

// await blocks until cond holds, waking on each recorded request.
func (r *recorder) await(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(testTimeout)
	for !cond() {
		select {
		case <-r.changed:
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// relayFixture is a real relay served in process.
type relayFixture struct {
	t         *testing.T
	server    *relay.Server
	storeDir  string
	keysDir   string
	log       syncBuffer
	recorder  *recorder
	transport http.RoundTripper
}

func newRelayFixture(t *testing.T, trusted map[string]ed25519.PublicKey) *relayFixture {
	t.Helper()
	f := &relayFixture{t: t, storeDir: t.TempDir(), keysDir: t.TempDir(), recorder: newRecorder()}
	for city, pub := range trusted {
		f.trust(city, pub)
	}
	cfg := relay.DefaultConfig()
	cfg.Hub, cfg.KeysDir, cfg.StorePath = "hub", f.keysDir, filepath.Join(f.storeDir, "relay.db")
	cfg.MaxWait = 2 * time.Second
	s, err := relay.Open(cfg, &f.log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	f.server = s
	f.transport = f.recorder.wrap(handlerTransport{s})
	return f
}

func (f *relayFixture) trust(city string, pub ed25519.PublicKey) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.keysDir, city+".pub"), []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// post sends a raw wire envelope as city, the way a misbehaving peer could.
func (f *relayFixture) post(city string, signer ed25519.PrivateKey, wire []byte) int {
	f.t.Helper()
	req, err := http.NewRequest("POST", relayBase+"/v1/city/"+city+"/envelopes", bytes.NewReader(wire))
	if err != nil {
		f.t.Fatal(err)
	}
	tok, err := envelope.MintGrant(signer, city, city, envelope.RequestFromURL("POST", req.URL, wire), time.Minute)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("X-GC-Relay-Grant", tok)
	resp, err := f.transport.RoundTrip(req)
	if err != nil {
		f.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// storeBytes returns every byte the relay keeps on disk.
func (f *relayFixture) storeBytes() []byte {
	f.t.Helper()
	var all []byte
	entries, err := os.ReadDir(f.storeDir)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(f.storeDir, e.Name()))
		if err != nil {
			f.t.Fatal(err)
		}
		all = append(all, b...)
	}
	return all
}

// apiState is the smallest city state the real mail route needs. Any other
// method reaches the nil embedded interface and fails the test loudly.
type apiState struct {
	api.State
	name, path string
	cfg        *config.City
	mail       mail.Provider
	events     events.Provider
}

func (s *apiState) Config() *config.City              { return s.cfg }
func (s *apiState) CityName() string                  { return s.name }
func (s *apiState) CityPath() string                  { return s.path }
func (s *apiState) MailProvider(string) mail.Provider { return s.mail }
func (s *apiState) MailProviders() map[string]mail.Provider {
	return map[string]mail.Provider{s.name: s.mail}
}
func (s *apiState) EventProvider() events.Provider        { return s.events }
func (s *apiState) SessionsBeadStore() beads.SessionStore { return beads.SessionStore{} }
func (s *apiState) CityBeadStore() beads.Store            { return nil }
func (s *apiState) BeadStore(string) beads.Store          { return nil }
func (s *apiState) BeadStores() map[string]beads.Store    { return nil }
func (s *apiState) StartedAt() time.Time                  { return time.Time{} }
func (s *apiState) Version() string                       { return "test" }

func (s *apiState) ListCities() []api.CityInfo {
	return []api.CityInfo{{Name: s.name, Path: s.path, Running: true}}
}

func (s *apiState) CityState(name string) api.State {
	if name == s.name {
		return s
	}
	return nil
}

// cityAPI is a city's real mail API over an in-memory store.
type cityAPI struct {
	t        *testing.T
	state    *apiState
	store    *beads.MemStore
	handler  http.Handler
	recorder *recorder
}

func newCityAPI(t *testing.T, name string, recipients ...string) *cityAPI {
	t.Helper()
	store := beads.NewMemStore()
	return newCityAPIWith(t, name, store, beadmail.New(store), recipients...)
}

func newCityAPIWith(t *testing.T, name string, store *beads.MemStore, provider mail.Provider, recipients ...string) *cityAPI {
	t.Helper()
	cfg := &config.City{Workspace: config.Workspace{Name: name}}
	for _, r := range recipients {
		cfg.Agents = append(cfg.Agents, config.Agent{Name: r})
	}
	c := &cityAPI{t: t, store: store, recorder: newRecorder()}
	c.state = &apiState{name: name, path: t.TempDir(), cfg: cfg, mail: provider, events: events.NewFake()}
	c.serve()
	return c
}

func (c *cityAPI) serve() {
	mux := api.NewSupervisorMux(c.state, nil, false, "test", "", time.Now())
	c.handler = mux.WithAnyHostAllowed().Handler()
}

// recreate builds a new API process over the same mail store.
func (c *cityAPI) recreate() { c.serve() }

func (c *cityAPI) transport() http.RoundTripper {
	return c.recorder.wrap(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return handlerTransport{c.handler}.RoundTrip(r)
	}))
}

// messages returns every message the city holds, archived ones included.
func (c *cityAPI) messages() []beads.Bead {
	c.t.Helper()
	all, err := c.store.List(beads.ListQuery{Type: "message", IncludeClosed: true, AllowScan: true, TierMode: beads.TierBoth})
	if err != nil {
		c.t.Fatal(err)
	}
	return all
}

func (c *cityAPI) inbox(recipient string) []mail.Message {
	c.t.Helper()
	msgs, err := c.state.mail.Inbox(recipient)
	if err != nil {
		c.t.Fatal(err)
	}
	return msgs
}

func (c *cityAPI) posts() []recorded { return c.recorder.matching("POST", "/mail") }

// standInGC answers the agent's gc commands the way gc would.
type standInGC struct {
	mu      sync.Mutex
	running bool
	broken  bool
	calls   [][]string
	after   func(args []string)
}

func (g *standInGC) Run(_ context.Context, args ...string) error {
	g.mu.Lock()
	g.calls = append(g.calls, append([]string(nil), args...))
	running, broken, after := g.running, g.broken, g.after
	g.mu.Unlock()
	if after != nil {
		defer after(args)
	}
	switch {
	case broken:
		return errors.New("gc: exit status 2")
	case len(args) >= 2 && args[0] == "session" && args[1] == "nudge" && !running:
		return errors.New("gc session nudge: session not running: exit status 1")
	}
	return nil
}

func (g *standInGC) commands() [][]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([][]string(nil), g.calls...)
}

// cityFixture lays out one city's agent files.
type cityFixture struct {
	t        *testing.T
	id       string
	dir      string
	signer   ed25519.PrivateKey
	settings Settings
	log      *syncBuffer
	gc       *standInGC
	boxKey   *ecdh.PublicKey
}

func newCityFixture(t *testing.T, id string) *cityFixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = pub
	dir := t.TempDir()
	c := &cityFixture{t: t, id: id, dir: dir, signer: priv, log: &syncBuffer{}, gc: &standInGC{running: true}}
	keyPath := filepath.Join(dir, "signing.key")
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	peers := filepath.Join(dir, "peers")
	if err := os.Mkdir(peers, 0o700); err != nil {
		t.Fatal(err)
	}
	c.settings = DefaultSettings(func(string) string { return "" })
	c.settings.City = id
	c.settings.RelayURL = relayBase
	c.settings.SigningKey = keyPath
	c.settings.EncryptionKeysDir = filepath.Join(dir, "keys")
	c.settings.PeersDir = peers
	c.settings.APIURL = apiBase
	c.settings.GCCity = id
	c.settings.StateDir = filepath.Join(dir, "state")
	c.settings.Wait = 200 * time.Millisecond
	c.settings.RequestLimit = 5 * time.Second
	return c
}

func (c *cityFixture) public() ed25519.PublicKey { return c.signer.Public().(ed25519.PublicKey) }

func (c *cityFixture) pin(peer *cityFixture) { c.pinKey(peer.id, peer.public()) }

func (c *cityFixture) pinKey(id string, pub ed25519.PublicKey) {
	c.t.Helper()
	path := filepath.Join(c.settings.PeersDir, id+".pub")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o600); err != nil {
		c.t.Fatal(err)
	}
}

// keygen makes the city's first encryption key.
func (c *cityFixture) keygen() {
	c.t.Helper()
	info, err := Keygen(c.settings, false, io.Discard)
	if err != nil {
		c.t.Fatal(err)
	}
	key, err := ecdh.X25519().NewPublicKey(info.PublicKey)
	if err != nil {
		c.t.Fatal(err)
	}
	c.boxKey = key
}

func (c *cityFixture) deps(relayTransport, apiTransport http.RoundTripper) Deps {
	return Deps{RelayTransport: relayTransport, APITransport: apiTransport, GC: c.gc, Log: c.log, Sleep: noSleep}
}

// noSleep skips every back-off wait.
func noSleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

// seal makes an envelope from sender to recipient as a peer would.
func seal(t testing.TB, from *cityFixture, to *cityFixture, id string, sent time.Time, m envelope.Mail) []byte {
	t.Helper()
	label := envelope.Label{Version: envelope.Version, ID: id, From: from.id, To: to.id, Sent: sent.Unix()}
	m.ID = id
	env, err := envelope.Seal(label, m, from.signer, to.boxKey)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := env.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func letter(to string) envelope.Mail {
	return envelope.Mail{From: "writer", To: to, Subject: subjectSentinel, Body: bodySentinel}
}

// running is one agent run in the background.
type running struct {
	cancel context.CancelFunc
	done   chan error
}

func start(t testing.TB, s Settings, d Deps) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- Run(ctx, s, d) }()
	t.Cleanup(func() { r.stop(t) })
	return r
}

// stop cancels the run and fails the test if the run ended in an error.
func (r *running) stop(t testing.TB) {
	t.Helper()
	r.cancel()
	select {
	case err, ok := <-r.done:
		if !ok {
			return
		}
		close(r.done)
		if err != nil {
			t.Errorf("agent run: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("agent did not stop")
	}
}

// fileDigest returns a file's content hash, to prove it never changed.
func fileDigest(t testing.TB, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// scriptedRelay is a stand-in relay with a hand-filled queue. It checks no
// grant; tests that need the relay's own checks use relayFixture.
type scriptedRelay struct {
	mu       sync.Mutex
	queue    []relay.Delivery
	last     int64 // the highest number handed out, as the relay keeps it
	recorder *recorder
	wake     chan struct{}
}

func newScriptedRelay() *scriptedRelay {
	return &scriptedRelay{recorder: newRecorder(), wake: make(chan struct{}, 1)}
}

func (s *scriptedRelay) add(number int64, wire []byte) {
	s.mu.Lock()
	s.queue = append(s.queue, relay.Delivery{Number: number, Envelope: wire})
	s.last = max(s.last, number)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *scriptedRelay) transport() http.RoundTripper { return s.recorder.wrap(handlerTransport{s}) }

func (s *scriptedRelay) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/key"):
		w.WriteHeader(http.StatusNoContent)
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/queue"):
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		wait, _ := time.ParseDuration(r.URL.Query().Get("wait"))
		s.mu.Lock()
		ahead := after > s.last
		s.mu.Unlock()
		if ahead {
			// The relay refuses a place above the highest number it handed out.
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		for {
			s.mu.Lock()
			kept := s.queue[:0]
			for _, d := range s.queue {
				if d.Number > after {
					kept = append(kept, d)
				}
			}
			s.queue = kept
			batch := append([]relay.Delivery{}, kept...)
			s.mu.Unlock()
			if len(batch) > 0 {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(batch)
				return
			}
			select {
			case <-s.wake:
			case <-timer.C:
				_, _ = io.WriteString(w, "[]")
				return
			case <-r.Context().Done():
				w.WriteHeader(http.StatusRequestTimeout)
				return
			}
		}
	default:
		http.Error(w, fmt.Sprintf("unexpected %s %s", r.Method, r.URL.Path), http.StatusNotFound)
	}
}
