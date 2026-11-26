package relay

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citywriteauth"
	"github.com/gastownhall/gascity/internal/mailrelay/envelope"
)

type fixture struct {
	t      *testing.T
	cfg    Config
	server *Server
	sign   map[string]ed25519.PrivateKey
	box    map[string]*ecdh.PrivateKey
	log    safeLog
}

type safeLog struct {
	sync.Mutex
	buf bytes.Buffer
}

func (l *safeLog) Write(b []byte) (int, error) {
	l.Lock()
	defer l.Unlock()
	return l.buf.Write(b)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, cfg: DefaultConfig(), sign: make(map[string]ed25519.PrivateKey), box: make(map[string]*ecdh.PrivateKey)}
	f.cfg.Hub = "hub"
	f.cfg.KeysDir = t.TempDir()
	f.cfg.StorePath = filepath.Join(t.TempDir(), "relay.db")
	for _, city := range []string{"hub", "alpha", "beta"} {
		f.addKey(city)
	}
	f.open()
	t.Cleanup(func() {
		if err := f.server.Close(); err != nil {
			t.Error(err)
		}
	})
	return f
}

func (f *fixture) open() {
	f.t.Helper()
	s, err := Open(f.cfg, &f.log)
	if err != nil {
		f.t.Fatal(err)
	}
	f.server = s
}

