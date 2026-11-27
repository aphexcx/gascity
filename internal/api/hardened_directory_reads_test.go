package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/supervisor"
)

// A registered city's key in the hardened listener's trusted directory is for
// sending: it verifies write grants. It never signs a read. These tests walk
// every per-city read route of the hardened listener with a read grant signed
// by a directory key (refused by the read gate on every route) and with the
// listener's own inline read key (admitted on every route).

const (
	directoryProbeCity = "alpha" // the city the hardened listener serves
	directoryKid       = "beta"  // a registered city's key, from the directory
	inlineReadKid      = "hub"   // the listener's own inline read key
)

type directoryReadKeys struct {
	dirPub, readPub   ed25519.PublicKey
	dirPriv, readPriv ed25519.PrivateKey
}

func newDirectoryReadKeys(t *testing.T) directoryReadKeys {
	t.Helper()
	var k directoryReadKeys
	k.dirPub, k.dirPriv = mustKeypair(t)
	k.readPub, k.readPriv = mustKeypair(t)
	return k
}

// newDirectoryListener builds a hardened listener over a mux that serves
// directoryProbeCity and has no primary read gate, with the directory holding
// directoryKid. inlineRead adds the listener's own read key (inlineReadKid).
func newDirectoryListener(t *testing.T, keys directoryReadKeys, inlineRead bool) (*HardenedAuth, *SupervisorMux) {
	t.Helper()
	dir := t.TempDir()
	writeTrustedKey(t, dir, directoryKid, keys.dirPub)
	sm := newTestSupervisorMux(t, map[string]*fakeState{directoryProbeCity: newFakeState(t)}).WithAnyHostAllowed()
	cfg := supervisor.Section{Hardened: supervisor.HardenedListener{Port: 8447, TrustedKeysDir: dir, TrustedKeysReloadInterval: "0"}}
	if inlineRead {
		cfg.Hardened.ReadAuthVerifyKey = inlineReadKid + ":" + base64.StdEncoding.EncodeToString(keys.readPub)
	}
	h, err := NewHardenedAuth(sm, cfg, t.Logf)
	if err != nil {
		t.Fatalf("NewHardenedAuth: %v", err)
	}
	return h, sm
}

type cityReadRoute struct {
	method, pattern, path string
}

var pathParam = regexp.MustCompile(`\{([^}]+)\}`)

// perCityReadRoutes lists every GET and HEAD operation the per-city API
// registers, read from the mux's own registered operations so a route added
// later is walked with no edit here, plus one path under the per-city prefix
// that no handler serves: the read gate decides by path, before any route. A
// GET operation is also probed as HEAD, since the mux serves HEAD through a
// GET pattern and the read gate covers both methods.
func perCityReadRoutes(t *testing.T, sm *SupervisorMux) []cityReadRoute {
	t.Helper()
	const cityRoot = "/v0/city/{cityName}"
	var routes []cityReadRoute
	for pattern, item := range sm.humaAPI.OpenAPI().Paths {
		if pattern != cityRoot && !strings.HasPrefix(pattern, cityRoot+"/") {
			continue
		}
		path := pathParam.ReplaceAllStringFunc(pattern, func(param string) string {
			if param == "{cityName}" {
				return directoryProbeCity
			}
			return "probe"
		})
		if item.Get != nil {
			routes = append(routes, cityReadRoute{http.MethodGet, pattern, path})
		}
		if item.Get != nil || item.Head != nil {
			routes = append(routes, cityReadRoute{http.MethodHead, pattern, path})
		}
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].pattern != routes[j].pattern {
			return routes[i].pattern < routes[j].pattern
		}
		return routes[i].method < routes[j].method
	})
	var sawBeads bool
	for _, r := range routes {
		sawBeads = sawBeads || (r.method == http.MethodGet && r.pattern == cityRoot+"/beads")
	}
	if !sawBeads {
		t.Fatalf("registered per-city reads (%d) lack GET %s/beads; the walk is not reading the registered operations", len(routes), cityRoot)
	}
	unserved := cityRoot + "/no-handler-serves-this-read"
	unservedPath := strings.Replace(unserved, "{cityName}", directoryProbeCity, 1)
	return append(routes,
		cityReadRoute{http.MethodGet, unserved, unservedPath},
		cityReadRoute{http.MethodHead, unserved, unservedPath})
}

// probeWriter records the first status written and the body written with it.
type probeWriter struct {
	mu      sync.Mutex
	header  http.Header
	status  int
	body    bytes.Buffer
	written chan struct{}
}

func (p *probeWriter) Header() http.Header { return p.header }

func (p *probeWriter) WriteHeader(code int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.status == 0 {
		p.status = code
		close(p.written)
	}
}

