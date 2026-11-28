package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mailrelay/envelope"
)

// pair is a recipient city alpha with its peer hub, both with keys.
func pair(t *testing.T) (hub, alpha *cityFixture) {
	t.Helper()
	hub, alpha = newCityFixture(t, "hub"), newCityFixture(t, "alpha")
	alpha.pin(hub)
	hub.pin(alpha)
	hub.keygen()
	alpha.keygen()
	return hub, alpha
}

// A4: a mail the city filed before the agent stopped is filed once, whatever
// happens to the city's API before the agent comes back.
func TestFiledMailIsNeverFiledTwice(t *testing.T) {
	for _, tc := range []struct {
		name    string
		between func(c *cityAPI, filed beads.Bead)
	}{
		{"at once", nil},
		// Inside the bubble this timer runs on the fake clock.
		{"after the API window", func(*cityAPI, beads.Bead) { <-time.After(31 * time.Minute) }},
		{"after the API is recreated", func(c *cityAPI, _ beads.Bead) { c.recreate() }},
		{"after the mail is archived", func(c *cityAPI, filed beads.Bead) {
			if err := c.state.mail.Archive(filed.ID); err != nil {
				c.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				hub, alpha := pair(t)
				city := newCityAPI(t, "alpha", "reader")
				rel := newScriptedRelay()
				rel.add(1, seal(t, hub, alpha, "m-1", time.Now(), letter("reader")))

				// The city files the mail, then the agent stops before it hears so.
				ctx, cancel := context.WithCancel(context.Background())
				lost := roundTripFunc(func(r *http.Request) (*http.Response, error) {
					resp, err := city.transport().RoundTrip(r)
					if err == nil {
						_ = resp.Body.Close()
					}
					cancel()
					return nil, errors.New("connection reset by peer")
				})
				if err := Run(ctx, alpha.settings, alpha.deps(rel.transport(), lost)); err != nil {
					t.Fatal(err)
				}
				filed := city.messages()
				if len(filed) != 1 {
					t.Fatalf("first run filed %d mails", len(filed))
				}
				if tc.between != nil {
					tc.between(city, filed[0])
				}

				again := start(t, alpha.settings, alpha.deps(rel.transport(), city.transport()))
				rel.recorder.await(t, "the place to pass the mail", func() bool { return slices.Contains(rel.recorder.reads(), 1) })
				again.stop(t)
				if got := city.messages(); len(got) != 1 || got[0].ID != filed[0].ID {
					t.Fatalf("city holds %d mails after the second run", len(got))
				}
				posts := city.posts()
				if len(posts) != 2 || posts[0].header.Get("Idempotency-Key") == "" || posts[0].header.Get("Idempotency-Key") != posts[1].header.Get("Idempotency-Key") {
					t.Fatalf("the two filings did not carry one key: %d posts", len(posts))
				}
			})
		})
	}
}

// A4, the other stopping point: the agent remembered the mail but stopped
// before it saved the place. The city's API is not called again.
func TestRememberedMailIsSkippedAfterRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub, alpha := pair(t)
		city := newCityAPI(t, "alpha", "reader")
		rel := newScriptedRelay()
		rel.add(1, seal(t, hub, alpha, "m-1", time.Now(), letter("reader")))
		ctx, cancel := context.WithCancel(context.Background())
		alpha.gc.after = func([]string) { cancel() }
		if err := Run(ctx, alpha.settings, alpha.deps(rel.transport(), city.transport())); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(alpha.settings.StateDir, placeFile)); !os.IsNotExist(err) {
			t.Fatalf("the first run saved the place: %v", err)
		}
		if _, err := os.Stat(filepath.Join(alpha.settings.StateDir, seenFile)); err != nil {
			t.Fatalf("the first run did not remember the mail: %v", err)
		}
		alpha.gc.after = nil
		again := start(t, alpha.settings, alpha.deps(rel.transport(), city.transport()))
		rel.recorder.await(t, "the place to pass the mail", func() bool { return slices.Contains(rel.recorder.reads(), 1) })
		again.stop(t)
		if n := len(city.posts()); n != 1 {
			t.Fatalf("city API called %d times", n)
		}
		if n := len(city.messages()); n != 1 {
			t.Fatalf("city holds %d mails", n)
		}
	})
}