func (f *fixture) addKey(city string) {
	f.t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		f.t.Fatal(err)
	}
	f.sign[city] = priv
	if err := os.WriteFile(filepath.Join(f.cfg.KeysDir, city+".pub"), []byte(base64.StdEncoding.EncodeToString(pub)), 0o600); err != nil {
		f.t.Fatal(err)
	}
	f.box[city], err = envelope.GenerateKey()
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) statement(city, signer string, issued int64) []byte {
	f.t.Helper()
	s, err := envelope.SignStatement(city, "first", f.box[city].PublicKey(), issued, f.sign[signer])
	if err != nil {
		f.t.Fatal(err)
	}
	b, err := s.MarshalBinary()
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

func (f *fixture) letter(from, to, id string) []byte {
	f.t.Helper()
	label := envelope.Label{Version: envelope.Version, From: from, To: to, ID: id, Sent: time.Now().Unix()}
	mail := envelope.Mail{ID: id, From: from + "/writer", To: to + "/reader", Subject: "private subject sentinel", Body: "private body sentinel"}
	key := f.box[to]
	if key == nil {
		key = f.box["alpha"]
	}
	e, err := envelope.Seal(label, mail, f.sign[from], key.PublicKey())
	if err != nil {
		f.t.Fatal(err)
	}
	b, err := e.MarshalBinary()
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

func (f *fixture) token(city, method, path string, body []byte) string {
	f.t.Helper()
	u := httptest.NewRequest(method, path, nil).URL
	tok, err := envelope.MintGrant(f.sign[city], city, city, envelope.RequestFromURL(method, u, body), time.Minute)
	if err != nil {
		f.t.Fatal(err)
	}
	return tok
}

func (f *fixture) request(method, path string, body []byte, tok string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if tok != "" {
		req.Header.Set("X-GC-Relay-Grant", tok)
	}
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, req)
	return w
}

func (f *fixture) call(city, method, suffix string, body []byte, want int) []byte {
	f.t.Helper()
	path := "/v1/city/" + city + "/" + suffix
	w := f.request(method, path, body, f.token(city, method, path, body))
	if w.Code != want {
		f.t.Fatalf("%s %s: got %d, want %d", method, suffix, w.Code, want)
	}
	return w.Body.Bytes()
}

func (f *fixture) read(after int64) []Delivery {
	f.t.Helper()
	var batch []Delivery
	b := f.call("hub", "GET", fmt.Sprintf("queue?after=%d&wait=0s", after), nil, 200)
	if err := json.Unmarshal(b, &batch); err != nil || batch == nil {
		f.t.Fatalf("invalid batch: %v", err)
	}
	return batch
}

func TestExchange(t *testing.T) {
	f := newFixture(t)
	for _, city := range []string{"hub", "alpha"} {
		f.call(city, "PUT", "key", f.statement(city, city, 1), 204)
	}
	for city, peer := range map[string]string{"alpha": "hub", "hub": "alpha"} {
		if got := f.call(city, "GET", "keys/"+peer, nil, 200); !bytes.Equal(got, f.statement(peer, peer, 1)) {
			t.Fatal("key mismatch")
		}
	}
	wire := f.letter("alpha", "hub", "first")
	f.call("alpha", "POST", "envelopes", wire, 204)
	batch := f.read(0)
	if len(batch) != 1 || batch[0].Number != 1 || !bytes.Equal(batch[0].Envelope, wire) {
		t.Fatal("envelope mismatch")
	}
	if w := f.request("GET", "/healthz", nil, ""); w.Code != 200 || w.Body.String() != "ok" {
		t.Fatal("health response")
	}
	file, err := os.ReadFile(f.cfg.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	response, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range [][]byte{[]byte("private subject sentinel"), []byte("private body sentinel"), f.sign["alpha"], f.box["hub"].Bytes()} {
		for _, data := range [][]byte{file, f.log.buf.Bytes(), response} {
			if bytes.Contains(data, secret) {
				t.Fatal("private material appeared at relay")
			}
		}
	}
}

func TestPairsAndKeyStatements(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct {
		caller, from, to string
		want             int
	}{
		{"alpha", "alpha", "beta", 403},
		{"alpha", "alpha", "hub", 204},
		{"hub", "hub", "beta", 204},
		{"alpha", "alpha", "alpha", 403},
		{"alpha", "beta", "hub", 403},
		{"hub", "hub", "unknown", 403},
	} {
		f.call(c.caller, "POST", "envelopes", f.letter(c.from, c.to, c.from+"-"+c.to), c.want)
	}
	f.call("alpha", "GET", "keys/beta", nil, 403)
	f.call("alpha", "PUT", "key", f.statement("alpha", "alpha", 2), 204)
	f.call("alpha", "PUT", "key", f.statement("alpha", "alpha", 2), 204)
	f.call("alpha", "PUT", "key", f.statement("alpha", "alpha", 1), 409)
	f.call("alpha", "PUT", "key", f.statement("alpha", "beta", 3), 403)
	f.call("alpha", "PUT", "key", f.statement("beta", "beta", 3), 403)
}

func TestCursorAndDuplicateIDsAcrossRestart(t *testing.T) {
	f := newFixture(t)
	f.call("alpha", "POST", "envelopes", f.letter("alpha", "hub", "one"), 204)
	f.call("alpha", "POST", "envelopes", f.letter("alpha", "hub", "one"), 204)
	f.call("beta", "POST", "envelopes", f.letter("beta", "hub", "one"), 409)
	f.call("alpha", "POST", "envelopes", f.letter("alpha", "hub", "two"), 204)
	if len(f.read(0)) != 2 {
		t.Fatal("duplicate post")
	}
	if batch := f.read(1); len(batch) != 1 || batch[0].Number != 2 {
		t.Fatal("cursor failed")
	}
	if batch := f.read(0); len(batch) != 1 || batch[0].Number != 2 {
		t.Fatal("stale cursor")
	}
	path := "/v1/city/hub/queue?after=2&wait=0s"
	tok := f.token("hub", "GET", path, nil)
	if f.request("GET", path, nil, tok).Code != 200 {
		t.Fatal("first use failed")
	}
	if err := f.server.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	if f.request("GET", path, nil, tok).Code != 401 {
		t.Fatal("restart forgot spent grant")
	}
	f.call("alpha", "POST", "envelopes", f.letter("alpha", "hub", "one"), 204)
	f.call("alpha", "POST", "envelopes", f.letter("alpha", "hub", "three"), 204)
	if batch := f.read(0); len(batch) != 1 || batch[0].Number != 3 {
		t.Fatal("queue numbers reset")
	}
}

func TestTwoRelayProcessesShareSingleUse(t *testing.T) {
	f := newFixture(t)
	other, err := Open(f.cfg, &f.log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := other.Close(); err != nil {
			t.Error(err)
		}
	}()
	path := "/v1/city/alpha/envelopes"
	data := f.letter("alpha", "hub", "shared")
	token := f.token("alpha", "POST", path, data)
	codes := make(chan int, 2)
	for _, s := range []*Server{f.server, other} {
		go func() {
			r := httptest.NewRequest("POST", path, bytes.NewReader(data))
			r.Header.Set(grantHeader, token)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			codes <- w.Code
		}()
	}
	a, b := <-codes, <-codes
	if min(a, b) != 204 || max(a, b) != 401 || len(f.read(0)) != 1 {
		t.Fatalf("shared grant results %d, %d", a, b)
	}
}

func TestGrantExpiredBeforeStoreAdmission(t *testing.T) {
	f := newFixture(t)
	if err := f.server.store.Use("alpha/expired", time.Now().Add(-time.Minute)); err == nil {
		t.Fatal("grant recorded after its acceptance deadline")
	}
	var count int
	if err := f.server.store.db.QueryRow("SELECT count(*) FROM grants").Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired grant count %d, error %v", count, err)
	}
}

func TestBatchByteBoundAndCursor(t *testing.T) {
	f := newFixture(t)
	large := make([]byte, envelope.MaxMailSize)
	for i := range 12 {
		// The relay only interprets the public label. Opaque bytes count toward
		// its batch bound regardless of the recipient's ability to open them.
		env := envelope.Envelope{Label: envelope.Label{Version: envelope.Version, ID: fmt.Sprint(i), From: "alpha", To: "hub"}, Sealed: large}
		wire, err := env.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		f.call("alpha", "POST", "envelopes", wire, 204)
	}
	f.call("hub", "GET", "queue?after=1000&wait=0s", nil, 400)
	first := f.read(0)
	size := 0
	for _, d := range first {
		size += len(d.Envelope)
	}
	if size > batchBytes || len(first) == 0 || len(first) == 12 {
		t.Fatal("byte bound did not divide batch")
	}
	second := f.read(first[len(first)-1].Number)
	if len(second) == 0 || second[0].Number != first[len(first)-1].Number+1 {
		t.Fatal("byte limit skipped a delivery")
	}
}

func TestUniformAuthentication(t *testing.T) {
	f := newFixture(t)
	for _, route := range []struct{ method, suffix string }{{"PUT", "key"}, {"GET", "keys/beta"}, {"GET", "queue?wait=0s"}, {"POST", "envelopes"}} {
		path := "/v1/city/alpha/" + route.suffix
		now := time.Now().Unix()
		body := []byte("test")
		base := citywriteauth.Grant{Kid: "alpha", City: "alpha", Aud: envelope.GrantAudience, IAT: now, Exp: now + 60, JTI: "test", Req: citywriteauth.ReqDigest(route.method, strings.Split(path, "?")[0], httptest.NewRequest(route.method, path, nil).URL.RawQuery, body)}
		refused := f.request(route.method, path, body, "")
		if refused.Code != 401 {
			t.Fatal("unsigned request passed")
		}
		check := func(token string) {
			t.Helper()
			w := f.request(route.method, path, body, token)
			if w.Code != refused.Code || w.Body.String() != refused.Body.String() {
				t.Fatal("sign-in refusal differs")
			}
		}
		for _, edit := range []func(*citywriteauth.Grant){
			func(g *citywriteauth.Grant) { g.Kid = "beta" }, func(g *citywriteauth.Grant) { g.Kid = "unknown" },
			func(g *citywriteauth.Grant) { g.City = "beta" }, func(g *citywriteauth.Grant) { g.IAT -= 200; g.Exp -= 200 },
			func(g *citywriteauth.Grant) { g.Aud = "gc-city-write" }, func(g *citywriteauth.Grant) { g.Aud = "gc-city-write.v2" },
			func(g *citywriteauth.Grant) { g.Aud = "gc-city-read" }, func(g *citywriteauth.Grant) { g.JTI = strings.Repeat("x", 300) },
		} {
			g := base
			edit(&g)
			check(testGrant(t, g, f.sign["alpha"]))
		}
		check(testGrant(t, base, f.sign["beta"]))
		check(f.token("alpha", route.method, path, []byte("another body")))
		check(f.token("alpha", route.method, "/v1/city/alpha/other", body))
		check(f.token("alpha", route.method, "/v1/city/alpha/"+strings.Split(route.suffix, "?")[0]+"?extra=1", body))
		var count int
		if err := f.server.store.db.QueryRow("SELECT count(*) FROM grants").Scan(&count); err != nil || count != 0 {
			t.Fatalf("rejected grant recorded: %d, %v", count, err)
		}
		token := f.token("alpha", route.method, path, body)
		if w := f.request(route.method, path, body, token); w.Code == 401 {
			t.Fatal("valid grant was rejected")
		}
		check(token)
		if _, err := f.server.store.db.Exec("DELETE FROM grants"); err != nil {
			t.Fatal(err)
		}
	}
}

func testGrant(t *testing.T, g citywriteauth.Grant, k ed25519.PrivateKey) string {
	t.Helper()
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(k, b))
}

