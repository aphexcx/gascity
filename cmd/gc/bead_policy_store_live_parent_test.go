package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// staleCachedStore is a store whose plain Get answers from a cache that has
// fallen behind — the shape a CachingStore takes for a parent another process
// relabeled since the cache was primed — while its explicit live handle reads
// the backing store. Its Handles() is what beads.HandlesFor hands a caller
// that asks for the authoritative read.
type staleCachedStore struct {
	beads.Store
	stale map[string]beads.Bead
}

func (s *staleCachedStore) Get(id string) (beads.Bead, error) {
	if b, ok := s.stale[id]; ok {
		return b, nil
	}
	return s.Store.Get(id)
}

// Handles exposes the cached read (this store's own Get) and the live read
// (the backing store's), the way CachingStore.Handles does.
func (s *staleCachedStore) Handles() beads.StoreHandles {
	return beads.StoreHandles{
		Cached: staleCachedReader{s},
		Live:   liveBackingReader{s.Store},
		Writer: s,
	}
}

type staleCachedReader struct{ s *staleCachedStore }

func (r staleCachedReader) Get(id string) (beads.Bead, error)            { return r.s.Get(id) }
func (r staleCachedReader) List(q beads.ListQuery) ([]beads.Bead, error) { return r.s.List(q) }
func (r staleCachedReader) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	return r.s.Ready(q...)
}

func (r staleCachedReader) DepList(id, dir string) ([]beads.Dep, error) {
	return r.s.DepList(id, dir)
}

// staleCachedGraphStore is staleCachedStore with a graph-apply door, the way
// a caching store over a graph-capable backing exposes both; it records the
// plan it receives.
type staleCachedGraphStore struct {
	*staleCachedStore
	plan *beads.GraphApplyPlan
}

func (s *staleCachedGraphStore) ApplyGraphPlan(_ context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) { //nolint:unparam // a recording fake never fails; the plan it received is the assertion
	s.plan = plan
	return &beads.GraphApplyResult{}, nil
}

type liveBackingReader struct{ s beads.Store }

func (r liveBackingReader) Get(id string) (beads.Bead, error)            { return r.s.Get(id) }
func (r liveBackingReader) List(q beads.ListQuery) ([]beads.Bead, error) { return r.s.List(q) }
func (r liveBackingReader) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	return r.s.Ready(q...)
}
func (r liveBackingReader) DepList(id, dir string) ([]beads.Dep, error) { return r.s.DepList(id, dir) }

// TestBeadPolicyStoreReadsTheParentOwnerFromTheLiveHandle pins the gf-jr0
// vs-integration gate's Major at cmd/gc/bead_policy_store.go:194 (gf-pu2
// finding 1f): the wrapper read the parent for the owner label through the
// store's plain Get, which a caching store answers from its cache. When
// another process had moved the parent to another city's lane since the
// prime, the child was stamped with the STALE owner, the backing BdStore took
// that stamp for an explicit override and switched its own inheritance off,
// and another city's child became claimable locally. The parent is now read
// through the store's live handle on every door — Create, a transaction's
// Create and a graph plan — so the child follows the parent's current owner.
func TestBeadPolicyStoreReadsTheParentOwnerFromTheLiveHandle(t *testing.T) {
	mem := beads.NewMemStore()
	parent, err := mem.Create(beads.Bead{Title: "epic", Type: "epic", Labels: []string{"owner:jadegate"}})
	if err != nil {
		t.Fatal(err)
	}
	// The cache still remembers the parent as this city's own.
	staleParent := parent
	staleParent.Labels = []string{"owner:citadel"}
	backing := &staleCachedStore{Store: mem, stale: map[string]beads.Bead{parent.ID: staleParent}}
	if got, _ := backing.Get(parent.ID); !reflect.DeepEqual(got.Labels, []string{"owner:citadel"}) {
		t.Fatalf("the fixture's plain Get must answer the stale labels, got %v", got.Labels)
	}
	store := wrapStoreWithBeadPolicies(backing, federatedTestConfig("citadel"))
	want := []string{"pool:x", "owner:jadegate"}

	child, err := store.Create(beads.Bead{Title: "child", ParentID: parent.ID, Labels: []string{"pool:x"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := mem.Get(child.ID); !reflect.DeepEqual(got.Labels, want) {
		t.Fatalf("Create: child labels = %v, want the parent's live owner %v", got.Labels, want)
	}

	var txChild beads.Bead
	err = store.Tx("t", func(tx beads.Tx) error {
		var err error
		txChild, err = tx.Create(beads.Bead{Title: "tx child", ParentID: parent.ID, Labels: []string{"pool:x"}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := mem.Get(txChild.ID); !reflect.DeepEqual(got.Labels, want) {
		t.Fatalf("Tx.Create: child labels = %v, want the parent's live owner %v", got.Labels, want)
	}

	recording := &staleCachedGraphStore{staleCachedStore: backing}
	graph := wrapStoreWithBeadPolicies(recording, federatedTestConfig("citadel"))
	applier, ok := beads.GraphApplyFor(graph)
	if !ok {
		t.Fatal("policy wrapper over a graph-apply store must expose ApplyGraphPlan")
	}
	if _, err := applier.ApplyGraphPlan(context.Background(), &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{
		{Key: "under-parent", Title: "x", ParentID: parent.ID, Labels: []string{"pool:x"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if recording.plan == nil || !reflect.DeepEqual(recording.plan.Nodes[0].Labels, want) {
		t.Fatalf("ApplyGraphPlan: node labels = %+v, want the parent's live owner %v", recording.plan, want)
	}
}