// A5: with the city's API down nothing moves; when it is back each waiting
// mail is filed once, in order.
func TestNothingIsLostWhileCityAPIIsDown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub, alpha := pair(t)
		city := newCityAPI(t, "alpha", "reader")
		rel := newScriptedRelay()
		for i := int64(1); i <= 3; i++ {
			m := letter("reader")
			m.Subject = "mail " + strconv.FormatInt(i, 10)
			rel.add(i, seal(t, hub, alpha, "m-"+strconv.FormatInt(i, 10), time.Now(), m))
		}
		var down atomic.Bool
		down.Store(true)
		apiTransport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if down.Load() {
				return nil, refused()
			}
			return city.transport().RoundTrip(r)
		})
		deps := alpha.deps(rel.transport(), apiTransport)
		deps.Sleep = nil
		run := start(t, alpha.settings, deps)
		rel.recorder.await(t, "three asks while the API is down", func() bool { return len(rel.recorder.reads()) >= 3 })
		for _, after := range rel.recorder.reads() {
			if after != 0 {
				t.Fatalf("the place moved to %d while the API was down", after)
			}
		}
		down.Store(false)
		rel.recorder.await(t, "the place to pass all three", func() bool { return slices.Contains(rel.recorder.reads(), 3) })
		run.stop(t)
		inbox := city.inbox("reader")
		slices.SortFunc(inbox, func(a, b mail.Message) int { return a.CreatedAt.Compare(b.CreatedAt) })
		if len(inbox) != 3 || inbox[0].Subject != "mail 1" || inbox[1].Subject != "mail 2" || inbox[2].Subject != "mail 3" {
			t.Fatalf("filed %d mails out of order", len(inbox))
		}
		if posts := city.posts(); len(posts) != 3 {
			t.Fatalf("the API was called %d times once back", len(posts))
		}
	})
}

// sleeps records each back-off wait the agent asks for, without waiting.
type sleeps struct {
	mu   sync.Mutex
	list []time.Duration
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.list = append(s.list, d)
	s.mu.Unlock()
	return ctx.Err()
}

func (s *sleeps) all() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.list...)
}

// A6: a read the relay never answers is given up at the limit and asked
// again from the same place.
func TestUnansweredReadIsGivenUpAtTheLimit(t *testing.T) {
	hub, alpha := pair(t)
	rel := newRelayFixture(t, map[string]ed25519.PublicKey{"hub": hub.public(), "alpha": alpha.public()})
	city := newCityAPI(t, "alpha", "reader")
	var hung atomic.Bool
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/queue") && hung.CompareAndSwap(false, true) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return rel.transport.RoundTrip(r)
	})
	waits := &sleeps{}
	alpha.settings.RequestLimit = 100 * time.Millisecond
	alpha.settings.Wait = 50 * time.Millisecond
	deps := alpha.deps(rel.recorder.wrap(transport), city.transport())
	deps.Sleep = waits.sleep
	run := start(t, alpha.settings, deps)
	rel.recorder.await(t, "a second ask", func() bool { return len(rel.recorder.reads()) >= 2 })
	run.stop(t)
	if reads := rel.recorder.reads(); reads[0] != 0 || reads[1] != 0 {
		t.Fatalf("asked again from %v", reads)
	}
	first := alpha.log.lines("ask ")[0]
	if !strings.Contains(first, "outcome=cut") || !strings.Contains(first, "reason=limit") {
		t.Fatalf("the hung read was logged as %q", first)
	}
	if w := waits.all(); len(w) == 0 || w[0] != time.Second {
		t.Fatalf("back-off after the cut read: %v", w)
	}
}