func TestRequestLimitsBeforeUse(t *testing.T) {
	f := newFixture(t)
	path := "/v1/city/alpha/envelopes"
	body := f.letter("alpha", "hub", "one")
	tok := f.token("alpha", "POST", path, body)
	if got := f.request("POST", path, make([]byte, envelope.MaxEnvelopeSize+1), tok).Code; got != 413 {
		t.Fatalf("oversized body: %d", got)
	}
	if w := f.request("POST", path, body, strings.Repeat("a", 9000)); w.Code != 401 {
		t.Fatal("token limit")
	}
	r := httptest.NewRequest("POST", path, bytes.NewReader(body))
	r.Header.Set("X-GC-Relay-Grant", tok)
	r.Header.Set("Extra", strings.Repeat("x", 65536))
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	if w.Code != 431 {
		t.Fatalf("header limit: %d", w.Code)
	}
	if got := f.request("POST", path, body, tok).Code; got != 204 {
		t.Fatal("oversized requests spent grant")
	}
	for _, p := range []string{"/v1/city/alpha/../hub/queue", "/v1/city/alpha/%2e%2e/hub/queue"} {
		if w := f.request("GET", p, nil, f.token("alpha", "GET", p, nil)); w.Code < 400 {
			t.Fatal("dot segment passed")
		}
	}
}