func (p *probeWriter) Write(b []byte) (int, error) {
	p.WriteHeader(http.StatusOK)
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.body.Write(b)
}

func (p *probeWriter) Flush() {}

const readProbeBound = 3 * time.Second

// probeRead serves one read and returns the status and body the listener gave.
// The request is canceled once a status is out, and the probe stops waiting
// after readProbeBound, so an admitted stream can never hold the test open.
// Status 0 means no status within the bound: the gate answers at once, so an
// unanswered read was admitted to a handler.
func probeRead(h http.Handler, r *http.Request) (int, string) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	w := &probeWriter{header: http.Header{}, written: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(w, r.WithContext(ctx))
	}()
	bound := time.NewTimer(readProbeBound)
	defer bound.Stop()
	select {
	case <-done:
	case <-w.written:
		cancel()
		select {
		case <-done:
		case <-bound.C:
		}
	case <-bound.C:
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status, w.body.String()
}

// signedRead builds a read of route signed by kid with priv, bound to exactly
// that method and path (no query).
func signedRead(t *testing.T, route cityReadRoute, priv ed25519.PrivateKey, kid, jti string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(route.method, route.path, nil)
	if priv != nil {
		g := readGrant(time.Now(), directoryProbeCity, route.method, route.path, "", jti)
		g.Kid = kid
		r.Header.Set(readAuthHeader, mintToken(t, priv, g))
	}
	return r
}

func isReadGateRejection(status int, body string) bool {
	return status == problemReadAuthRejected.status && body == string(problemReadAuthRejected.body)
}

func isReadGateMissingGrant(status int, body string) bool {
	return status == problemReadAuthMissingGrant.status && body == string(problemReadAuthMissingGrant.body)
}

// A1: a read grant signed by a directory key is refused by the read gate on
// every per-city read route, with and without an inline read key on the
// listener.
func TestHardenedDirectoryKeyReadRefusedOnEveryRoute(t *testing.T) {
	keys := newDirectoryReadKeys(t)
	for _, tc := range []struct {
		name       string
		inlineRead bool
	}{
		{"directory-and-inline-read-key", true},
		{"directory-only", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, sm := newDirectoryListener(t, keys, tc.inlineRead)
			routes := perCityReadRoutes(t, sm)
			t.Logf("a1 %s: %d read routes", tc.name, len(routes))
			for i, route := range routes {
				status, body := probeRead(h, signedRead(t, route, keys.dirPriv, directoryKid, fmt.Sprintf("a1-%s-%d", tc.name, i)))
				refused := isReadGateRejection(status, body)
				t.Logf("a1 %s %s %s -> %d read-gate-403=%t", tc.name, route.method, route.pattern, status, refused)
				if !refused {
					t.Errorf("%s %s (%s): directory key read answered %d %q, want the read gate's 403", route.method, route.pattern, tc.name, status, body)
				}
			}
		})
	}
}

// A2: the listener's own inline read key reads every per-city read route: the
// read gate admits the grant and the answer is the handler's own.
func TestHardenedInlineReadKeyReadsEveryRoute(t *testing.T) {
	keys := newDirectoryReadKeys(t)
	h, sm := newDirectoryListener(t, keys, true)
	routes := perCityReadRoutes(t, sm)
	t.Logf("a2: %d read routes", len(routes))
	for i, route := range routes {
		status, body := probeRead(h, signedRead(t, route, keys.readPriv, inlineReadKid, fmt.Sprintf("a2-%d", i)))
		gate := isReadGateRejection(status, body) || isReadGateMissingGrant(status, body)
		t.Logf("a2 %s %s -> %d read-gate=%t", route.method, route.pattern, status, gate)
		if gate {
			t.Errorf("%s %s: inline read key refused by the read gate: %d %q", route.method, route.pattern, status, body)
		}
	}
}

