package relay

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/mailrelay/envelope"
)

// registryAddress is the stand-in registry's address. Tests reach the
// stand-in through its in-process transport, so no request leaves the process.
const registryAddress = "https://registry.test/v1/cities"

// standIn is a stand-in registry and the http.RoundTripper that reaches it.
// RoundTrip serves the current answer in the calling goroutine: no listener
// opens and no test server starts.
type standIn struct {
	t      *testing.T
	mu     sync.Mutex
	answer http.HandlerFunc // nil makes every request fail
	calls  map[string]int   // requests by scheme and host
}

func newStandIn(t *testing.T) *standIn {
	return &standIn{t: t, calls: make(map[string]int)}
}

func (s *standIn) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.calls[req.URL.Scheme+"://"+req.URL.Host]++
	answer := s.answer
	s.mu.Unlock()
	plain := req.Method == http.MethodGet && (req.Body == nil || req.Body == http.NoBody) &&
		req.Header.Get("Authorization") == "" && req.Header.Get("Cookie") == "" && req.UserAgent() == "gc-mail-relay"
	if !plain {
		s.t.Errorf("registry request is more than a plain read: %s %s %v", req.Method, req.URL, req.Header)
	}
	if answer == nil {
		return nil, errors.New("connection refused")
	}
	w := httptest.NewRecorder()
	answer(w, req)
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	resp := w.Result()
	resp.Request = req
	return resp, nil
}

func (s *standIn) set(answer http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answer = answer
}

func (s *standIn) reply(code int, body string) {
	s.set(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	})
}

func (s *standIn) count(origin string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[origin]
}

func (s *standIn) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		n += c
	}
	return n
}

