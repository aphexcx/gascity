package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// parentReadFailingStore fails Get for the ids in fail, the way a bd read
// fails when the parent lives in a store this process cannot reach at the
// moment (a dolt server mid-restart, a federation peer's store).
type parentReadFailingStore struct {
	beads.Store
	fail map[string]error
}

func (s *parentReadFailingStore) Get(id string) (beads.Bead, error) {
	if err := s.fail[id]; err != nil {
		return beads.Bead{}, err
	}
	return s.Store.Get(id)
}

func countPolicyStoreBeads(t *testing.T, store beads.Store) int {
	t.Helper()
	all, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	return len(all)
}

// TestBeadPolicyStoreAbortsCreateWhenTheParentCannotBeRead pins the gf-4c9
// round-2 gate finding at cmd/gc/bead_policy_store.go:174 (finding 1d): a
// failed parent read became "no owner", the wrapper stamped the creating
// city's owner label on the child, and the backing BdStore — which reads the
// parent itself and treats an owner label already on the bead as an explicit
// override — then turned its own inheritance off. A child of another city's
// bead ended up claimable by this city. A parent that cannot be read now
// aborts the create with that error, through Create, a transaction and a
// graph plan; nothing is stamped and nothing is created. The same parent,
// once readable, inherits its foreign owner as before.
func TestBeadPolicyStoreAbortsCreateWhenTheParentCannotBeRead(t *testing.T) {
	mem := beads.NewMemStore()
	parent, err := mem.Create(beads.Bead{Title: "jadegate epic", Type: "epic", Labels: []string{"owner:jadegate"}})
	if err != nil {
		t.Fatal(err)
	}
	readErr := errors.New("dolt server unavailable")
	backing := &parentReadFailingStore{Store: mem, fail: map[string]error{parent.ID: readErr}}
	store := wrapStoreWithBeadPolicies(backing, federatedTestConfig("citadel"))
	before := countPolicyStoreBeads(t, mem)

	_, err = store.Create(beads.Bead{Title: "child", ParentID: parent.ID, Labels: []string{"pool:x"}})
	if !errors.Is(err, readErr) {
		t.Fatalf("Create under an unreadable parent: err = %v, want the parent read error", err)
	}
	if !strings.Contains(err.Error(), parent.ID) {
		t.Fatalf("Create error %q does not name the parent", err)
	}

	err = store.Tx("t", func(tx beads.Tx) error {
		_, err := tx.Create(beads.Bead{Title: "tx child", ParentID: parent.ID})
		return err
	})
	if !errors.Is(err, readErr) {
		t.Fatalf("Tx.Create under an unreadable parent: err = %v, want the parent read error", err)
	}

	recording := &recordingGraphApplyStore{Store: backing}
	graph := wrapStoreWithBeadPolicies(recording, federatedTestConfig("citadel"))
	applier, ok := beads.GraphApplyFor(graph)
	if !ok {
		t.Fatal("policy wrapper over a graph-apply store must expose ApplyGraphPlan")
	}
	_, err = applier.ApplyGraphPlan(context.Background(), &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{
		{Key: "under-unreadable", Title: "x", ParentID: parent.ID},
	}})
	if !errors.Is(err, readErr) {
		t.Fatalf("ApplyGraphPlan under an unreadable parent: err = %v, want the parent read error", err)
	}
	if recording.plan != nil {
		t.Fatalf("a plan under an unreadable parent reached the backing store: %+v", recording.plan.Nodes)
	}

	if after := countPolicyStoreBeads(t, mem); after != before {
		t.Fatalf("beads created under an unreadable parent: %d -> %d", before, after)
	}

	// The control: the same foreign parent, readable, is inherited.
	delete(backing.fail, parent.ID)
	child, err := store.Create(beads.Bead{Title: "child", ParentID: parent.ID, Labels: []string{"pool:x"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := mem.Get(child.ID); !reflect.DeepEqual(got.Labels, []string{"pool:x", "owner:jadegate"}) {
		t.Fatalf("child of a readable foreign parent labels = %v, want the parent's owner", got.Labels)
	}
}

// TestBeadPolicyStoreDoesNotReadTheParentWhenNotFederated: without an
// identity the wrapper stamps nothing and reads nothing, so a parent read
// failure cannot reach a non-federated city's creates.
func TestBeadPolicyStoreDoesNotReadTheParentWhenNotFederated(t *testing.T) {
	mem := beads.NewMemStore()
	parent, err := mem.Create(beads.Bead{Title: "epic", Type: "epic"})
	if err != nil {
		t.Fatal(err)
	}
	backing := &parentReadFailingStore{Store: mem, fail: map[string]error{parent.ID: errors.New("unreadable")}}
	store := wrapStoreWithBeadPolicies(backing, federatedTestConfig(""))
	if _, err := store.Create(beads.Bead{Title: "child", ParentID: parent.ID}); err != nil {
		t.Fatalf("non-federated create under an unreadable parent: %v", err)
	}
}
