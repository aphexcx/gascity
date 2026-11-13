package main

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
)

// Models a List queued behind a holder of the native store's list gate.
type queuedOrderListStore struct {
	beads.Store
	release      chan struct{}
	exited       atomic.Bool
	backingCalls atomic.Int32
}

func (s *queuedOrderListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	return s.ListWithQueueContext(context.Background(), q)
}

func (s *queuedOrderListStore) ListWithQueueContext(ctx context.Context, _ beads.ListQuery) ([]beads.Bead, error) {
	defer s.exited.Store(true)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.backingCalls.Add(1)
		return nil, nil
	}
}

func TestOrderGateTimeoutCancelsQueuedDescendantList(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &queuedOrderListStore{Store: beads.NewMemStore(), release: make(chan struct{})}
		idx := newOrderDispatchTrackingIndex(io.Discard)
		idx.entries["scope"] = map[string]orderTrackingSummary{"sample": {openRoots: []beads.Bead{{ID: "root", Type: "molecule", Status: "open"}}}}
		m := &memoryOrderDispatcher{stderr: io.Discard}
		cand := &orderDispatchCandidate{order: orders.Order{Name: "sample"}, scoped: "sample", gateStores: []beads.Store{store}, gateStoreKeys: []string{"scope"}}
		if !m.openWorkGateShut(context.Background(), cand, idx, time.Now()) {
			t.Error("non-idempotent order must stay blocked on timeout")
		}
		synctest.Wait()
		if !store.exited.Load() {
			t.Error("timed-out order leaves its list queued on the shared handle")
		}
		close(store.release)
		synctest.Wait()
		if got := store.backingCalls.Load(); got != 0 {
			t.Errorf("abandoned gate performed %d backing reads after its timeout", got)
		}
	})
}