// relayFixture prepares an unopened relay for hub, alpha and beta. Each city
// has a signing key; only the cities named are pinned in the keys directory.
func relayFixture(t *testing.T, pinned ...string) *fixture {
	t.Helper()
	f := &fixture{t: t, cfg: DefaultConfig(), sign: make(map[string]ed25519.PrivateKey), box: make(map[string]*ecdh.PrivateKey)}
	f.cfg.Hub = "hub"
	f.cfg.KeysDir = t.TempDir()
	f.cfg.StorePath = filepath.Join(t.TempDir(), "relay.db")
	for _, city := range []string{"hub", "alpha", "beta"} {
		f.addKey(city)
		if !slices.Contains(pinned, city) {
			f.unpin(city)
		}
	}
	t.Cleanup(func() {
		if f.server != nil {
			if err := f.server.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	return f
}

// registryFixture opens a relay that pins hub and alpha and copies the
// stand-in registry, which lists beta under beta's own key.
func registryFixture(t *testing.T, configure func(*Config)) (*fixture, *standIn) {
	t.Helper()
	f := relayFixture(t, "hub", "alpha")
	reg := newStandIn(t)
	reg.reply(200, list(f.entry("beta", "beta")))
	f.cfg.RegistryURL = registryAddress
	f.cfg.RegistryTransport = reg
	if configure != nil {
		configure(&f.cfg)
	}
	f.open()
	return f, reg
}

func (f *fixture) unpin(city string) {
	f.t.Helper()
	if err := os.Remove(filepath.Join(f.cfg.KeysDir, city+".pub")); err != nil {
		f.t.Fatal(err)
	}
}

func publicKey(k ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(k.Public().(ed25519.PublicKey))
}

func row(city, key string) string {
	return fmt.Sprintf(`{"city":%q,"public_key":%q,"registered_at":"2026-01-02T03:04:05Z"}`, city, key)
}

// entry is a row of the registry's list for city carrying signer's public key.
func (f *fixture) entry(city, signer string) string {
	return row(city, publicKey(f.sign[signer]))
}

func list(entries ...string) string {
	return `{"cities":[` + strings.Join(entries, ",") + `]}`
}

// readSigned reads city's queue with a grant signed by signer's private key.
func (f *fixture) readSigned(city, signer string, want int) {
	f.t.Helper()
	path := "/v1/city/" + city + "/queue?wait=0s"
	tok, err := envelope.MintGrant(f.sign[signer], city, city, envelope.RequestFromURL("GET", httptest.NewRequest("GET", path, nil).URL, nil), time.Minute)
	if err != nil {
		f.t.Fatal(err)
	}
	if w := f.request("GET", path, nil, tok); w.Code != want {
		f.t.Fatalf("read of %s signed by %s: got %d, want %d", city, signer, w.Code, want)
	}
}

func (f *fixture) logLen() int {
	f.log.Lock()
	defer f.log.Unlock()
	return f.log.buf.Len()
}

func (f *fixture) logFrom(start int) string {
	f.log.Lock()
	defer f.log.Unlock()
	return f.log.buf.String()[start:]
}

// keysInForce returns the key set sign-in checks against, as base64 by city.
func (f *fixture) keysInForce() map[string]string {
	keys := make(map[string]string)
	for city, key := range f.server.trusted.Load().keys {
		keys[city] = base64.StdEncoding.EncodeToString(key)
	}
	return keys
}

// tick runs the maintenance loop for n reload intervals on an injected clock
// and returns after the last interval's work has finished.
func tick(t *testing.T, s *Server, n int) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	reload, sweep := make(chan time.Time), make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.maintain(ctx, reload, sweep)
	}()
	for i := range n {
		reload <- time.Unix(int64(i+1)*60, 0)
	}
	// The loop takes the sweep only once the last interval's work is done.
	sweep <- time.Unix(int64(n+1)*60, 0)
	cancel()
	<-done
}

// oneLine fails unless text is exactly one log line holding want and none of
// the keys.
func oneLine(t *testing.T, text, want string, keys ...string) {
	t.Helper()
	if strings.Count(text, "\n") != 1 || !strings.Contains(text, want) {
		t.Fatalf("want one log line holding %q, got %q", want, text)
	}
	for _, key := range keys {
		if strings.Contains(text, key) {
			t.Fatalf("log line carries a key: %q", text)
		}
	}
}

func TestRegistryCitySignsIn(t *testing.T) {
	f, _ := registryFixture(t, nil)
	oneLine(t, f.logFrom(0), "registry copy good entries=1 pinned=0 added=1 removed=0 changed=0", publicKey(f.sign["beta"]))
	f.call("beta", "GET", "queue?wait=0s", nil, 200)
	f.call("beta", "PUT", "key", f.statement("beta", "beta", 1), 204)
	f.call("hub", "GET", "keys/beta", nil, 200)
	wire := f.letter("hub", "beta", "first")
	f.call("hub", "POST", "envelopes", wire, 204)
	f.checkQueue("beta", 0, 1, wire)
}

func TestRegistryRemovedRowRefusedWithinOneInterval(t *testing.T) {
	f, reg := registryFixture(t, nil)
	f.call("beta", "PUT", "key", f.statement("beta", "beta", 1), 204)
	path := "/v1/city/beta/queue?wait=20s"
	tok := f.token("beta", "GET", path, nil)
	held := make(chan int, 1)
	go func() { held <- f.request("GET", path, nil, tok).Code }()
	waitHeld(t, f.server, "beta")

	// beta's row is gone; the list still holds a row, so it is not empty.
	reg.reply(200, list(f.entry("alpha", "alpha")))
	start := f.logLen()
	tick(t, f.server, 1)
	oneLine(t, f.logFrom(start), "registry copy good entries=1 pinned=1 added=0 removed=1 changed=0")
	select {
	case code := <-held:
		if code != 401 {
			t.Fatalf("held read of a removed city: got %d, want 401", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("held read did not end at its wait")
	}
	f.call("beta", "GET", "queue?wait=0s", nil, 401)
	f.call("hub", "POST", "envelopes", f.letter("hub", "beta", "after"), 403)
	f.call("hub", "GET", "keys/beta", nil, 403)
}

func waitHeld(t *testing.T, s *Server, city string) {
	t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	probe := time.NewTicker(time.Millisecond)
	defer probe.Stop()
	for {
		s.mu.Lock()
		waiting := s.waiting[city] != nil
		s.mu.Unlock()
		if waiting {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("read did not register")
		case <-probe.C:
		}
	}
}

func TestRegistryBadCopyChangesNothing(t *testing.T) {
	f, reg := registryFixture(t, nil)
	before := f.keysInForce()
	if before["beta"] == "" {
		t.Fatal("beta's registry key is not in force")
	}
	beta := f.entry("beta", "beta")
	other := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	urlSafe := base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 32))
	if !strings.ContainsAny(urlSafe, "-_") {
		t.Fatal("fixture key must differ between the two alphabets")
	}
	padded := func(body string, size int) string { return body + strings.Repeat(" ", size-len(body)) }
	stall := func(s *standIn) {
		s.set(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	}
	for _, tc := range []struct {
		name, reason string
		answer       func(*standIn)
	}{
		{"no answer in time", `reason="timeout"`, stall},
		{"connection refused", `reason="request failed"`, func(s *standIn) { s.set(nil) }},
		{"status 500", `reason="status 500"`, func(s *standIn) { s.reply(500, list(beta)) }},
		{"status 429", `reason="status 429"`, func(s *standIn) { s.reply(429, list(beta)) }},
		{"status 204", `reason="status 204"`, func(s *standIn) { s.reply(204, "") }},
		{"not JSON", `reason="body is not JSON"`, func(s *standIn) { s.reply(200, "<html>registry</html>") }},
		{"truncated JSON", `reason="body is not JSON"`, func(s *standIn) { s.reply(200, `{"cities":[`+beta) }},
		{"data after the object", `reason="body is not JSON"`, func(s *standIn) { s.reply(200, list(beta)+"[]") }},
		{"no list", `reason="body has no cities list"`, func(s *standIn) { s.reply(200, `{"city":"beta"}`) }},
		{"null list", `reason="body has no cities list"`, func(s *standIn) { s.reply(200, `{"cities":null}`) }},
		{"list of another type", `reason="body does not have the list's shape"`, func(s *standIn) { s.reply(200, `{"cities":{}}`) }},
		{"bare array", `reason="body does not have the list's shape"`, func(s *standIn) { s.reply(200, `[`+beta+`]`) }},
		{"entry that is not an object", `reason="body does not have the list's shape"`, func(s *standIn) { s.reply(200, list(beta, `"gamma"`)) }},
		{"null entry", `reason="entry 2: invalid city id"`, func(s *standIn) { s.reply(200, list(beta, "null")) }},
		{"id with a leading hyphen", `reason="entry 2: invalid city id"`, func(s *standIn) { s.reply(200, list(beta, row("-gamma", other))) }},
		{"id in upper case", `reason="entry 2: invalid city id"`, func(s *standIn) { s.reply(200, list(beta, row("Gamma", other))) }},
		{"id with a slash", `reason="entry 2: invalid city id"`, func(s *standIn) { s.reply(200, list(beta, row("gamma/x", other))) }},
		{"empty id", `reason="entry 1: invalid city id"`, func(s *standIn) { s.reply(200, list(row("", other), beta)) }},
		{"no id", `reason="entry 2: invalid city id"`, func(s *standIn) {
			s.reply(200, list(beta, `{"public_key":"`+other+`","registered_at":"2026-01-02T03:04:05Z"}`))
		}},
		{"key of 31 bytes", `reason="entry 2: public key is not standard base64 of 32 bytes"`, func(s *standIn) {
			s.reply(200, list(beta, row("gamma", base64.StdEncoding.EncodeToString(make([]byte, 31)))))
		}},
		{"key of 33 bytes", `reason="entry 2: public key is not standard base64 of 32 bytes"`, func(s *standIn) {
			s.reply(200, list(beta, row("gamma", base64.StdEncoding.EncodeToString(make([]byte, 33)))))
		}},
		{"key that is not base64", `reason="entry 2: public key is not standard base64 of 32 bytes"`, func(s *standIn) {
			s.reply(200, list(beta, row("gamma", "not a key")))
		}},
		{"key in the URL alphabet", `reason="entry 2: public key is not standard base64 of 32 bytes"`, func(s *standIn) {
			s.reply(200, list(beta, row("gamma", urlSafe)))
		}},
		{"key without padding", `reason="entry 2: public key is not standard base64 of 32 bytes"`, func(s *standIn) {
			s.reply(200, list(beta, row("gamma", strings.TrimRight(other, "="))))
		}},
		{"key with a line break", `reason="entry 2: public key is not standard base64 of 32 bytes"`, func(s *standIn) {
			s.reply(200, list(beta, row("gamma", other[:20]+"\n"+other[20:])))
		}},
		{"no key", `reason="entry 2: public key is not standard base64 of 32 bytes"`, func(s *standIn) {
			s.reply(200, list(beta, `{"city":"gamma","registered_at":"2026-01-02T03:04:05Z"}`))
		}},
		{"no registered_at", `reason="entry 2: no registered_at"`, func(s *standIn) {
			s.reply(200, list(beta, `{"city":"gamma","public_key":"`+other+`"}`))
		}},
		{"blank registered_at", `reason="entry 2: no registered_at"`, func(s *standIn) {
			s.reply(200, list(beta, `{"city":"gamma","public_key":"`+other+`","registered_at":" "}`))
		}},
		{"registered_at that is not a string", `reason="body does not have the list's shape"`, func(s *standIn) {
			s.reply(200, list(beta, `{"city":"gamma","public_key":"`+other+`","registered_at":1}`))
		}},
		{"two entries with one id", `reason="entry 2: duplicate city id"`, func(s *standIn) { s.reply(200, list(beta, beta)) }},
		{"two keys for one id", `reason="entry 2: duplicate city id"`, func(s *standIn) { s.reply(200, list(beta, row("beta", other))) }},
		{"body over 1 MiB", `reason="body over 1 MiB"`, func(s *standIn) { s.reply(200, padded(list(beta, row("gamma", other)), 1<<20+1)) }},
		{"declared length over 1 MiB", `reason="body over 1 MiB"`, func(s *standIn) {
			// The body itself is small and good: only its declared length is refused.
			s.set(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", fmt.Sprint(1<<20+1))
				_, _ = io.WriteString(w, list(beta, row("gamma", other)))
			})
		}},
		{"empty list", `reason="empty list"`, func(s *standIn) { s.reply(200, `{"cities":[]}`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func(parent *testing.T) { f.t = parent }(f.t)
			f.t = t
			if tc.name == "no answer in time" {
				// The stand-in holds the request until the client's limit ends it.
				client := f.server.registry.(*registryCopy).client
				defer func(limit time.Duration) { client.Timeout = limit }(client.Timeout)
				client.Timeout = 50 * time.Millisecond
			}
			tc.answer(reg)
			start := f.logLen()
			tick(t, f.server, 1)
			line := f.logFrom(start)
			oneLine(t, line, "registry copy bad ", publicKey(f.sign["beta"]), other, urlSafe)
			if !strings.Contains(line, tc.reason) || !strings.Contains(line, " kept=1 ") {
				t.Fatalf("bad copy line %q: want %q and kept=1", line, tc.reason)
			}
			if after := f.keysInForce(); !maps.Equal(before, after) {
				t.Fatalf("a bad copy changed the key set: before %v, after %v", before, after)
			}
			f.call("beta", "GET", "queue?wait=0s", nil, 200)
		})
	}

	// The limit is 1 MiB: a body of exactly that size is read whole.
	reg.reply(200, padded(list(beta, row("gamma", other)), 1<<20))
	start := f.logLen()
	tick(t, f.server, 1)
	oneLine(t, f.logFrom(start), "registry copy good entries=2 pinned=0 added=1 removed=0 changed=0")
}

func TestRegistryEmptyList(t *testing.T) {
	t.Run("changes nothing by default", func(t *testing.T) {
		f, reg := registryFixture(t, nil)
		reg.reply(200, `{"cities":[]}`)
		start := f.logLen()
		tick(t, f.server, 1)
		oneLine(t, f.logFrom(start), `registry copy bad entries=0 kept=1 reason="empty list"`)
		f.call("beta", "GET", "queue?wait=0s", nil, 200)
	})
	t.Run("removes every registry key when allowed", func(t *testing.T) {
		f, reg := registryFixture(t, func(c *Config) { c.RegistryAllowEmpty = true })
		reg.reply(200, `{"cities":[]}`)
		start := f.logLen()
		tick(t, f.server, 1)
		oneLine(t, f.logFrom(start), "registry copy good entries=0 pinned=0 added=0 removed=1 changed=0")
		f.call("beta", "GET", "queue?wait=0s", nil, 401)
		f.call("alpha", "GET", "queue?wait=0s", nil, 200)
	})
}

func TestRegistryPinnedKeyWins(t *testing.T) {
	f, reg := registryFixture(t, nil)
	// The registry lists alpha and hub under beta's key, which they do not hold.
	reg.reply(200, list(f.entry("beta", "beta"), f.entry("alpha", "beta"), f.entry("hub", "beta")))
	start := f.logLen()
	tick(t, f.server, 1)
	oneLine(t, f.logFrom(start), "registry copy good entries=3 pinned=2 added=0 removed=0 changed=0")
	for _, city := range []string{"alpha", "hub"} {
		f.readSigned(city, city, 200)
		f.readSigned(city, "beta", 401)
	}

	// A pin removed while the registry is down uncovers nothing from the last
	// good copy, and the hub is never taken from the registry. A pin leaves at
	// the second read that misses it.
	reg.reply(503, "")
	f.unpin("alpha")
	f.unpin("hub")
	tick(t, f.server, 2)
	for _, city := range []string{"alpha", "hub"} {
		f.readSigned(city, city, 401)
		f.readSigned(city, "beta", 401)
	}

	// The next good copy makes alpha a registry city. The hub stays out.
	reg.reply(200, list(f.entry("beta", "beta"), f.entry("alpha", "beta"), f.entry("hub", "beta")))
	start = f.logLen()
	tick(t, f.server, 1)
	oneLine(t, f.logFrom(start), "registry copy good entries=3 pinned=1 added=1 removed=0 changed=0")
	f.readSigned("alpha", "beta", 200)
	f.readSigned("hub", "beta", 401)

	// A city pinned while its registry key is in force signs in only with the
	// pinned key, from the reload on.
	listed := f.sign["beta"]
	f.addKey("beta")
	reg.reply(503, "")
	tick(t, f.server, 1)
	f.readSigned("beta", "beta", 200)
	f.sign["listed"] = listed
	f.readSigned("beta", "listed", 401)

	// Unpinned again while the registry is still down, beta's old registry
	// key does not come back.
	f.unpin("beta")
	tick(t, f.server, 2)
	f.readSigned("beta", "beta", 401)
	f.readSigned("beta", "listed", 401)
}

func TestRelayRefusesToStartWithoutPinnedHub(t *testing.T) {
	for _, address := range []string{"", registryAddress} {
		t.Run("address="+address, func(t *testing.T) {
			f := relayFixture(t, "alpha", "beta")
			reg := newStandIn(t)
			reg.reply(200, list(f.entry("hub", "hub"), f.entry("beta", "beta")))
			f.cfg.RegistryURL = address
			f.cfg.RegistryTransport = reg
			s, err := Open(f.cfg, &f.log)
			if err == nil {
				f.server = s
				t.Fatal("relay started with the hub only in the registry")
			}
			if !strings.Contains(err.Error(), "pinned") || strings.Contains(err.Error(), "\n") {
				t.Fatalf("want one plain line naming the pinned hub key, got %q", err)
			}
			if reg.total() != 0 {
				t.Fatal("relay read the registry before refusing to start")
			}
		})
	}
}

func TestBadStartupCopyStartsOnPinnedKeys(t *testing.T) {
	f := relayFixture(t, "hub", "alpha")
	reg := newStandIn(t)
	reg.reply(503, "")
	f.cfg.RegistryURL = registryAddress
	f.cfg.RegistryTransport = reg
	f.open()
	oneLine(t, f.logFrom(0), `registry copy bad entries=0 kept=0 reason="status 503"`)
	f.call("alpha", "GET", "queue?wait=0s", nil, 200)
	f.call("beta", "GET", "queue?wait=0s", nil, 401)
	reg.reply(200, list(f.entry("beta", "beta")))
	tick(t, f.server, 1)
	f.call("beta", "GET", "queue?wait=0s", nil, 200)
}

func TestRegistryAddressRule(t *testing.T) {
	for _, address := range []string{
		"http://registry.test/v1/cities",
		"http://192.0.2.1/v1/cities",
		"http://localhost.registry.test/v1/cities",
		"http://127.0.0.1.registry.test/v1/cities",
		"ftp://registry.test/v1/cities",
		"registry.test/v1/cities",
		"https:///v1/cities",
		"https://reader@registry.test/v1/cities",
		"https://reader:secret@registry.test/v1/cities",
		"https://registry.test:bad/v1/cities",
	} {
		t.Run("refused "+address, func(t *testing.T) {
			f := relayFixture(t, "hub", "alpha")
			reg := newStandIn(t)
			f.cfg.RegistryURL = address
			f.cfg.RegistryTransport = reg
			s, err := Open(f.cfg, &f.log)
			if err == nil {
				f.server = s
				t.Fatal("relay started with a refused registry address")
			}
			if !strings.Contains(err.Error(), "registry URL") || strings.Contains(err.Error(), "secret") {
				t.Fatalf("refusal %q must name the setting and never echo it", err)
			}
			if _, err := os.Stat(f.cfg.StorePath); !errors.Is(err, os.ErrNotExist) || reg.total() != 0 {
				t.Fatal("a refused address must stop the relay before its store or the registry")
			}
		})
	}
	for _, address := range []string{
		"https://registry.test/v1/cities",
		"https://registry.test:8443/v1/cities?format=keys",
		"http://127.0.0.1:8080/v1/cities",
		"http://127.0.0.2/v1/cities",
		"http://localhost/v1/cities",
		"http://[::1]:8080/v1/cities",
	} {
		t.Run("taken "+address, func(t *testing.T) {
			f := relayFixture(t, "hub", "alpha")
			reg := newStandIn(t)
			reg.reply(200, list(f.entry("beta", "beta")))
			f.cfg.RegistryURL = address
			f.cfg.RegistryTransport = reg
			f.open()
			if reg.total() != 1 {
				t.Fatalf("startup copy made %d requests, want 1", reg.total())
			}
			f.call("beta", "GET", "queue?wait=0s", nil, 200)
		})
	}
}

func TestRegistryClient(t *testing.T) {
	f, _ := registryFixture(t, nil)
	client := f.server.registry.(*registryCopy).client
	if client.Timeout != 10*time.Second || client.Jar != nil {
		t.Fatalf("registry client: timeout %v, cookie jar %v; want 10s and none", client.Timeout, client.Jar)
	}
}

func TestRegistryRedirects(t *testing.T) {
	for _, tc := range []struct {
		name, location string
		good           bool
	}{
		{"to another host", "https://elsewhere.test/v1/cities", false},
		{"to another port", "https://registry.test:8443/v1/cities", false},
		{"to plain http", "http://registry.test/v1/cities", false},
		{"with a user name", "https://reader@registry.test/v2/cities", false},
		{"on the same host", "https://registry.test/v2/cities", true},
		{"on the same host in another case", "https://REGISTRY.test/v2/cities", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, reg := registryFixture(t, nil)
			reg.set(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/cities" && strings.EqualFold(r.URL.Host, "registry.test") && r.URL.Scheme == "https" {
					http.Redirect(w, r, tc.location, http.StatusFound)
					return
				}
				_, _ = io.WriteString(w, list(f.entry("alpha", "alpha")))
			})
			start := f.logLen()
			tick(t, f.server, 1)
			line := f.logFrom(start)
			if tc.good {
				oneLine(t, line, "registry copy good entries=1 pinned=1 added=0 removed=1 changed=0")
				f.call("beta", "GET", "queue?wait=0s", nil, 401)
				return
			}
			oneLine(t, line, `registry copy bad entries=0 kept=1 reason="redirect away from the registry address"`)
			if reg.total() != 2 {
				t.Fatalf("redirect was followed: %d requests, want 2 (startup and this copy)", reg.total())
			}
			f.call("beta", "GET", "queue?wait=0s", nil, 200)
		})
	}
	t.Run("without end", func(t *testing.T) {
		f, reg := registryFixture(t, nil)
		reg.set(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://registry.test/v1/cities", http.StatusFound)
		})
		start := f.logLen()
		tick(t, f.server, 1)
		oneLine(t, f.logFrom(start), `reason="too many redirects"`)
		if got := reg.count("https://registry.test"); got != 1+10 {
			t.Fatalf("redirect loop made %d requests, want the startup copy and 10", got)
		}
	})
}

