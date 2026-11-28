package agent

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// The city's session store is down for a while and its API still answers.
// No mail waiting meanwhile is lost: each is filed once the store is back.
func TestCityStoreOutageLosesNoMail(t *testing.T) {
	t.Run("store down five minutes", func(t *testing.T) { storeOutage(t, 5*time.Minute) })
	t.Run("store never down", func(t *testing.T) { storeOutage(t, 0) })
}

func storeOutage(t *testing.T, outage time.Duration) {
	hub, alpha := pair(t)
	rel := newScriptedRelay()
	clock := newTestClock()
	for i := 1; i <= 3; i++ {
		rel.add(int64(i), seal(t, hub, alpha, fmt.Sprintf("m-%d", i), clock.Now(), mailTo("reader")))
	}
	city := newSessionCity(t, "reader")
	city.down.Store(outage > 0)
	clock.onSleep = func(elapsed time.Duration) {
		if elapsed >= outage {
			city.down.Store(false)
		}
	}
	run := start(t, alpha.settings, clock.use(alpha.deps(rel.transport(), city.transport())))
	rel.recorder.await(t, "the place to pass all three", func() bool { return slices.Contains(rel.recorder.reads(), 3) })
	run.stop(t)
	if n := len(city.inbox("reader")); n != 3 {
		t.Fatalf("%d of 3 mails filed after a store outage of %s; drops %q", n, outage, alpha.log.lines("drop "))
	}
	if outage > 0 && clock.elapsed() < outage {
		t.Fatalf("the place passed the mail %s into an outage of %s", clock.elapsed(), outage)
	}
}

// Mail to a recipient the city does not have is passed at once, with one log
// line each, and the good mail behind it is filed in the same batch.
func TestMailToMissingRecipientIsPassedAtOnce(t *testing.T) {
	hub, alpha := pair(t)
	rel := newScriptedRelay()
	clock := newTestClock()
	city := newSessionCity(t, "reader")
	for i := 1; i <= 3; i++ {
		rel.add(int64(i), seal(t, hub, alpha, fmt.Sprintf("bad-%d", i), clock.Now(), mailTo("nobody")))
	}
	rel.add(4, seal(t, hub, alpha, "good-1", clock.Now(), mailTo("reader")))
	run := start(t, alpha.settings, clock.use(alpha.deps(rel.transport(), city.transport())))
	rel.recorder.await(t, "the place to pass all four", func() bool { return slices.Contains(rel.recorder.reads(), 4) })
	run.stop(t)
	if len(city.inbox("reader")) != 1 {
		t.Fatalf("inbox holds %d mails", len(city.inbox("reader")))
	}
	// One batch, each mail asked once: the good mail never waited.
	if reads := rel.recorder.reads(); reads[0] != 0 || reads[1] != 4 {
		t.Fatalf("the four mails took more than one batch: reads %v", reads)
	}
	if n := len(city.posts()); n != 4 {
		t.Fatalf("the city API was asked %d times for four mails", n)
	}
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("bad-%d", i)
		var lines []string
		for _, l := range strings.Split(alpha.log.String(), "\n") {
			if strings.Contains(l, fmt.Sprintf("id=%q", id)) {
				lines = append(lines, l)
			}
		}
		if len(lines) != 1 || countDrops(alpha.log, id, "refused") != 1 {
			t.Fatalf("%s logged %q, want one drop line", id, lines)
		}
	}
}

// Mail to an address that names more than one recipient is passed at once:
// waiting does not change it, the sender has to address it again.
func TestMailToAmbiguousRecipientIsPassedAtOnce(t *testing.T) {
	hub, alpha := pair(t)
	rel := newScriptedRelay()
	clock := newTestClock()
	city := newSessionCity(t, "reader")
	city.addSession("twin")
	city.addSession("twin")
	rel.add(1, seal(t, hub, alpha, "twin-1", clock.Now(), mailTo("twin")))
	rel.add(2, seal(t, hub, alpha, "good-2", clock.Now(), mailTo("reader")))
	run := start(t, alpha.settings, clock.use(alpha.deps(rel.transport(), city.transport())))
	rel.recorder.await(t, "the place to pass both", func() bool { return slices.Contains(rel.recorder.reads(), 2) })
	run.stop(t)
	if n := len(city.posts()); n != 2 {
		t.Fatalf("the city API was asked %d times for two mails", n)
	}
	if countDrops(alpha.log, "twin-1", "refused") != 1 || len(city.inbox("reader")) != 1 {
		t.Fatalf("drops %q, inbox %d", alpha.log.lines("drop "), len(city.inbox("reader")))
	}
}

