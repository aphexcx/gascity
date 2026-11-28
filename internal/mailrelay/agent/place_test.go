package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The saved place belongs to one city on one relay. A state folder kept
// across a change of relay address, reused under another city's name, or
// saved before the place recorded its owner, starts from 0 and says so in one
// log line. An unchanged one keeps its place.
func TestSavedPlaceBelongsToItsCityAndRelay(t *testing.T) {
	for _, tc := range []struct {
		name string
		// second returns the agent that starts on the same state folder.
		second func(t *testing.T, alpha *cityFixture) *cityFixture
		reason string // empty when the place is kept
	}{
		{"unchanged", func(_ *testing.T, alpha *cityFixture) *cityFixture { return alpha }, ""},
		{"relay address changed", func(_ *testing.T, alpha *cityFixture) *cityFixture {
			alpha.settings.RelayURL = "https://relay-two.test"
			return alpha
		}, "relay-changed"},
		{"city name changed", func(t *testing.T, alpha *cityFixture) *cityFixture {
			beta := newCityFixture(t, "beta")
			beta.keygen()
			beta.settings.StateDir = alpha.settings.StateDir
			return beta
		}, "city-changed"},
		{"owner never recorded", func(t *testing.T, alpha *cityFixture) *cityFixture {
			if err := os.WriteFile(filepath.Join(alpha.settings.StateDir, placeFile), []byte(`{"place":2}`), 0o600); err != nil {
				t.Fatal(err)
			}
			return alpha
		}, "owner-unrecorded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, alpha := pair(t)
			rel := newScriptedRelay()
			city := newCityAPI(t, "alpha", "reader")
			rel.add(1, seal(t, hub, alpha, "m-1", time.Now(), letter("reader")))
			rel.add(2, seal(t, hub, alpha, "m-2", time.Now(), letter("reader")))
			first := start(t, alpha.settings, alpha.deps(rel.transport(), city.transport()))
			rel.recorder.await(t, "the place to reach 2", func() bool { return slices.Contains(rel.recorder.reads(), 2) })
			first.stop(t)

			next := tc.second(t, alpha)
			before := len(rel.recorder.reads())
			again := start(t, next.settings, next.deps(rel.transport(), city.transport()))
			rel.recorder.await(t, "the first read of the second run", func() bool { return len(rel.recorder.reads()) > before })
			again.stop(t)

			from := rel.recorder.reads()[before]
			resets := next.log.lines("place-reset ")
			if next != alpha {
				resets = append(resets, alpha.log.lines("place-reset ")...)
			}
			if tc.reason == "" {
				if from != 2 || len(resets) != 0 {
					t.Fatalf("an unchanged state folder read from %d with resets %q", from, resets)
				}
				return
			}
			if from != 0 {
				t.Fatalf("the second run read from %d, want 0", from)
			}
			if len(resets) != 1 || !strings.Contains(resets[0], "from=2") || !strings.Contains(resets[0], "reason="+tc.reason) {
				t.Fatalf("reset lines %q, want one with from=2 and reason=%s", resets, tc.reason)
			}
		})
	}
}
