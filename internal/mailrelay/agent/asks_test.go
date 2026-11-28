package agent

import (
	"io"
	"net/http"
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
