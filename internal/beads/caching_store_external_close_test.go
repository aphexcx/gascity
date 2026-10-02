package beads

import (
	"context"
	"encoding/json"
	"testing"
)

// A live assignment read must not consume a worker's external close before
// reconciliation can notify the controller's autoclose subscriber.
func TestCachingStoreExternalCloseNotification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query ListQuery
	}{
		{"reconcile_first", ListQuery{}},
		{"live_assignment_read_first", ListQuery{Status: "in_progress", Assignee: "worker", Live: true, TierMode: TierBoth}},
		{"live_closed_read_first", ListQuery{Status: "closed", Live: true}},
		{"parent_read_first", ListQuery{ParentID: "batch"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backing := NewMemStore()
			child, err := backing.Create(Bead{Title: "worker task", Assignee: "worker", ParentID: "batch"})
			if err != nil {
				t.Fatal(err)
			}
			status := "in_progress"
			if err := backing.Update(child.ID, UpdateOpts{Status: &status}); err != nil {
				t.Fatal(err)
			}
			var closed []Bead
			cached := NewCachingStoreForTest(backing, func(kind, _ string, payload json.RawMessage) {
				if kind == "bead.closed" {
					b, ok := DecodeBeadEventPayload(payload)
					if !ok {
						t.Fatal("invalid close payload")
					}
					closed = append(closed, b)
				}
			})
			if err := cached.Prime(context.Background()); err != nil {
				t.Fatal(err)
			}
			// gc bd close writes the backing store in another process.
			if err := backing.Close(child.ID); err != nil {
				t.Fatal(err)
			}
			if tc.name != "reconcile_first" {
				if _, err := cached.List(tc.query); err != nil {
					t.Fatal(err)
				}
			}
			cached.runReconciliation()
			cached.runReconciliation()
			if len(closed) != 1 || closed[0].ID != child.ID || closed[0].Status != "closed" {
				t.Fatalf("close notifications = %v, want one closed payload for %s", closed, child.ID)
			}
		})
	}
}