func TestNoRegistryAddressNoRequest(t *testing.T) {
	f := relayFixture(t, "hub", "alpha", "beta")
	reg := newStandIn(t)
	reg.reply(200, list(f.entry("gamma", "beta")))
	f.cfg.RegistryTransport = reg
	f.open()
	tick(t, f.server, 3)
	if reg.total() != 0 {
		t.Fatalf("relay without a registry address made %d requests", reg.total())
	}
	if log := f.logFrom(0); strings.Contains(log, "registry") {
		t.Fatalf("relay without a registry address logged a copy: %q", log)
	}
	if f.cfg.ReloadInterval != time.Minute {
		t.Fatalf("default interval %v, want one minute", f.cfg.ReloadInterval)
	}
	f.call("beta", "GET", "queue?wait=0s", nil, 200)
}

func TestSingleUseSurvivesRegistrySwap(t *testing.T) {
	f, reg := registryFixture(t, nil)
	for i, city := range []string{"hub", "beta"} {
		path := "/v1/city/" + city + "/queue?wait=0s"
		tok := f.token(city, "GET", path, nil)
		if w := f.request("GET", path, nil, tok); w.Code != 200 {
			t.Fatalf("%s: first use got %d", city, w.Code)
		}
		// Each copy adds a city, so a new key set is swapped in.
		extra := fmt.Sprintf("extra%d", i)
		reg.reply(200, list(f.entry("beta", "beta"), f.entry(extra, "alpha")))
		tick(t, f.server, 1)
		if f.keysInForce()[extra] == "" {
			t.Fatal("copy was not swapped in")
		}
		if w := f.request("GET", path, nil, tok); w.Code != 401 {
			t.Fatalf("%s: grant used before the swap got %d after it, want 401", city, w.Code)
		}
	}
}

