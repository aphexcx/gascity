package beads

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
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

// A local Close owns its notification while its post-write Get is in flight;
// neither live reads nor reconciliation may publish the same transition.
func TestCachingStoreLocalCloseRacingRefresh(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query ListQuery
	}{
		{"reconcile", ListQuery{}},
		{"live_closed", ListQuery{Status: "closed", Live: true}},
		{"live_missing", ListQuery{Status: "open", Live: true}},
		{"parent_missing", ListQuery{ParentID: "batch", Status: "open"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				backing := &closeRefreshBarrierStore{Store: NewMemStore(), entered: make(chan struct{}), release: make(chan struct{})}
				child, err := backing.Create(Bead{Title: "worker task", ParentID: "batch"})
				if err != nil {
					t.Fatal(err)
				}
				var closed []Bead
				cached := NewCachingStoreForTest(backing, func(kind, _ string, payload json.RawMessage) {
					if kind == "bead.closed" {
						b, _ := DecodeBeadEventPayload(payload)
						closed = append(closed, b)
					}
				})
				if err := cached.Prime(t.Context()); err != nil {
					t.Fatal(err)
				}
				backing.blockNextGet.Store(true)
				done := make(chan error, 1)
				go func() { done <- cached.Close(child.ID) }()
				<-backing.entered
				if tc.name == "reconcile" {
					cached.runReconciliation()
				} else if _, err := cached.List(tc.query); err != nil {
					t.Error(err)
				}
				close(backing.release)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				cached.runReconciliation()
				if len(closed) != 1 || closed[0].ID != child.ID || closed[0].Status != "closed" {
					t.Fatalf("close notifications = %v, want one closed payload for %s", closed, child.ID)
				}
			})
		})
	}
}

type closeRefreshBarrierStore struct {
	Store
	blockNextGet atomic.Bool
	entered      chan struct{}
	release      chan struct{}
	closeErr     error
}

func (s *closeRefreshBarrierStore) Get(id string) (Bead, error) {
	if s.blockNextGet.CompareAndSwap(true, false) {
		close(s.entered)
		<-s.release
	}
	return s.Store.Get(id)
}

func (s *closeRefreshBarrierStore) Close(id string) error {
	if s.closeErr != nil {
		return s.closeErr
	}
	return s.Store.Close(id)
}

// A failed local close must release its reservation so a later external
// close can still wake the controller's subscribers.
func TestCachingStoreFailedCloseAllowsExternalNotification(t *testing.T) {
	for _, reconcile := range []bool{false, true} {
		name := "live_read"
		if reconcile {
			name = "reconcile"
		}
		t.Run(name, func(t *testing.T) {
			backing := &closeRefreshBarrierStore{Store: NewMemStore(), closeErr: errors.New("close unavailable")}
			child, err := backing.Create(Bead{Title: "worker task"})
			if err != nil {
				t.Fatal(err)
			}
			var closed []string
			cached := NewCachingStoreForTest(backing, func(kind, id string, _ json.RawMessage) {
				if kind == "bead.closed" {
					closed = append(closed, id)
				}
			})
			if err := cached.Prime(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := cached.Close(child.ID); !errors.Is(err, backing.closeErr) {
				t.Fatalf("Close = %v, want backing failure", err)
			}
			if len(closed) != 0 {
				t.Fatalf("failed close emitted notifications: %v", closed)
			}
			if err := backing.Store.Close(child.ID); err != nil {
				t.Fatal(err)
			}
			if reconcile {
				cached.runReconciliation()
			} else if _, err := cached.List(ListQuery{Status: "closed", Live: true}); err != nil {
				t.Fatal(err)
			}
			if len(closed) != 1 || closed[0] != child.ID {
				t.Fatalf("external close notifications = %v, want [%s]", closed, child.ID)
			}
		})
	}
}

func TestCachingStorePendingClosePreservesDeleteFence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		backing := &closeRefreshBarrierStore{Store: NewMemStore(), entered: make(chan struct{}), release: make(chan struct{})}
		child, err := backing.Create(Bead{Title: "worker task"})
		if err != nil {
			t.Fatal(err)
		}
		cached := NewCachingStoreForTest(backing, nil)
		if err := cached.Prime(t.Context()); err != nil {
			t.Fatal(err)
		}
		backing.blockNextGet.Store(true)
		done := make(chan error, 1)
		go func() { done <- cached.Close(child.ID) }()
		<-backing.entered
		query := ListQuery{Status: "closed", Live: true}
		cached.mu.RLock()
		startSeq := cached.mutationSeq
		cached.mu.RUnlock()
		rows, err := backing.List(query)
		if err != nil {
			t.Fatal(err)
		}
		if err := cached.Delete(child.ID); err != nil {
			t.Fatal(err)
		}
		got := cached.refreshCachedBeads(query, startSeq, rows)
		close(backing.release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("live refresh returned locally deleted row: %v", got)
		}
	})
}