func TestReload(t *testing.T) {
	f := newFixture(t)
	pub := filepath.Join(f.cfg.KeysDir, "beta.pub")
	if err := os.Remove(pub); err != nil {
		t.Fatal(err)
	}
	f.server.refreshKeys()
	f.call("beta", "GET", "keys/hub", nil, 401)
	f.addKey("beta")
	f.server.refreshKeys()
	f.call("beta", "GET", "keys/hub", nil, 404)
	bad := filepath.Join(f.cfg.KeysDir, "broken.pub")
	if err := os.WriteFile(bad, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(pub); err != nil {
		t.Fatal(err)
	}
	before := f.log.buf.Len()
	f.server.refreshKeys()
	if lines := f.log.buf.Bytes()[before:]; bytes.Count(lines, []byte("\n")) != 1 || !bytes.Contains(lines, []byte("reload failed")) {
		t.Fatal("malformed reload must log one failure")
	}
	f.call("beta", "GET", "keys/hub", nil, 404)
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	if err := f.server.reloadKeys(); err != nil {
		t.Fatal(err)
	}
	f.call("beta", "GET", "keys/hub", nil, 401)
}

func TestRateBatchAndRetention(t *testing.T) {
	f := newFixture(t)
	f.server.config.PostsPerMinute = 2
	now := time.Now().UTC().Truncate(time.Minute)
	f.server.now = func() time.Time { return now }
	post := func(id string, want int) { f.call("alpha", "POST", "envelopes", f.letter("alpha", "hub", id), want) }
	post("first", 204)
	post("second", 204)
	post("third", 429)
	now = now.Add(time.Minute)
	post("third", 204)
	now = now.Add(48 * time.Hour)
	post("later", 204)
	now = now.Add(29 * 24 * time.Hour)
	if err := f.server.store.sweep(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if batch := f.read(0); len(batch) != 1 || batch[0].Number != 4 {
		t.Fatal("retention")
	}
	var count int
	if err := f.server.store.db.QueryRow("SELECT count(*) FROM labels").Scan(&count); err != nil || count != 1 {
		t.Fatalf("label retention: %d %v", count, err)
	}
	f.server.config.PostsPerMinute = 100
	for i := 0; i < batchCount; i++ {
		post(fmt.Sprint(i), 204)
	}
	if len(f.read(0)) != batchCount {
		t.Fatal("batch count")
	}
}

func TestHeldRead(t *testing.T) {
	f := newFixture(t)
	f.server.config.MaxWait = 15 * time.Millisecond
	start := time.Now()
	if got := string(f.call("hub", "GET", "queue?wait=900s", nil, 200)); strings.TrimSpace(got) != "[]" || time.Since(start) < f.server.config.MaxWait {
		t.Fatal("empty read deadline")
	}
	f.server.config.MaxWait = time.Minute
	path := "/v1/city/hub/queue?wait=25s"
	tok := f.token("hub", "GET", path, nil)
	answer := make(chan *httptest.ResponseRecorder, 1)
	go func() { answer <- f.request("GET", path, nil, tok) }()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	probe := time.NewTicker(time.Millisecond)
	defer probe.Stop()
	for {
		f.server.mu.Lock()
		waiting := f.server.waiting["hub"] != nil
		f.server.mu.Unlock()
		if waiting {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("read did not register")
		case <-probe.C:
		}
	}
	f.call("hub", "GET", "queue?wait=0s", nil, 409)
	f.call("alpha", "POST", "envelopes", f.letter("alpha", "hub", "wake"), 204)
	select {
	case got := <-answer:
		if got.Code != 200 || !strings.Contains(got.Body.String(), `"number":1`) {
			t.Fatal("read did not deliver")
		}
	case <-deadline.C:
		t.Fatal("post did not wake reader")
	}
}
