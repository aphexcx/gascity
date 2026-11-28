package agent

import (
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// A relay that answers every read at once is asked at most once a second.
func TestInstantAnswersAreAskedAtMostOncePerSecond(t *testing.T) {
	_, alpha := pair(t)
	clock := newTestClock()
	var mu sync.Mutex
	var asks []time.Time
	answered := newRecorder()
	relayTransport := answered.wrap(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp := &http.Response{StatusCode: http.StatusNoContent, Request: r, Body: http.NoBody, Header: http.Header{}}
		if r.Method == http.MethodGet {
			mu.Lock()
			asks = append(asks, clock.Now())
			mu.Unlock()
			resp.StatusCode, resp.Body = http.StatusOK, io.NopCloser(strings.NewReader("[]"))
		}
		return resp, nil
	}))
	run := start(t, alpha.settings, clock.use(alpha.deps(relayTransport, nil)))
	answered.await(t, "five asks", func() bool { return len(answered.reads()) >= 5 })
	run.stop(t)
	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(asks); i++ {
		if gap := asks[i].Sub(asks[i-1]); gap < time.Second {
			t.Fatalf("ask %d came %s after the one before it", i+1, gap)
		}
	}
}

// After the place starts over, and after a read that failed, the agent puts
// its key statement on the relay again: a relay that lost its store has lost
// the statement too.
func TestKeyStatementIsPublishedAgain(t *testing.T) {
	t.Run("after a place reset", func(t *testing.T) {
		hub, alpha := pair(t)
		rel := newScriptedRelay()
		city := newCityAPI(t, "alpha", "reader")
		rel.add(1, seal(t, hub, alpha, "m-1", time.Now(), letter("reader")))
		run := start(t, alpha.settings, alpha.deps(rel.transport(), city.transport()))
		rel.recorder.await(t, "the place to reach 1", func() bool { return slices.Contains(rel.recorder.reads(), 1) })
		// The relay loses its store: its counter starts over below the place.
		rel.mu.Lock()
		rel.last, rel.queue = 0, nil
		rel.mu.Unlock()
		rel.recorder.await(t, "a read from 0 after the reset", func() bool {
			reads := rel.recorder.reads()
			return len(reads) > 2 && slices.Contains(reads[slices.Index(reads, 1):], 0)
		})
		run.stop(t)
		assertPublishedAfter(t, rel.recorder.all(), func(r recorded) bool {
			return r.method == http.MethodGet && strings.Contains(r.query, "after=1")
		})
	})
	t.Run("after a failed read", func(t *testing.T) {
		_, alpha := pair(t)
		rel := newScriptedRelay()
		var once sync.Once
		failing := rel.recorder.wrap(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			failed := false
			if r.Method == http.MethodGet {
				once.Do(func() { failed = true })
			}
			if failed {
				return problem(r, http.StatusServiceUnavailable, "unavailable"), nil
			}
			return handlerTransport{rel}.RoundTrip(r)
		}))
		run := start(t, alpha.settings, alpha.deps(failing, nil))
		rel.recorder.await(t, "a second read", func() bool { return len(rel.recorder.reads()) >= 2 })
		run.stop(t)
		assertPublishedAfter(t, rel.recorder.all(), func(r recorded) bool { return r.method == http.MethodGet })
	})
}

// assertPublishedAfter checks that a key statement was put after the first
// request that matches.
func assertPublishedAfter(t *testing.T, all []recorded, match func(recorded) bool) {
	t.Helper()
	i := slices.IndexFunc(all, match)
	if i < 0 {
		t.Fatal("no matching request")
	}
	if !slices.ContainsFunc(all[i+1:], func(r recorded) bool { return r.method == http.MethodPut && strings.HasSuffix(r.path, "/key") }) {
		var seen []string
		for _, r := range all {
			seen = append(seen, r.method+" "+r.path+"?"+r.query)
		}
		t.Fatalf("no key statement put after request %d: %q", i+1, seen)
	}
}