func (f *fixture) writePin(city, key string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.cfg.KeysDir, city+".pub"), []byte(key), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// tickLog runs one reload interval and returns what it logged, before any
// request of the test adds its own line.
func (f *fixture) tickLog() string {
	f.t.Helper()
	start := f.logLen()
	tick(f.t, f.server, 1)
	return f.logFrom(start)
}

// notWhole is the line a pass writes when its read of the pinned keys is not
// whole.
func notWhole(reason string) string {
	return `trusted key reload failed; keeping the whole key set, no trust decision this interval reason="` + reason + `"`
}

// In a pass whose read of the pinned keys is not whole, the relay makes no
// trust decision: a city whose pin arrives in that pass is not taken from the
// registry. The next whole read puts the pin in force and drops the registry
// row for that city.
func TestPinReadNotWholeMakesNoTrustDecision(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		breakPins    func(*fixture) (repair func())
	}{
		{"a key file invalid", "trust directory contains 1 invalid key files", func(f *fixture) func() {
			f.writePin("broken", "invalid")
			return func() { f.unpin("broken") }
		}},
		{"the directory unreadable", "keys directory could not be read", func(f *fixture) func() {
			moved := f.cfg.KeysDir + ".moved"
			if err := os.Rename(f.cfg.KeysDir, moved); err != nil {
				f.t.Fatal(err)
			}
			return func() {
				if err := os.Rename(moved, f.cfg.KeysDir); err != nil {
					f.t.Fatal(err)
				}
			}
		}},
		{"a verifier that cannot be built", "verifier refused the pinned keys", func(f *fixture) func() {
			// The loader takes this name as a key id; the relay's id rule does not.
			f.writePin("Gamma", publicKey(f.sign["beta"]))
			return func() { f.unpin("Gamma") }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := relayFixture(t, "hub")
			reg := newStandIn(t)
			reg.reply(200, list(f.entry("beta", "beta")))
			f.cfg.RegistryURL = registryAddress
			f.cfg.RegistryTransport = reg
			f.open()
			before := f.keysInForce()

			// alpha's pin file arrives while the read is not whole, and the
			// registry lists alpha under beta's key.
			f.addKey("alpha")
			repair := tc.breakPins(f)
			reg.reply(200, list(f.entry("beta", "beta"), f.entry("alpha", "beta")))
			line := f.tickLog()
			f.readSigned("alpha", "beta", 401)
			f.readSigned("alpha", "alpha", 401)
			f.readSigned("beta", "beta", 200)
			if after := f.keysInForce(); !maps.Equal(before, after) {
				t.Fatalf("a pass whose pin read is not whole changed the key set: before %v, after %v", before, after)
			}
			oneLine(t, line, notWhole(tc.reason))

			repair()
			line = f.tickLog()
			f.readSigned("alpha", "alpha", 200)
			f.readSigned("alpha", "beta", 401)
			oneLine(t, line, "registry copy good entries=2 pinned=1 added=0 removed=0 changed=0")
		})
	}
}