// The words the agent reads as no recipient, or more than one, are the
// session lookup's own, which the route answers with.
func TestRecipientFaultDetailsAreTheSessionLookupsWords(t *testing.T) {
	if detailRecipientNotFound != session.ErrSessionNotFound.Error() || detailRecipientAmbiguous != session.ErrAmbiguous.Error() {
		t.Fatalf("details %q and %q, session lookup %q and %q", detailRecipientNotFound, detailRecipientAmbiguous, session.ErrSessionNotFound, session.ErrAmbiguous)
	}
}

// The route also answers 400 when the city has no store to look the
// recipient up in, or no mail provider. Both pass when the store opens, so
// the mail stays at the relay and is asked again.
func TestStoreAbsentAnswersAreTriedAgain(t *testing.T) {
	for _, tc := range []struct {
		name string
		city func(t *testing.T) *cityAPI
		to   string
	}{
		{"no session store", func(t *testing.T) *cityAPI { return newCityAPI(t, "alpha", "reader") }, "nobody"},
		{"no mail provider", func(t *testing.T) *cityAPI { return newCityAPIWith(t, "alpha", beads.NewMemStore(), nil, "reader") }, "reader"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, alpha := pair(t)
			rel := newScriptedRelay()
			clock := newTestClock()
			city := tc.city(t)
			rel.add(1, seal(t, hub, alpha, "m-1", clock.Now(), mailTo(tc.to)))
			run := start(t, alpha.settings, clock.use(alpha.deps(rel.transport(), city.transport())))
			rel.recorder.await(t, "ten minutes of answers, or a drop", func() bool {
				return clock.elapsed() >= 10*time.Minute || len(alpha.log.lines("drop ")) > 0
			})
			run.stop(t)
			assertKeptAtRelay(t, alpha, rel, city.posts())
		})
	}
}

// Only an answer that puts the fault in the mail passes it: too large, or
// malformed. Any other refusal is asked again.
func TestOnlyAMailFaultPassesTheMail(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(*http.Request) (*http.Response, error)
		passed bool
	}{
		{"413 too large", problemWith(http.StatusRequestEntityTooLarge, "request-too-large", "request body too large"), true},
		{"422 malformed", problemWith(http.StatusUnprocessableEntity, "validation-failed", "validation failed"), true},
		{"400 without a recipient fault", problemWith(http.StatusBadRequest, "invalid-request", "something else"), false},
		{"405", problemWith(http.StatusMethodNotAllowed, "method-not-allowed", ""), false},
		{"410", problemWith(http.StatusGone, "gone", ""), false},
		{"415", problemWith(http.StatusUnsupportedMediaType, "unsupported-media-type", ""), false},
		{"451", problemWith(http.StatusUnavailableForLegalReasons, "unavailable", ""), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, alpha := pair(t)
			rel := newScriptedRelay()
			clock := newTestClock()
			city := newCityAPI(t, "alpha", "reader")
			stuck := filingKey("hub", "m-1")
			posts := newRecorder()
			apiTransport := posts.wrap(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Idempotency-Key") == stuck {
					return tc.answer(r)
				}
				return city.transport().RoundTrip(r)
			}))
			rel.add(1, seal(t, hub, alpha, "m-1", clock.Now(), mailTo("reader")))
			rel.add(2, seal(t, hub, alpha, "m-2", clock.Now(), mailTo("reader")))
			run := start(t, alpha.settings, clock.use(alpha.deps(rel.transport(), apiTransport)))
			if tc.passed {
				rel.recorder.await(t, "the place to pass both", func() bool { return slices.Contains(rel.recorder.reads(), 2) })
				run.stop(t)
				if n := len(posts.matching("POST", "/mail")); n != 2 {
					t.Fatalf("the city API was asked %d times for two mails", n)
				}
				if countDrops(alpha.log, "m-1", "refused") != 1 || len(city.inbox("reader")) != 1 {
					t.Fatalf("drops %q, inbox %d", alpha.log.lines("drop "), len(city.inbox("reader")))
				}
				return
			}
			rel.recorder.await(t, "ten minutes of answers, or a drop", func() bool {
				return clock.elapsed() >= 10*time.Minute || len(alpha.log.lines("drop ")) > 0
			})
			run.stop(t)
			assertKeptAtRelay(t, alpha, rel, posts.matching("POST", "/mail"))
		})
	}
}

// assertKeptAtRelay checks that a mail was asked again and never passed.
func assertKeptAtRelay(t *testing.T, alpha *cityFixture, rel *scriptedRelay, posts []recorded) {
	t.Helper()
	if drops := alpha.log.lines("drop "); len(drops) != 0 {
		t.Fatalf("dropped: %q", drops)
	}
	if slices.ContainsFunc(rel.recorder.reads(), func(n int64) bool { return n != 0 }) {
		t.Fatalf("the place moved past an unfiled mail: reads %v", rel.recorder.reads())
	}
	if len(posts) < 3 {
		t.Fatalf("the mail was asked %d times in ten minutes", len(posts))
	}
}
