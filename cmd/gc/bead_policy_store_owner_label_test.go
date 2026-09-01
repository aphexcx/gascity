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