// A pin file absent at one read, as while it is being replaced, keeps the
// whole key set, so the registry's key for that city does not sign in. A pin
// still absent at the next read is gone: that read is whole, and the copy in
// the same interval may list the city.
func TestPinAbsentForOneReloadKeepsTheKeySet(t *testing.T) {
	f, reg := registryFixture(t, nil)
	reg.reply(200, list(f.entry("beta", "beta"), f.entry("alpha", "beta")))
	tick(t, f.server, 1)
	pin, err := os.ReadFile(filepath.Join(f.cfg.KeysDir, "alpha.pub"))
	if err != nil {
		t.Fatal(err)
	}

	f.unpin("alpha")
	line := f.tickLog()
	f.readSigned("alpha", "beta", 401)
	f.readSigned("alpha", "alpha", 200)
	oneLine(t, line, notWhole("1 pinned key files missing since the last good read"))

	f.writePin("alpha", string(pin))
	line = f.tickLog()
	f.readSigned("alpha", "alpha", 200)
	f.readSigned("alpha", "beta", 401)
	oneLine(t, line, "registry copy good entries=2 pinned=1 added=0 removed=0 changed=0")

	f.unpin("alpha")
	tick(t, f.server, 1)
	f.readSigned("alpha", "alpha", 200)
	line = f.tickLog()
	f.readSigned("alpha", "alpha", 401)
	f.readSigned("alpha", "beta", 200)
	oneLine(t, line, "registry copy good entries=2 pinned=0 added=1 removed=0 changed=0")
}

