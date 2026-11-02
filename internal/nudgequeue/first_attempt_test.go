package nudgequeue

import (
	"testing"
	"time"
)

func TestClaimDueBatchRecordsFirstAttemptOnce(t *testing.T) {
	now := time.Now().UTC()
	for _, legacy := range []bool{false, true} {
		item := Item{ID: "n-1"}
		if legacy {
			item.Attempts = 1
			item.LastAttemptAt = now.Add(-time.Minute)
		}
		state := State{Pending: []Item{item}}
		claimed := ClaimDueBatch(&state, now, time.Minute, func(Item) bool { return true })
		want := now
		if legacy {
			want = time.Time{}
		}
		if len(claimed) != 1 || !claimed[0].FirstAttemptAt.Equal(want) {
			t.Fatalf("legacy=%v first claim=%+v", legacy, claimed)
		}
		state.Pending, state.InFlight = claimed, nil
		claimed = ClaimDueBatch(&state, now.Add(time.Hour), time.Minute, func(Item) bool { return true })
		if len(claimed) != 1 || !claimed[0].FirstAttemptAt.Equal(want) {
			t.Fatalf("legacy=%v retry changed first attempt: %+v", legacy, claimed)
		}
	}
}
