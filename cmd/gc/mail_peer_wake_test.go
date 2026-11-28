package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citywriteauth"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/supervisor"
)

var peerMailGrantSeq atomic.Int64

type peerMailAnswer struct {
	status   int
	id       string
	notified bool
}

// peerMailPost sends one mail to city beta. With a private key the request
// carries a write grant signed under kid; without one it carries no grant.
func peerMailPost(t *testing.T, h http.Handler, priv ed25519.PrivateKey, kid, to, from string) peerMailAnswer {
	t.Helper()
	const path = "/v0/city/beta/mail"
	body, err := json.Marshal(api.MailSendRequest{To: to, From: from, Subject: "update", Notify: true})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "http://beta.test"+path, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-GC-Request", "1")
	if priv != nil {
		now := time.Now()
		payload, err := json.Marshal(citywriteauth.Grant{
			Kid: kid, Aud: "gc-city-write.v2", City: "beta",
			IAT: now.Unix(), Exp: now.Add(30 * time.Second).Unix(),
			JTI: fmt.Sprintf("peer-mail-%d", peerMailGrantSeq.Add(1)),
			Req: citywriteauth.ReqDigest("POST", path, "", body),
		})
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("X-GC-City-Write", base64.RawURLEncoding.EncodeToString(payload)+"."+base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, payload)))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	answer := peerMailAnswer{status: w.Code}
	if w.Code != http.StatusCreated {
		return answer
	}
	var out struct {
		ID       string `json:"id"`
		Notified *bool  `json:"notified"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.ID == "" || out.Notified == nil {
		t.Fatalf("mail answer %s: %v", w.Body.String(), err)
	}
	answer.id, answer.notified = out.ID, *out.Notified
	return answer
}

// peerWakeCity is receiving city beta: its real controller state behind a
// hardened listener whose trusted-keys directory registers peer cities, and
// an ungated listener beside it.
type peerWakeCity struct {
	t        *testing.T
	dir      string
	cfg      *config.City
	store    beads.Store
	sp       *runtime.Fake
	state    *controllerState
	keysDir  string
	keys     map[string]ed25519.PrivateKey
	hardened *api.HardenedAuth
	ungated  http.Handler
}

func newPeerWakeCity(t *testing.T, store beads.Store, kids []string, agents ...config.Agent) *peerWakeCity {
	t.Helper()
	clearGCEnv(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	seedLocalCity(t)
	oldPoller := startNudgePoller
	startNudgePoller = func(string, string, string) error { return nil }
	t.Cleanup(func() { startNudgePoller = oldPoller })
	c := &peerWakeCity{
		t: t, dir: t.TempDir(), store: store, sp: runtime.NewFake(),
		keysDir: t.TempDir(), keys: make(map[string]ed25519.PrivateKey),
		cfg: &config.City{Workspace: config.Workspace{Name: "beta"}, Agents: agents},
	}
	c.cfg.Daemon.NudgeDispatcher = "supervisor"
	c.state = &controllerState{
		cityName: "beta", cityPath: c.dir, cfg: c.cfg, sp: c.sp,
		cityBeadStore: store, cityMailProv: beadmail.New(store), eventProv: events.NewFake(),
	}
	if err := os.Chmod(c.keysDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, kid := range kids {
		pub, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		c.keys[kid] = priv
		if err := os.WriteFile(filepath.Join(c.keysDir, kid+".pub"), []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mux := api.NewSupervisorMux(&singleCityStateResolver{c.state}, nil, false, "test", "", time.Now()).WithAllowedHosts([]string{"beta.test"})
	hardened, err := api.NewHardenedAuth(mux, supervisor.Section{Hardened: supervisor.HardenedListener{Port: 8447, TrustedKeysDir: c.keysDir}}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	c.hardened, c.ungated = hardened, mux.Handler()
	return c
}

// start creates a running, idle session of a configured agent.
func (c *peerWakeCity) start(template, alias string) session.Info {
	c.t.Helper()
	provider := "claude"
	if a := config.FindAgent(c.cfg, template); a != nil && a.Provider != "" {
		provider = a.Provider
	}
	mgr := newSessionManagerWithConfig(c.dir, c.store, c.sp, c.cfg)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template: template, Alias: alias, Title: template, Command: provider, WorkDir: c.dir, Provider: provider,
	})
	if err != nil {
		c.t.Fatal(err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: c.dir}); err != nil {
		c.t.Fatal(err)
	}
	c.sp.WaitForIdleErrors[info.SessionName] = nil
	return info
}

// keyed sends through the hardened listener with kid's registered key.
func (c *peerWakeCity) keyed(kid, to, from string) peerMailAnswer {
	c.t.Helper()
	return peerMailPost(c.t, c.hardened, c.keys[kid], kid, to, from)
}

// unkeyed sends through the ungated listener with no grant.
func (c *peerWakeCity) unkeyed(to, from string) peerMailAnswer {
	c.t.Helper()
	return peerMailPost(c.t, c.ungated, nil, "", to, from)
}

func (c *peerWakeCity) typed(info session.Info) []string {
	var messages []string
	for _, call := range c.sp.SnapshotCalls() {
		if call.Method == "NudgeNow" && call.Name == info.SessionName {
			messages = append(messages, call.Message)
		}
	}
	return messages
}

func (c *peerWakeCity) history(id string) []string {
	c.t.Helper()
	entries, err := sessionFrontDoor(c.store).MailWakeHistory(id)
	if err != nil {
		c.t.Fatal(err)
	}
	senders := make([]string, 0, len(entries))
	for _, entry := range entries {
		senders = append(senders, entry.Sender)
	}
	return senders
}

func (c *peerWakeCity) queued(store beads.Store) int {
	c.t.Helper()
	items, err := store.List(beads.ListQuery{Label: nudgeBeadLabel, TierMode: beads.TierBoth})
	if err != nil {
		c.t.Fatal(err)
	}
	return len(items)
}

// assertDeclined checks that a stored mail asked for no turn and left no trace
// on the recipient.
func (c *peerWakeCity) assertDeclined(what string, answer peerMailAnswer, recipient session.Info) {
	c.t.Helper()
	if answer.status != http.StatusCreated || answer.notified {
		c.t.Errorf("%s: status %d notified=%t, want the mail stored and no turn", what, answer.status, answer.notified)
	}
	if typed := c.typed(recipient); len(typed) != 0 {
		c.t.Errorf("%s: typed at the recipient: %q", what, typed)
	}
	if history := c.history(recipient.ID); len(history) != 0 {
		c.t.Errorf("%s: wake recorded under %q", what, history)
	}
	if n := c.queued(c.store); n != 0 {
		c.t.Errorf("%s: %d reminder(s) queued", what, n)
	}
}

// A recipient that lists a peer city is asked for a turn by that city's key,
// whatever sender the mail writes. The wake is recorded and announced under
// the key the receiver verified.
func TestPeerCityListedByKeyIsAsked(t *testing.T) {
	c := newPeerWakeCity(t, beads.NewMemStore(), []string{"alpha"},
		config.Agent{Name: "reader", Provider: "claude", StartCommand: "echo", MailWakeFrom: []string{"city:alpha"}},
		config.Agent{Name: "writer", Provider: "claude", StartCommand: "echo"},
	)
	reader, writer := c.start("reader", ""), c.start("writer", "")
	froms := []string{"alpha/mayor", "human", writer.ID}
	for i, from := range froms {
		answer := c.keyed("alpha", reader.ID, from)
		if answer.status != http.StatusCreated || !answer.notified {
			t.Fatalf("from %q: status %d notified=%t", from, answer.status, answer.notified)
		}
		stored, err := c.state.cityMailProv.Get(answer.id)
		if err != nil {
			t.Fatal(err)
		}
		if from != writer.ID && stored.From != from {
			t.Errorf("from %q: the return address was stored as %q", from, stored.From)
		}
		if typed := c.typed(reader); len(typed) != i+1 {
			t.Fatalf("from %q: %d reminder(s) typed, want %d", from, len(typed), i+1)
		}
	}
	for _, sender := range c.history(reader.ID) {
		if sender != "city:alpha" {
			t.Errorf("wake recorded under %q, want city:alpha", sender)
		}
	}
	if got := len(c.history(reader.ID)); got != len(froms) {
		t.Errorf("%d wake(s) recorded, want %d", got, len(froms))
	}
	for _, message := range c.typed(reader) {
		if !strings.Contains(message, "You have mail from city:alpha") {
			t.Errorf("reminder %q does not name the verified key", message)
		}
		for _, text := range []string{"alpha/mayor", "human", writer.ID, writer.SessionName} {
			if strings.Contains(message, text) {
				t.Errorf("reminder %q repeats the written sender %q", message, text)
			}
		}
	}
	if history := c.history(writer.ID); len(history) != 0 {
		t.Errorf("the local writer's own history changed: %q", history)
	}
}

// A registered key that writes "human" as its sender is not the local human.
func TestPeerCityCannotBeHuman(t *testing.T) {
	c := newPeerWakeCity(t, beads.NewMemStore(), []string{"alpha"},
		config.Agent{Name: "reader", Provider: "claude", StartCommand: "echo", MailWakeFrom: []string{"human"}},
	)
	reader := c.start("reader", "")
	c.assertDeclined("registered key writing human", c.keyed("alpha", reader.ID, "human"), reader)
	// A request that passed no gate is nobody, whatever it writes.
	c.assertDeclined("no key writing human", c.unkeyed(reader.ID, "human"), reader)
}

// A registered key that writes a listed local agent's address is not that agent.
func TestPeerCityCannotBeLocalAgent(t *testing.T) {
	c := newPeerWakeCity(t, beads.NewMemStore(), []string{"alpha"},
		config.Agent{Name: "reader", Provider: "claude", StartCommand: "echo", MailWakeFrom: []string{"writer"}},
		config.Agent{Name: "writer", Provider: "claude", StartCommand: "echo"},
	)
	reader, writer := c.start("reader", ""), c.start("writer", "scribe")
	for label, from := range map[string]string{"session id": writer.ID, "session name": writer.SessionName, "alias": "scribe"} {
		answer := c.keyed("alpha", reader.ID, from)
		c.assertDeclined("registered key writing the local writer's "+label, answer, reader)
		if answer.status != http.StatusCreated {
			continue
		}
		// The written address still resolves to the local session, so the
		// case is the one the list must refuse.
		stored, err := c.state.cityMailProv.Get(answer.id)
		if err != nil || stored.FromSessionID != writer.ID {
			t.Errorf("%s %q: stored mail bound to %q (%v), want the local writer %q", label, from, stored.FromSessionID, err, writer.ID)
		}
		c.assertDeclined("no key writing the local writer's "+label, c.unkeyed(reader.ID, from), reader)
	}
	if history := c.history(writer.ID); len(history) != 0 {
		t.Errorf("the local writer's own history changed: %q", history)
	}
}

// An entry names one registered key. Another registered key does not match it.
func TestPeerCityOtherRegisteredKeyIsNotListed(t *testing.T) {
	c := newPeerWakeCity(t, beads.NewMemStore(), []string{"alpha", "beta"},
		config.Agent{Name: "reader", Provider: "claude", StartCommand: "echo", MailWakeFrom: []string{"city:alpha"}},
	)
	reader := c.start("reader", "")
	for _, from := range []string{"beta/mayor", "alpha/mayor", "city:alpha", "human"} {
		c.assertDeclined("key beta writing "+from, c.keyed("beta", reader.ID, from), reader)
	}
	if answer := c.keyed("alpha", reader.ID, "alpha/mayor"); !answer.notified {
		t.Fatalf("the listed key was declined: %+v", answer)
	}
}

// The written sender never satisfies a peer entry: not through the API
// without a key, and not from a local command.
func TestPeerCityEntryIsNeverMatchedByText(t *testing.T) {
	c := newPeerWakeCity(t, beads.NewMemStore(), []string{"alpha"},
		config.Agent{Name: "reader", Provider: "claude", StartCommand: "echo", MailWakeFrom: []string{"city:alpha"}},
	)
	reader := c.start("reader", "")
	for _, from := range []string{"city:alpha", "alpha", "alpha/mayor"} {
		c.assertDeclined("no key writing "+from, c.unkeyed(reader.ID, from), reader)
	}

	build := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return c.sp, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = build })
	mp := c.state.cityMailProv
	for _, from := range []string{"city:alpha", "human"} {
		message, err := mp.Send(from, reader.ID, "update", "")
		if err != nil {
			t.Fatal(err)
		}
		var diagnostic bytes.Buffer
		wake := newLocalMailWake(c.dir, c.cfg, c.store, c.store, mp, from, "", "gc mail send", &diagnostic, time.Now)
		if wake != nil && wake(message) {
			t.Errorf("a local command sent as %q satisfied the peer entry", from)
		}
		if typed, history := c.typed(reader), c.history(reader.ID); len(typed) != 0 || len(history) != 0 {
			t.Errorf("local command sent as %q: typed %q, history %q", from, typed, history)
		}
	}
}

// The budget of three wakes belongs to the verified key, whatever the mails
// write, and a peer's wakes do not spend the local human's budget.
func TestPeerCityBudgetIsTheKeys(t *testing.T) {
	c := newPeerWakeCity(t, beads.NewMemStore(), []string{"alpha"},
		config.Agent{Name: "reader", Provider: "claude", StartCommand: "echo", MailWakeFrom: []string{"city:alpha", "human"}},
	)
	reader := c.start("reader", "")
	for _, from := range []string{"human", "alpha/mayor", "alpha/worker"} {
		if answer := c.keyed("alpha", reader.ID, from); !answer.notified {
			t.Fatalf("from %q inside the budget: %+v", from, answer)
		}
	}
	if answer := c.keyed("alpha", reader.ID, "alpha/another"); answer.status != http.StatusCreated || answer.notified {
		t.Fatalf("a fourth wake under one key: %+v", answer)
	}
	if typed := c.typed(reader); len(typed) != 3 {
		t.Fatalf("%d reminder(s) typed, want 3", len(typed))
	}
	for _, sender := range c.history(reader.ID) {
		if sender != "city:alpha" {
			t.Fatalf("wake recorded under %q, want city:alpha", sender)
		}
	}

	// The local operator's own three wakes are all still there.
	build := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return c.sp, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = build })
	mp := c.state.cityMailProv
	var diagnostic bytes.Buffer
	wake := newLocalMailWake(c.dir, c.cfg, c.store, c.store, mp, "human", "", "gc mail send", &diagnostic, time.Now)
	for i := range 3 {
		message, err := mp.Send("human", reader.ID, "from the local operator", "")
		if err != nil {
			t.Fatal(err)
		}
		if !wake(message) {
			t.Fatalf("local human wake %d declined after the peer's sends: %q", i+1, diagnostic.String())
		}
	}
	if typed := c.typed(reader); len(typed) != 6 {
		t.Errorf("%d reminder(s) typed, want 6", len(typed))
	}
}

// Removing a peer's key file refuses its mail at the gate. No list entry
// needs editing.
func TestPeerCityRevokedKeyIsRefused(t *testing.T) {
	c := newPeerWakeCity(t, beads.NewMemStore(), []string{"alpha"},
		config.Agent{Name: "reader", Provider: "claude", StartCommand: "echo", MailWakeFrom: []string{"city:alpha"}},
	)
	reader := c.start("reader", "")
	if answer := c.keyed("alpha", reader.ID, "alpha/mayor"); !answer.notified {
		t.Fatalf("the listed key was declined before the revocation: %+v", answer)
	}
	if err := os.Remove(filepath.Join(c.keysDir, "alpha.pub")); err != nil {
		t.Fatal(err)
	}
	if err := c.hardened.Reload(); err != nil {
		t.Fatal(err)
	}
	if answer := c.keyed("alpha", reader.ID, "alpha/mayor"); answer.status != http.StatusForbidden {
		t.Fatalf("revoked key: status %d", answer.status)
	}
	if typed, history := c.typed(reader), c.history(reader.ID); len(typed) != 1 || len(history) != 1 {
		t.Errorf("after the revocation: typed %q, history %q", typed, history)
	}
}