// A6: an unreachable relay is retried with the back-off, doubling from one
// second to sixty, and the mail that waited arrives in order afterwards.
func TestUnreachableRelayIsRetriedWithBackoff(t *testing.T) {
	hub, alpha := pair(t)
	rel := newRelayFixture(t, map[string]ed25519.PublicKey{"hub": hub.public(), "alpha": alpha.public()})
	city := newCityAPI(t, "alpha", "reader")
	for i := 1; i <= 3; i++ {
		m := letter("reader")
		m.Subject = "mail " + strconv.Itoa(i)
		if code := rel.post("hub", hub.signer, seal(t, hub, alpha, "m-"+strconv.Itoa(i), time.Now(), m)); code != http.StatusNoContent {
			t.Fatalf("posting mail %d: %d", i, code)
		}
	}
	var failures atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if failures.Add(1) <= 8 {
			return nil, refused()
		}
		return rel.transport.RoundTrip(r)
	})
	waits := &sleeps{}
	deps := alpha.deps(transport, city.transport())
	deps.Sleep = waits.sleep
	run := start(t, alpha.settings, deps)
	city.recorder.await(t, "three filings", func() bool { return len(city.inbox("reader")) == 3 })
	run.stop(t)
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60}
	got := waits.all()
	if len(got) < len(want) {
		t.Fatalf("back-off waits %v", got)
	}
	for i, w := range want {
		if got[i] != w*time.Second {
			t.Fatalf("back-off waits %v, want %v seconds", got[:len(want)], want)
		}
	}
	inbox := city.inbox("reader")
	slices.SortFunc(inbox, func(a, b mail.Message) int { return a.CreatedAt.Compare(b.CreatedAt) })
	for i, m := range inbox {
		if m.Subject != "mail "+strconv.Itoa(i+1) {
			t.Fatalf("mail %d filed as %q", i+1, m.Subject)
		}
	}
}

// A7: each bad mail is dropped with its own logged reason and the next good
// mail is still filed.
func TestBadMailIsDroppedAndTheLoopGoesOn(t *testing.T) {
	hub, alpha := pair(t)
	beta := newCityFixture(t, "beta")
	beta.keygen()
	rel := newRelayFixture(t, map[string]ed25519.PublicKey{"hub": hub.public(), "alpha": alpha.public(), "beta": beta.public()})
	city := newCityAPI(t, "hub", "writer")
	now := time.Now()
	post := func(from *cityFixture, wire []byte) {
		t.Helper()
		if code := rel.post(from.id, from.signer, wire); code != http.StatusNoContent {
			t.Fatalf("relay refused a test mail from %s: %d", from.id, code)
		}
	}
	post(beta, seal(t, beta, hub, "not-a-peer", now, letter("writer")))

	impostor := *alpha
	_, impostor.signer, _ = ed25519.GenerateKey(rand.Reader)
	post(alpha, seal(t, &impostor, hub, "bad-signature", now, letter("writer")))

	elsewhere := *hub
	elsewhere.boxKey = beta.boxKey
	post(alpha, seal(t, alpha, &elsewhere, "sealed-for-beta", now, letter("writer")))

	label := envelope.Label{Version: envelope.Version, ID: "changed-label", From: "alpha", To: "hub", Sent: now.Unix()}
	m := letter("writer")
	m.ID = label.ID
	env, err := envelope.Seal(label, m, alpha.signer, hub.boxKey)
	if err != nil {
		t.Fatal(err)
	}
	env.Label.Sent++
	changed, err := env.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	post(alpha, changed)

	post(alpha, seal(t, alpha, hub, "too-old", now.Add(-31*24*time.Hour), letter("writer")))
	good := letter("writer")
	good.Subject = "the good one"
	post(alpha, seal(t, alpha, hub, "good", now, good))

	run := start(t, hub.settings, hub.deps(rel.transport, city.transport()))
	city.recorder.await(t, "the good mail", func() bool { return len(city.inbox("writer")) == 1 })
	run.stop(t)
	if got := city.inbox("writer")[0]; got.Subject != "the good one" || got.From != "alpha/writer" {
		t.Fatalf("filed %+v", got)
	}
	drops := hub.log.lines("drop ")
	for id, reason := range map[string]string{
		"not-a-peer":      "reason=not-a-peer",
		"bad-signature":   "reason=bad-signature",
		"sealed-for-beta": "reason=not-opened",
		"changed-label":   "reason=not-opened",
		"too-old":         "reason=outside-window",
	} {
		if !slices.ContainsFunc(drops, func(l string) bool { return strings.Contains(l, strconv.Quote(id)) && strings.Contains(l, reason) }) {
			t.Fatalf("no drop line for %s with %s in %q", id, reason, drops)
		}
	}
	if len(city.posts()) != 1 {
		t.Fatalf("the city API saw %d filings", len(city.posts()))
	}
}

