package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func federatedTestConfig(identity string) *config.City {
	return &config.City{Federation: config.FederationConfig{Identity: identity}}
}

// TestBeadPolicyStoreStampsTheOwnerLabelOnCreate pins the universal in-process
// door: every store gc opens is wrapped with the bead policies, so a create
// through the wrapper carries owner:<identity> whatever backend serves it.
func TestBeadPolicyStoreStampsTheOwnerLabelOnCreate(t *testing.T) {
	tests := []struct {
		name     string
		identity string
		labels   []string
		want     []string
	}{
		{"stamped", "citadel", nil, []string{"owner:citadel"}},
		{"user labels first", "citadel", []string{"hold:mayor"}, []string{"hold:mayor", "owner:citadel"}},
		{"explicit foreign owner respected", "citadel", []string{"owner:jadegate"}, []string{"owner:jadegate"}},
		{"no identity, no stamp", "", []string{"hold:mayor"}, []string{"hold:mayor"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := wrapStoreWithBeadPolicies(beads.NewMemStore(), federatedTestConfig(tt.identity))
			created, err := store.Create(beads.Bead{Title: "t", Labels: tt.labels})
			if err != nil {
				t.Fatal(err)
			}
			got, err := store.Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Labels, tt.want) {
				t.Fatalf("labels = %v, want %v", got.Labels, tt.want)
			}
		})
	}
}

// recordingGraphApplyStore captures the plan the policy wrapper hands down.
type recordingGraphApplyStore struct {
	beads.Store
	plan *beads.GraphApplyPlan
}

func (s *recordingGraphApplyStore) ApplyGraphPlan(_ context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) { //nolint:unparam // a recording fake never fails; the plan it received is the assertion
	s.plan = plan
	return &beads.GraphApplyResult{}, nil
}

// TestBeadPolicyGraphStoreStampsTheOwnerLabelOnEveryNode: a graph apply is a
// bulk create (molecule roots, steps, control beads), so each node is stamped
// like a single create would be, and the caller's plan is not mutated.
func TestBeadPolicyGraphStoreStampsTheOwnerLabelOnEveryNode(t *testing.T) {
	backing := &recordingGraphApplyStore{Store: beads.NewMemStore()}
	store := wrapStoreWithBeadPolicies(backing, federatedTestConfig("citadel"))
	applier, ok := beads.GraphApplyFor(store)
	if !ok {
		t.Fatal("policy wrapper over a graph-apply store must expose ApplyGraphPlan")
	}
	plan := &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{
		{Key: "root", Title: "root"},
		{Key: "step", Title: "step", Labels: []string{"pool:x"}},
		{Key: "foreign", Title: "foreign", Labels: []string{"owner:jadegate"}},
	}}
	if _, err := applier.ApplyGraphPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if backing.plan == nil {
		t.Fatal("backing store received no plan")
	}
	got := make([]string, 0, len(backing.plan.Nodes))
	for _, n := range backing.plan.Nodes {
		got = append(got, strings.Join(n.Labels, ","))
	}
	want := []string{"owner:citadel", "pool:x,owner:citadel", "owner:jadegate"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("node labels = %v, want %v", got, want)
	}
	if plan.Nodes[0].Labels != nil || !reflect.DeepEqual(plan.Nodes[1].Labels, []string{"pool:x"}) {
		t.Fatalf("caller's plan was mutated: %+v", plan.Nodes)
	}
}

