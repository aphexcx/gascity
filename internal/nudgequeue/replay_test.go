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
	if err := RecordReplay(&state, dead.ID, item.ID, now); err != nil {
		t.Fatal(err)
	}
	if state.Dead[0].ReplayID != item.ID || !state.Dead[0].ReplayedAt.Equal(now) || !state.Dead[0].DeadAt.Equal(dead.DeadAt) {
		t.Fatalf("missing lineage or changed dead state: %+v", state.Dead[0])
	}
	if _, err := PrepareReplay(&state, dead.ID, "n-second", now, time.Hour); err == nil || !strings.Contains(err.Error(), item.ID) {
		t.Fatalf("second replay must name first replay: %v", err)
	}
}

func TestReplayRefusesSourceOwnedReminders(t *testing.T) {
	now := time.Date(2026, 11, 1, 20, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, source, recovery string
		ref                    *Reference
	}{
		{"wait", "wait", "gc wait ready", &Reference{Kind: "bead", ID: "wait-1"}},
		{"wait-without-reference", "wait", "gc wait ready", nil},
		{"mail", "mail", "gc mail read", &Reference{Kind: "mail", ID: "mail-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dead := Item{ID: "n-dead", Source: tc.source, Reference: tc.ref, DeadAt: now, Message: "preserved input"}
			state := State{Dead: []Item{dead}}
			if _, err := PrepareReplay(&state, dead.ID, "n-next", now, time.Hour); err == nil || !strings.Contains(err.Error(), tc.recovery) {
				t.Errorf("preview must direct recovery to %s: %v", tc.recovery, err)
			}
			if err := RecordReplay(&state, dead.ID, "n-next", now); err == nil || !strings.Contains(err.Error(), tc.recovery) {
				t.Errorf("commit must refuse source-owned replay: %v", err)
			}
			if !reflect.DeepEqual(state.Dead[0], dead) || !state.Dead[0].UnresolvedDeadLetter() {
				t.Fatal("refused replay changed the original or hid it from doctor")
			}
			if err := DismissDeadLetter(&state, dead.ID, now); err != nil || state.Dead[0].UnresolvedDeadLetter() {
				t.Fatalf("source-owned reminder cannot be dismissed: %v", err)
			}
		})
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

func TestReplayAndDismissDisposition(t *testing.T) {
	now := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		item Item
	}{
		{"superseded", Item{ID: "n-old", LastError: "superseded"}},
		{"dropped", Item{ID: "n-old", LastError: ManualDropReason}},
		{"dismissed", Item{ID: "n-old", DismissedAt: now.Add(-time.Hour)}},
		{"replayed", Item{ID: "n-old", ReplayID: "n-new"}},
		{"cap", Item{ID: "n-old", DeadAt: now.Add(-DeadLetterRetention)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := State{Dead: []Item{tc.item}}
			if _, err := PrepareReplay(&state, "n-old", "n-next", now, time.Hour); err == nil {
				t.Fatal("preview accepted disposed/expired item")
			}
			if err := RecordReplay(&state, "n-old", "n-next", now); err == nil {
				t.Fatal("stale replay committed")
			}
			if err := DismissDeadLetter(&state, "n-old", now); err == nil {
				t.Fatal("stale dismissal committed")
			}
			if !reflect.DeepEqual(state.Dead[0], tc.item) {
				t.Fatal("refusal mutated the disposition")
			}
		})
	}
	state := State{Dead: []Item{{ID: "n-live", DeadAt: now.Add(-DeadLetterRetention + time.Second)}}}
	if _, err := PrepareReplay(&state, "n-live", "n-next", now, time.Hour); err != nil {
		t.Fatalf("recovery window closed early: %v", err)
	}
	if err := DismissDeadLetter(&state, "n-live", now); err != nil || !state.Dead[0].DismissedAt.Equal(now) || state.Dead[0].UnresolvedDeadLetter() {
		t.Fatalf("dismissal not recorded: %+v %v", state, err)
	}
}