// A8: the same envelope delivered again under a new number is skipped.
func TestSeenMailIsNotFiledAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub, alpha := pair(t)
		city := newCityAPI(t, "alpha", "reader")
		rel := newScriptedRelay()
		wire := seal(t, hub, alpha, "m-1", time.Now(), letter("reader"))
		rel.add(1, wire)
		run := start(t, alpha.settings, alpha.deps(rel.transport(), city.transport()))
		rel.recorder.await(t, "the first mail", func() bool { return slices.Contains(rel.recorder.reads(), 1) })
		rel.add(2, wire)
		rel.recorder.await(t, "the copy", func() bool { return slices.Contains(rel.recorder.reads(), 2) })
		run.stop(t)
		if n := len(city.posts()); n != 1 {
			t.Fatalf("filed %d times", n)
		}
	})
}

// A10: a running recipient is nudged; a stopped one gets a notice with
// --notify; a failing gc leaves the mail filed and the place moving.
func TestTellTheRecipient(t *testing.T) {
	for _, tc := range []struct {
		name            string
		running, broken bool
		nudge, notice   bool
		failureLogLines int
	}{
		{name: "running", running: true, nudge: true},
		{name: "stopped", nudge: true, notice: true},
		{name: "gc failing", broken: true, nudge: true, notice: true, failureLogLines: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				hub, alpha := pair(t)
				alpha.gc.running, alpha.gc.broken = tc.running, tc.broken
				city := newCityAPI(t, "alpha", "reader")
				rel := newScriptedRelay()
				rel.add(1, seal(t, hub, alpha, "m-1", time.Now(), letter("reader")))
				run := start(t, alpha.settings, alpha.deps(rel.transport(), city.transport()))
				rel.recorder.await(t, "the place to move", func() bool { return slices.Contains(rel.recorder.reads(), 1) })
				run.stop(t)
				if len(city.inbox("reader")) != 1 {
					t.Fatal("the mail is not filed")
				}
				filed := city.inbox("reader")[0]
				cmds := alpha.gc.commands()
				nudged := slices.ContainsFunc(cmds, func(c []string) bool {
					return isNudge(c, "reader") && strings.Contains(c[len(c)-1], "hub/writer") && strings.Contains(c[len(c)-1], subjectSentinel)
				})
				noticed := slices.ContainsFunc(cmds, func(c []string) bool {
					return len(c) > 2 && c[0] == "mail" && c[1] == "send" && slices.Contains(c, "--notify") &&
						slices.Contains(c, "--to=reader") && slices.Contains(c, "--from=human") &&
						slices.ContainsFunc(c, func(a string) bool {
							return strings.HasPrefix(a, "--subject=Relay mail from hub/writer") && strings.Contains(a, "hub/writer") && strings.Contains(a, subjectSentinel) && strings.Contains(a, filed.ID)
						})
				})
				if nudged != tc.nudge || noticed != tc.notice {
					t.Fatalf("nudged=%v noticed=%v, commands %q", nudged, noticed, cmds)
				}
				if got := len(alpha.log.lines("tell-failed ")); got != tc.failureLogLines {
					t.Fatalf("%d tell failure lines", got)
				}
			})
		})
	}
}

// A11: a recipient the city does not have is dropped at its first refusal,
// logged, and passed.
func TestRefusedRecipientIsDropped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub, alpha := pair(t)
		city := newSessionCity(t, "reader")
		rel := newScriptedRelay()
		rel.add(1, seal(t, hub, alpha, "nobody-1", time.Now(), letter("nobody")))
		rel.add(2, seal(t, hub, alpha, "reader-1", time.Now(), letter("reader")))
		deps := alpha.deps(rel.transport(), city.transport())
		deps.Sleep = nil
		run := start(t, alpha.settings, deps)
		rel.recorder.await(t, "both places", func() bool { return slices.Contains(rel.recorder.reads(), 2) })
		run.stop(t)
		if n := len(city.messages()); n != 1 {
			t.Fatalf("city holds %d mails", n)
		}
		drops := alpha.log.lines("drop ")
		if len(drops) != 1 || !strings.Contains(drops[0], `"nobody-1"`) || !strings.Contains(drops[0], "reason=refused") {
			t.Fatalf("drop lines %q", drops)
		}
		if n := len(city.posts()); n != 2 {
			t.Fatalf("the refusal was asked %d times before the drop", n-1)
		}
	})
}