func serveHardenedRead(t *testing.T, h http.Handler, path string, priv ed25519.PrivateKey, kid, jti string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if priv != nil {
		g := readGrant(time.Now(), directoryProbeCity, http.MethodGet, path, "", jti)
		g.Kid = kid
		r.Header.Set(readAuthHeader, mintToken(t, priv, g))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

func serveDirectoryWrite(t *testing.T, h http.Handler, path string, body []byte, priv ed25519.PrivateKey, kid, jti string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	r.Header.Set(csrfHeaderName, "true")
	g := grantFor(time.Now(), directoryProbeCity, http.MethodPost, path, body, jti)
	g.Kid = kid
	r.Header.Set(writeAuthHeader, mintToken(t, priv, g))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// assertReadsClosed checks K3 on a hardened listener with a directory and no
// inline read key: per-city reads are refused by the read gate (unsigned 401,
// signed by any directory key 403), other reads by the scope guard, /health and
// /v0/cities stay public, and the primary listener (no read gate) is unchanged.
func assertReadsClosed(t *testing.T, stage string, h *HardenedAuth, sm *SupervisorMux, signers map[string]ed25519.PrivateKey) {
	t.Helper()
	const path = "/v0/city/" + directoryProbeCity + "/beads"
	if status, body := serveHardenedRead(t, h, path, nil, "", ""); !isReadGateMissingGrant(status, body) {
		t.Errorf("%s: unsigned per-city read = %d %q, want the read gate's 401", stage, status, body)
	}
	for kid, priv := range signers {
		if status, body := serveHardenedRead(t, h, path, priv, kid, stage+"-"+kid); !isReadGateRejection(status, body) {
			t.Errorf("%s: read signed by directory key %s = %d %q, want the read gate's 403", stage, kid, status, body)
		}
	}
	if status, body := serveHardenedRead(t, h, "/v0/events", nil, "", ""); status != problemHardenedReadScopeRefused.status || body != string(problemHardenedReadScopeRefused.body) {
		t.Errorf("%s: read outside the per-city API = %d %q, want the scope guard's 403", stage, status, body)
	}
	for _, public := range []string{"/health", "/v0/cities"} {
		if status, body := serveHardenedRead(t, h, public, nil, "", ""); status != http.StatusOK {
			t.Errorf("%s: public %s = %d %q", stage, public, status, body)
		}
	}
	if status, body := serveHardenedRead(t, sm.Handler(), path, nil, "", ""); status != http.StatusOK {
		t.Errorf("%s: primary per-city read = %d %q, want 200 (no primary read gate)", stage, status, body)
	}
}

// A3 (K3): a listener with a directory and no inline read key refuses every
// per-city read and never takes the primary's open read posture: at boot, after
// a reload that added a key, after a reload that left the directory empty, and
// after a failed reload. A start with an empty directory and an inline write
// key comes up the same way.
func TestHardenedDirectoryWithoutReadKeyClosesReads(t *testing.T) {
	for _, tc := range []struct {
		name          string
		bootDirectory bool
	}{
		{"directory-key-at-boot", true},
		{"empty-directory-inline-write-key", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys := newDirectoryReadKeys(t)
			dir := t.TempDir()
			cfg := supervisor.Section{Hardened: supervisor.HardenedListener{Port: 8447, TrustedKeysDir: dir, TrustedKeysReloadInterval: "0"}}
			signers := map[string]ed25519.PrivateKey{}
			if tc.bootDirectory {
				writeTrustedKey(t, dir, directoryKid, keys.dirPub)
				signers[directoryKid] = keys.dirPriv
			} else {
				inlinePub, _ := mustKeypair(t)
				cfg.Hardened.WriteAuthVerifyKey = "inline-write:" + base64.StdEncoding.EncodeToString(inlinePub)
			}
			sm := newTestSupervisorMux(t, map[string]*fakeState{directoryProbeCity: newFakeState(t)}).WithAnyHostAllowed()
			if sm.readAuth != nil {
				t.Fatal("the primary must have no read gate for this test")
			}
			h, err := NewHardenedAuth(sm, cfg, t.Logf)
			if err != nil {
				t.Fatalf("a directory with no inline read key must start: %v", err)
			}
			assertReadsClosed(t, "boot", h, sm, signers)

			gammaPub, gammaPriv := mustKeypair(t)
			writeTrustedKey(t, dir, "gamma", gammaPub)
			if err := h.Reload(); err != nil {
				t.Fatal(err)
			}
			signers["gamma"] = gammaPriv
			assertReadsClosed(t, "reload-added", h, sm, signers)
			if status, body := serveDirectoryWrite(t, h, "/v0/city/"+directoryProbeCity+"/mail", []byte(`{"from":"gamma/mayor","to":"worker","subject":"added"}`), gammaPriv, "gamma", "gamma-write"); status != http.StatusCreated {
				t.Fatalf("added key's write = %d %q, want 201", status, body)
			}

			for kid := range signers {
				if err := os.Remove(filepath.Join(dir, kid+".pub")); err != nil {
					t.Fatal(err)
				}
			}
			if err := h.Reload(); err != nil {
				t.Fatal(err)
			}
			assertReadsClosed(t, "reload-emptied", h, sm, signers)

			if err := os.WriteFile(filepath.Join(dir, "bad.pub"), []byte("malformed"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := h.current.Load()
			if err := h.Reload(); err == nil {
				t.Fatal("malformed directory file reloaded")
			}
			if h.current.Load() != before {
				t.Fatal("failed reload replaced the active snapshot")
			}
			assertReadsClosed(t, "reload-failed", h, sm, signers)
		})
	}
}

// A4 (K1, K4): a registered city's key still sends mail through the hardened
// listener; the mail is read with the listener's own read key; the sender's
// key cannot read the mailbox it wrote to.
func TestHardenedDirectoryKeySendsMailButCannotReadIt(t *testing.T) {
	keys := newDirectoryReadKeys(t)
	h, _ := newDirectoryListener(t, keys, true)
	const mailPath = "/v0/city/" + directoryProbeCity + "/mail"
	status, body := serveDirectoryWrite(t, h, mailPath, []byte(`{"from":"beta/mayor","to":"worker","subject":"from beta","body":"hello"}`), keys.dirPriv, directoryKid, "a4-send")
	if status != http.StatusCreated {
		t.Fatalf("directory key mail POST = %d %q, want 201", status, body)
	}
	const inbox = mailPath + "?agent=myrig/worker"
	readInbox := func(priv ed25519.PrivateKey, kid, jti string) (int, string) {
		r := httptest.NewRequest(http.MethodGet, inbox, nil)
		g := readGrant(time.Now(), directoryProbeCity, http.MethodGet, mailPath, "agent=myrig/worker", jti)
		g.Kid = kid
		r.Header.Set(readAuthHeader, mintToken(t, priv, g))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	status, body = readInbox(keys.readPriv, inlineReadKid, "a4-inline-read")
	if status != http.StatusOK || !strings.Contains(body, `"subject":"from beta"`) || !strings.Contains(body, `"from":"beta/mayor"`) {
		t.Fatalf("inline read key inbox read = %d %q, want 200 with the sent mail", status, body)
	}
	if status, body = readInbox(keys.dirPriv, directoryKid, "a4-directory-read"); !isReadGateRejection(status, body) {
		t.Fatalf("directory key inbox read = %d %q, want the read gate's 403", status, body)
	}
}

// A8 (K7): read_auth_required with an inline read key starts as before; with a
// directory and no inline read key it starts with reads closed; with neither it
// refuses the start.
func TestHardenedReadAuthRequiredWithDirectory(t *testing.T) {
	keys := newDirectoryReadKeys(t)
	inlineRead := inlineReadKid + ":" + base64.StdEncoding.EncodeToString(keys.readPub)
	inlineWritePub, _ := mustKeypair(t)
	inlineWrite := "inline-write:" + base64.StdEncoding.EncodeToString(inlineWritePub)
	newMux := func() *SupervisorMux {
		return newTestSupervisorMux(t, map[string]*fakeState{directoryProbeCity: newFakeState(t)}).WithAnyHostAllowed()
	}
	const path = "/v0/city/" + directoryProbeCity + "/beads"

	t.Run("inline-read-key", func(t *testing.T) {
		h, err := NewHardenedAuth(newMux(), supervisor.Section{Hardened: supervisor.HardenedListener{
			Port: 8447, ReadAuthRequired: true, WriteAuthVerifyKey: inlineWrite, ReadAuthVerifyKey: inlineRead,
		}}, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		if status, body := serveHardenedRead(t, h, path, keys.readPriv, inlineReadKid, "a8-inline"); status != http.StatusOK {
			t.Fatalf("inline read key read = %d %q", status, body)
		}
		if status, body := serveHardenedRead(t, h, path, nil, "", ""); !isReadGateMissingGrant(status, body) {
			t.Fatalf("unsigned read = %d %q", status, body)
		}
	})
	t.Run("directory-no-inline-read-key", func(t *testing.T) {
		dir := t.TempDir()
		writeTrustedKey(t, dir, directoryKid, keys.dirPub)
		sm := newMux()
		h, err := NewHardenedAuth(sm, supervisor.Section{Hardened: supervisor.HardenedListener{
			Port: 8447, ReadAuthRequired: true, TrustedKeysDir: dir, TrustedKeysReloadInterval: "0",
		}}, t.Logf)
		if err != nil {
			t.Fatalf("required with a directory must start: %v", err)
		}
		assertReadsClosed(t, "required", h, sm, map[string]ed25519.PrivateKey{directoryKid: keys.dirPriv})
	})
	t.Run("neither", func(t *testing.T) {
		_, err := NewHardenedAuth(newMux(), supervisor.Section{Hardened: supervisor.HardenedListener{
			Port: 8447, ReadAuthRequired: true, WriteAuthVerifyKey: inlineWrite,
		}}, t.Logf)
		if err == nil || !strings.Contains(err.Error(), "read_auth_verify_key") {
			t.Fatalf("required with neither key nor directory: %v", err)
		}
	})
}
