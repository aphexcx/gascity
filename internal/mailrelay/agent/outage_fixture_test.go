package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/mailrelay/envelope"
	"github.com/gastownhall/gascity/internal/session"
)

// testClock is a clock only the agent's own waits move, so a test can run
// hours of back-off in an instant.
type testClock struct {
	mu      sync.Mutex
	start   time.Time
	now     time.Time
	onSleep func(elapsed time.Duration)
}

func newTestClock() *testClock {
	t0 := time.Now()
	return &testClock{start: t0, now: t0}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.now = c.now.Add(d)
	elapsed, on := c.now.Sub(c.start), c.onSleep
	c.mu.Unlock()
	if on != nil {
		on(elapsed)
	}
	return ctx.Err()
}

func (c *testClock) elapsed() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now.Sub(c.start)
}

// use makes the agent read and wait on this clock.
func (c *testClock) use(d Deps) Deps {
	d.Now, d.Sleep = c.Now, c.Sleep
	return d
}

var errSessionsDown = errors.New("session store unreachable")

// switchableSessions is a session store whose reads fail while it is down.
type switchableSessions struct {
	beads.Store
	down *atomic.Bool
}

func (s switchableSessions) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.down.Load() {
		return nil, errSessionsDown
	}
	return s.Store.List(q)
}

func (s switchableSessions) Get(id string) (beads.Bead, error) {
	if s.down.Load() {
		return beads.Bead{}, errSessionsDown
	}
	return s.Store.Get(id)
}

// sessionState is the fixture's city state with a session store behind the
// route, as every real city has.
type sessionState struct {
	*apiState
	sessions beads.Store
}

func (s *sessionState) SessionsBeadStore() beads.SessionStore {
	return beads.SessionStore{Store: s.sessions}
}

func (s *sessionState) CityState(name string) api.State {
	if name == s.name {
		return s
	}
	return nil
}

func (s *sessionState) ListCities() []api.CityInfo {
	return []api.CityInfo{{Name: s.name, Path: s.path, Running: true}}
}

// sessionCity is the city alpha's real mail API with a session store the
// test can take down. Each recipient is a live session under that alias.
type sessionCity struct {
	*cityAPI
	sessions *beads.MemStore
	down     *atomic.Bool
}

func newSessionCity(t *testing.T, recipients ...string) *sessionCity {
	t.Helper()
	mem := beads.NewMemStore()
	c := newCityAPIWith(t, "alpha", mem, beadmail.New(mem), recipients...)
	sc := &sessionCity{cityAPI: c, sessions: beads.NewMemStore(), down: &atomic.Bool{}}
	for _, r := range recipients {
		sc.addSession(r)
	}
	state := &sessionState{apiState: c.state, sessions: switchableSessions{Store: sc.sessions, down: sc.down}}
	c.handler = api.NewSupervisorMux(state, nil, false, "test", "", time.Now()).WithAnyHostAllowed().Handler()
	return sc
}

// addSession adds a live session under an alias.
func (c *sessionCity) addSession(alias string) {
	c.t.Helper()
	if _, err := c.sessions.Create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{
		"alias": alias, "template": alias, "session_name": alias + "-session", "state": "active",
	}}); err != nil {
		c.t.Fatal(err)
	}
}

// problemWith answers the way the city API reports a problem, with a detail.
func problemWith(status int, code, detail string) func(*http.Request) (*http.Response, error) {
	return func(r *http.Request) (*http.Response, error) {
		body := fmt.Sprintf(`{"code":%q,"status":%d,"title":"problem","detail":%q}`, code, status, detail)
		return &http.Response{
			StatusCode: status, Request: r, Body: io.NopCloser(strings.NewReader(body)),
			Header: http.Header{"Content-Type": {"application/problem+json"}},
		}, nil
	}
}

// mailTo is a mail to one address in the receiving city.
func mailTo(to string) envelope.Mail {
	return envelope.Mail{From: "writer", To: to, Subject: subjectSentinel, Body: bodySentinel}
}

// countDrops counts drop lines that name the mail id and the reason.
func countDrops(log *syncBuffer, id, reason string) int {
	n := 0
	for _, l := range log.lines("drop ") {
		if strings.Contains(l, fmt.Sprintf("id=%q", id)) && strings.Contains(l, "reason="+reason) {
			n++
		}
	}
	return n
}