// A 400 the city gives while its own store is briefly unreachable is not a
// refusal of the mail: it is asked again, and the mail is filed.
func TestShortLivedRefusalIsRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub, alpha := pair(t)
		city := newCityAPI(t, "alpha", "reader")
		rel := newScriptedRelay()
		rel.add(1, seal(t, hub, alpha, "m-1", time.Now(), letter("reader")))
		var refusals atomic.Int32
		apiTransport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if refusals.Add(1) <= 3 {
				return problem(r, http.StatusBadRequest, "invalid-request"), nil
			}
			return city.transport().RoundTrip(r)
		})
		deps := alpha.deps(rel.transport(), apiTransport)
		deps.Sleep = nil
		run := start(t, alpha.settings, deps)
		rel.recorder.await(t, "the place to move", func() bool { return slices.Contains(rel.recorder.reads(), 1) })
		run.stop(t)
		if n := len(city.inbox("reader")); n != 1 {
			t.Fatalf("filed %d mails after a short refusal", n)
		}
		if drops := alpha.log.lines("drop "); len(drops) != 0 {
			t.Fatalf("dropped: %q", drops)
		}
	})
}

// A peer pinned while the agent runs is trusted without a restart.
func TestPeerPinnedWhileRunningIsAccepted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub, alpha := newCityFixture(t, "hub"), newCityFixture(t, "alpha")
		hub.keygen()
		alpha.keygen()
		city := newCityAPI(t, "alpha", "reader")
		rel := newScriptedRelay()
		run := start(t, alpha.settings, alpha.deps(rel.transport(), city.transport()))
		rel.recorder.await(t, "the first ask", func() bool { return len(rel.recorder.reads()) > 0 })
		alpha.pin(hub)
		rel.add(1, seal(t, hub, alpha, "m-1", time.Now(), letter("reader")))
		rel.recorder.await(t, "the place to move", func() bool { return slices.Contains(rel.recorder.reads(), 1) })
		run.stop(t)
		if n := len(city.inbox("reader")); n != 1 {
			t.Fatalf("filed %d mails from the new peer: %q", n, alpha.log.lines("drop "))
		}
	})
}

// A place above the relay's highest number, as after the relay lost its
// store, starts over from zero instead of waiting for the relay's counter to
// pass it and delete mail unread.
func TestPlaceAboveRelayCounterStartsOver(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub, alpha := pair(t)
		city := newCityAPI(t, "alpha", "reader")
		if err := os.MkdirAll(alpha.settings.StateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(alpha.settings.StateDir, placeFile), []byte(`{"place":10}`), 0o600); err != nil {
			t.Fatal(err)
		}
		rel := newScriptedRelay()
		rel.add(1, seal(t, hub, alpha, "m-1", time.Now(), letter("reader")))
		rel.add(2, seal(t, hub, alpha, "m-2", time.Now(), letter("reader")))
		run := start(t, alpha.settings, alpha.deps(rel.transport(), city.transport()))
		rel.recorder.await(t, "the place to reach 2", func() bool { return slices.Contains(rel.recorder.reads(), 2) })
		run.stop(t)
		if reads := rel.recorder.reads(); reads[0] != 10 || reads[1] != 0 {
			t.Fatalf("reads %v", reads)
		}
		if n := len(city.inbox("reader")); n != 2 {
			t.Fatalf("filed %d of two mails", n)
		}
		if n := len(alpha.log.lines("place-reset ")); n != 1 {
			t.Fatalf("%d place-reset lines", n)
		}
	})
}

// A 201 whose body cannot be read is still a filing: the place moves.
func TestFilingAnsweredWithUnreadableBodyIsFiled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub, alpha := pair(t)
		rel := newScriptedRelay()
		rel.add(1, seal(t, hub, alpha, "m-1", time.Now(), letter("reader")))
		posts := newRecorder()
		apiTransport := posts.wrap(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusCreated, Request: r, Body: io.NopCloser(strings.NewReader("{not json")),
				Header: http.Header{"Content-Type": {"application/json"}},
			}, nil
		}))
		run := start(t, alpha.settings, alpha.deps(rel.transport(), apiTransport))
		rel.recorder.await(t, "the place to move", func() bool { return slices.Contains(rel.recorder.reads(), 1) })
		run.stop(t)
		if n := len(posts.all()); n != 1 {
			t.Fatalf("filed %d times", n)
		}
	})
}