// TestBdStoreOptionsForConfigCarryTheOwnerLabel: the bd-backed stores gc
// builds directly (bdStoreForCity/bdStoreForRig) stamp through the same helper.
func TestBdStoreOptionsForConfigCarryTheOwnerLabel(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"id":"bd-x","title":"t","status":"open","issue_type":"task","created_at":"2025-01-15T10:30:00Z"}`), nil
	}
	store := beads.NewBdStore("/city", runner, bdStoreOptionsForConfig(federatedTestConfig("citadel"))...)
	if _, err := store.Create(beads.Bead{Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(gotArgs, " "); !strings.Contains(joined, "--labels owner:citadel") {
		t.Fatalf("args = %q, want --labels owner:citadel", joined)
	}
	gotArgs = nil
	store = beads.NewBdStore("/city", runner, bdStoreOptionsForConfig(federatedTestConfig(""))...)
	if _, err := store.Create(beads.Bead{Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(gotArgs, " "); strings.Contains(joined, "--labels") {
		t.Fatalf("args = %q, want no --labels without an identity", joined)
	}
}

// TestBeadPolicyStoreStampsTheOwnerLabelInsideATransaction: Store.Tx hands
// the caller a transaction whose Create must stamp like the store's own —
// session beads and idempotency records are created that way.
func TestBeadPolicyStoreStampsTheOwnerLabelInsideATransaction(t *testing.T) {
	store := wrapStoreWithBeadPolicies(beads.NewMemStore(), federatedTestConfig("citadel"))
	var created beads.Bead
	err := store.Tx("test: tx create", func(tx beads.Tx) error {
		var err error
		created, err = tx.Create(beads.Bead{Title: "in tx", Labels: []string{"hold:mayor"}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"hold:mayor", "owner:citadel"}; !reflect.DeepEqual(got.Labels, want) {
		t.Fatalf("labels = %v, want %v", got.Labels, want)
	}
}

// TestBeadPolicyStoreChildOfAnOwnedParentInheritsThatOwner: on every backend
// — not only bd, which copies parent labels itself — a child created under
// another city's bead stays in that city's lane, through Create, a
// transaction, and a graph node that names an existing parent.
func TestBeadPolicyStoreChildOfAnOwnedParentInheritsThatOwner(t *testing.T) {
	mem := beads.NewMemStore()
	parent, err := mem.Create(beads.Bead{Title: "jadegate epic", Type: "epic", Labels: []string{"owner:jadegate"}})
	if err != nil {
		t.Fatal(err)
	}
	orphanParent, err := mem.Create(beads.Bead{Title: "unowned epic", Type: "epic"})
	if err != nil {
		t.Fatal(err)
	}
	store := wrapStoreWithBeadPolicies(mem, federatedTestConfig("citadel"))

	child, err := store.Create(beads.Bead{Title: "child", ParentID: parent.ID, Labels: []string{"pool:x"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := mem.Get(child.ID); !reflect.DeepEqual(got.Labels, []string{"pool:x", "owner:jadegate"}) {
		t.Fatalf("Create child labels = %v, want the parent's owner", got.Labels)
	}

	var txChild beads.Bead
	if err := store.Tx("t", func(tx beads.Tx) error {
		var err error
		txChild, err = tx.Create(beads.Bead{Title: "tx child", ParentID: parent.ID})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := mem.Get(txChild.ID); !reflect.DeepEqual(got.Labels, []string{"owner:jadegate"}) {
		t.Fatalf("Tx child labels = %v, want the parent's owner", got.Labels)
	}

	own, err := store.Create(beads.Bead{Title: "own child", ParentID: orphanParent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := mem.Get(own.ID); !reflect.DeepEqual(got.Labels, []string{"owner:citadel"}) {
		t.Fatalf("child of an unowned parent labels = %v, want the creator's owner", got.Labels)
	}

	backing := &recordingGraphApplyStore{Store: mem}
	graph := wrapStoreWithBeadPolicies(backing, federatedTestConfig("citadel"))
	applier, _ := beads.GraphApplyFor(graph)
	if _, err := applier.ApplyGraphPlan(context.Background(), &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{
		{Key: "under-jadegate", Title: "x", ParentID: parent.ID},
		{Key: "under-unowned", Title: "y", ParentID: orphanParent.ID},
		{Key: "in-plan", Title: "z", ParentKey: "under-jadegate"},
	}}); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, 3)
	for _, n := range backing.plan.Nodes {
		got = append(got, strings.Join(n.Labels, ","))
	}
	// The in-plan child follows its parent's EFFECTIVE lane: a subtree created
	// under another city's bead stays in that city's lane.
	if want := []string{"owner:jadegate", "owner:citadel", "owner:jadegate"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("graph node labels = %v, want %v", got, want)
	}
}

// TestBeadPolicyStoreTxChildInheritsAnInTransactionParent: a parent created
// earlier in the same transaction is not yet visible through the outer store;
// the transaction remembers what it created so its children still inherit.
func TestBeadPolicyStoreTxChildInheritsAnInTransactionParent(t *testing.T) {
	mem := beads.NewMemStore()
	store := wrapStoreWithBeadPolicies(&invisibleUntilCommitStore{Store: mem}, federatedTestConfig("citadel"))
	var child beads.Bead
	if err := store.Tx("t", func(tx beads.Tx) error {
		parent, err := tx.Create(beads.Bead{Title: "jadegate epic", Labels: []string{"owner:jadegate"}})
		if err != nil {
			return err
		}
		child, err = tx.Create(beads.Bead{Title: "child", ParentID: parent.ID})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got, err := mem.Get(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Labels, []string{"owner:jadegate"}) {
		t.Fatalf("child labels = %v, want the in-transaction parent's owner", got.Labels)
	}
}

// invisibleUntilCommitStore hides every bead from Get while a transaction is
// open, the way a real transactional backend does.
type invisibleUntilCommitStore struct {
	beads.Store
	inTx bool
}

func (s *invisibleUntilCommitStore) Tx(msg string, fn func(tx beads.Tx) error) error {
	s.inTx = true
	defer func() { s.inTx = false }()
	return s.Store.Tx(msg, fn)
}

func (s *invisibleUntilCommitStore) Get(id string) (beads.Bead, error) {
	if s.inTx {
		return beads.Bead{}, beads.ErrNotFound
	}
	return s.Store.Get(id)
}