// A key statement stored under a city's old signing key never blocks the
// statement the city signs with its new key, whichever source changed the key.
func TestStatementUnderARetiredKeyDoesNotBlockTheNewKey(t *testing.T) {
	for _, tc := range []struct{ source, city string }{{"registry", "beta"}, {"pin", "alpha"}} {
		t.Run(tc.source, func(t *testing.T) {
			f, reg := registryFixture(t, nil)
			// The holder of the old key publishes a statement dated far ahead.
			f.call(tc.city, "PUT", "key", f.statement(tc.city, tc.city, 1<<62), 204)
			f.addKey(tc.city)
			if tc.source == "registry" {
				f.unpin(tc.city)
				reg.reply(200, list(f.entry("beta", "beta")))
			}
			start := f.logLen()
			tick(t, f.server, 1)
			if tc.source == "registry" {
				oneLine(t, f.logFrom(start), "registry copy good entries=1 pinned=0 added=0 removed=0 changed=1")
			}
			current := f.statement(tc.city, tc.city, 2)
			f.call(tc.city, "PUT", "key", current, 204)
			if got := f.call("hub", "GET", "keys/"+tc.city, nil, 200); !bytes.Equal(got, current) {
				t.Fatal("hub did not get the statement signed with the current key")
			}
			// The current key's own statements still move forward only.
			f.call(tc.city, "PUT", "key", f.statement(tc.city, tc.city, 1), 409)
		})
	}
}