// Two sender cities may choose the same mail id; each mail is filed.
func TestSameIDFromTwoSendersFilesBoth(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub, alpha := pair(t)
		beta := newCityFixture(t, "beta")
		hub.pin(beta)
		city := newCityAPI(t, "hub", "writer")
		rel := newScriptedRelay()
		rel.add(1, seal(t, alpha, hub, "same", time.Now(), letter("writer")))
		rel.add(2, seal(t, beta, hub, "same", time.Now(), letter("writer")))
		run := start(t, hub.settings, hub.deps(rel.transport(), city.transport()))
		rel.recorder.await(t, "both mails", func() bool { return slices.Contains(rel.recorder.reads(), 2) })
		run.stop(t)
		if n := len(city.inbox("writer")); n != 2 {
			t.Fatalf("filed %d of two mails", n)
		}
		posts := city.posts()
		if posts[0].header.Get("Idempotency-Key") == posts[1].header.Get("Idempotency-Key") {
			t.Fatal("two sender cities produced one filing key")
		}
	})
}

func TestFilingKeyNeverJoinsTwoPairs(t *testing.T) {
	pairs := [][2]string{{"ab", "c"}, {"a", "bc"}, {"a", "b/c"}, {"a-b", "c"}, {"a", "-bc"}, {"hub", "x"}, {"hub", "x\x00"}}
	seen := map[string][2]string{}
	for _, p := range pairs {
		k := filingKey(p[0], p[1])
		if prev, dup := seen[k]; dup {
			t.Fatalf("%q and %q share the key %q", prev, p, k)
		}
		seen[k] = p
		if strings.ContainsFunc(k, func(r rune) bool { return r < 0x21 || r > 0x7e }) {
			t.Fatalf("key %q is not a plain header value", k)
		}
	}
}

// The city API's answers: what moves the place and what is retried.
func TestCityAPIAnswers(t *testing.T) {
	answer := func(code int) func(*http.Request) (*http.Response, error) {
		return func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: code, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
		}
	}
	for _, tc := range []struct {
		name string
		// api answers the filing; nil uses the real city API.
		api       func(c *cityAPI) func(*http.Request) (*http.Response, error)
		moves     bool
		filedMail int
	}{
		{name: "201 filed", moves: true, filedMail: 1},
		{name: "501 no durable filing", api: func(c *cityAPI) func(*http.Request) (*http.Response, error) {
			fake := newCityAPIWith(c.t, "alpha", nil, mail.NewFake(), "reader")
			return fake.transport().RoundTrip
		}},
		{name: "503", api: func(*cityAPI) func(*http.Request) (*http.Response, error) { return answer(503) }},
		{name: "timeout", api: func(*cityAPI) func(*http.Request) (*http.Response, error) {
			return func(r *http.Request) (*http.Response, error) {
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
		}},
		{name: "refused connection", api: func(*cityAPI) func(*http.Request) (*http.Response, error) {
			return func(*http.Request) (*http.Response, error) { return nil, refused() }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				hub, alpha := pair(t)
				city := newCityAPI(t, "alpha", "reader")
				rel := newScriptedRelay()
				rel.add(1, seal(t, hub, alpha, "m-1", time.Now(), letter("reader")))
				keys := newRecorder()
				transport := city.transport()
				if tc.api != nil {
					transport = roundTripFunc(tc.api(city))
				}
				deps := alpha.deps(rel.transport(), keys.wrap(transport))
				deps.Sleep = nil
				run := start(t, alpha.settings, deps)
				if tc.moves {
					rel.recorder.await(t, "the place to move", func() bool { return slices.Contains(rel.recorder.reads(), 1) })
				} else {
					rel.recorder.await(t, "three asks", func() bool { return len(rel.recorder.reads()) >= 3 })
				}
				run.stop(t)
				if !tc.moves && slices.Contains(rel.recorder.reads(), 1) {
					t.Fatal("the place moved past an unfiled mail")
				}
				if n := len(city.messages()); n != tc.filedMail {
					t.Fatalf("city holds %d mails", n)
				}
				for _, p := range keys.all() {
					if p.header.Get("Idempotency-Key") == "" {
						t.Fatal("a filing went out without its key")
					}
				}
			})
		})
	}
}

