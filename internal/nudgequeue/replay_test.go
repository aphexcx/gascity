package nudgequeue

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestReplayPreservesInputAndProvenance(t *testing.T) {
	now := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	dead := Item{ID: "n-dead", BeadID: "shadow-old", Agent: "worker", SessionID: "s-1", ContinuationEpoch: "3", Source: "sling", Message: " first line\r\nsecond\tline\n", Reference: &Reference{Kind: "bead", ID: "work-1"}, CreatedAt: now.Add(-time.Hour), Attempts: 5, LastAttemptAt: now.Add(-time.Minute), LastError: "draft_ownership_lost", DeadAt: now}
	state := State{Dead: []Item{dead}}
	item, err := PrepareReplay(&state, dead.ID, "n-new", now, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if item.ID != "n-new" || item.OriginID != dead.ID || item.Agent != dead.Agent || item.SessionID != dead.SessionID || item.ContinuationEpoch != dead.ContinuationEpoch || item.Source != dead.Source || item.Message != dead.Message || !reflect.DeepEqual(item.Reference, dead.Reference) {
		t.Fatalf("replay lost input or provenance: %+v", item)
	}
	if item.BeadID != "" || item.ReplayID != "" || item.Attempts != 0 || !item.LastAttemptAt.IsZero() || !item.DeadAt.IsZero() || item.LastError != "" || !item.CreatedAt.Equal(now) || !item.DeliverAfter.Equal(now) || !item.ExpiresAt.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("replay retained delivery state: %+v", item)
	}
	if !reflect.DeepEqual(state.Dead[0], dead) {
		t.Fatal("preparing a replay mutated the dead item")
	}
	item.Reference.ID = "changed"
	if dead.Reference.ID != "work-1" {
		t.Fatal("replay aliases the dead item's reference")
	}
	if err := RecordReplay(&state, dead.ID, item.ID); err != nil {
		t.Fatal(err)
	}
	if state.Dead[0].ReplayID != item.ID || !state.Dead[0].DeadAt.Equal(dead.DeadAt) {
		t.Fatalf("missing lineage or changed dead state: %+v", state.Dead[0])
	}
	if _, err := PrepareReplay(&state, dead.ID, "n-second", now, time.Hour); err == nil || !strings.Contains(err.Error(), item.ID) {
		t.Fatalf("second replay must name first replay: %v", err)
	}
}

func TestReplayRefusesNonDeadItems(t *testing.T) {
	state := State{Pending: []Item{{ID: "n-pending"}}, InFlight: []Item{{ID: "n-flight"}}}
	for _, id := range []string{"n-pending", "n-flight", "n-missing"} {
		if _, err := PrepareReplay(&state, id, "n-new", time.Now(), time.Hour); err == nil || !strings.Contains(err.Error(), id) {
			t.Fatalf("replay %s: %v", id, err)
		}
	}
}