// A reuse of an id with other content is reported once, filed never, and
// passed like a seen id.
func TestReusedIDWithOtherContentIsReportedOnce(t *testing.T) {
	hub, alpha := pair(t)
	city := newCityAPI(t, "alpha", "reader")
	rel := newScriptedRelay()
	rel.add(1, seal(t, hub, alpha, "m-1", time.Now(), letter("reader")))
	run := start(t, alpha.settings, alpha.deps(rel.transport(), city.transport()))
	rel.recorder.await(t, "the first filing", func() bool { return slices.Contains(rel.recorder.reads(), 1) })
	run.stop(t)

	// The agent loses its list of pairs seen; the place survives.
	if err := os.Remove(filepath.Join(alpha.settings.StateDir, seenFile)); err != nil {
		t.Fatal(err)
	}
	other := letter("reader")
	other.Body = "different content"
	reused := seal(t, hub, alpha, "m-1", time.Now(), other)
	rel.add(2, reused)
	alpha.settings.Socket = shortSocket(t)
	run = start(t, alpha.settings, alpha.deps(rel.transport(), city.transport()))
	rel.recorder.await(t, "the reused id", func() bool { return slices.Contains(rel.recorder.reads(), 2) })
	rel.add(3, reused)
	rel.recorder.await(t, "its copy", func() bool { return slices.Contains(rel.recorder.reads(), 3) })
	status := health(t, alpha.settings.Socket)
	run.stop(t)

	if n := len(city.messages()); n != 1 {
		t.Fatalf("city holds %d mails", n)
	}
	if n := len(city.posts()); n != 2 {
		t.Fatalf("city API called %d times", n)
	}
	if n := len(alpha.log.lines("fault kind=mismatch")); n != 1 {
		t.Fatalf("%d mismatch lines", n)
	}
	if n := countFaults(status.Faults, "mismatch"); n != 1 {
		t.Fatalf("status lists %d mismatch faults", n)
	}
}

func countFaults(faults []Fault, kind string) int {
	n := 0
	for _, f := range faults {
		if f.Kind == kind {
			n++
		}
	}
	return n
}

// shortSocket returns a socket path short enough for every platform.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

// health asks the agent's health path over its Unix socket.
func health(t *testing.T, socket string) Status {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}}
	resp, err := client.Get("http://agent/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health answered %d", resp.StatusCode)
	}
	var s Status
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// A13: given a socket path by the service environment, the agent answers its
// health path there.
func TestHealthOnServiceSocket(t *testing.T) {
	hub, alpha := pair(t)
	socket := shortSocket(t)
	env := map[string]string{
		"GC_SERVICE_SOCKET":      socket,
		"GC_SERVICE_STATE_ROOT":  alpha.settings.StateDir,
		"GC_SERVICE_SECRETS_DIR": alpha.settings.EncryptionKeysDir,
	}
	s := DefaultSettings(func(k string) string { return env[k] })
	if s.Socket != socket || s.StateDir != alpha.settings.StateDir || s.EncryptionKeysDir != alpha.settings.EncryptionKeysDir {
		t.Fatalf("service defaults not taken: %+v", s)
	}
	settings := alpha.settings
	settings.Socket = s.Socket
	rel := newScriptedRelay()
	rel.add(1, seal(t, hub, alpha, "m-1", time.Now(), letter("reader")))
	city := newCityAPI(t, "alpha", "reader")
	run := start(t, settings, alpha.deps(rel.transport(), city.transport()))
	rel.recorder.await(t, "the first ask", func() bool { return len(rel.recorder.reads()) > 0 })
	status := health(t, socket)
	run.stop(t)
	if status.City != "alpha" {
		t.Fatalf("health status %+v", status)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
}

// A13: the agent's code opens no listener but the Unix socket.
func TestAgentListensOnlyOnUnixSockets(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	calls := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !strings.HasPrefix(sel.Sel.Name, "Listen") {
				return true
			}
			calls++
			lit, ok := call.Args[0].(*ast.BasicLit)
			if sel.Sel.Name != "Listen" || !ok || lit.Value != `"unix"` {
				t.Errorf("%s: %s opens a listener other than a Unix socket", fset.Position(call.Pos()), sel.Sel.Name)
			}
			return true
		})
	}
	if calls != 1 {
		t.Fatalf("found %d listener calls, want the one Unix socket", calls)
	}
}
