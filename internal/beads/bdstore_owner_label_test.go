package beads_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func bdCreateLabelsArg(t *testing.T, args []string) (string, bool) {
	t.Helper()
	for i, a := range args {
		if a == "--labels" {
			if i+1 >= len(args) {
				t.Fatalf("--labels has no value in %q", args)
			}
			return args[i+1], true
		}
	}
	return "", false
}

// TestBdStoreCreateStampsTheOwnerLabel is the SDK door of gp-0uj: an
// in-process create through BdStore carries owner:<identity> exactly as the
// gc bd argv injector would have stamped it.
func TestBdStoreCreateStampsTheOwnerLabel(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   string
	}{
		{"no labels", nil, "owner:citadel"},
		{"user labels first", []string{"foo"}, "foo,owner:citadel"},
		{"explicit foreign owner respected", []string{"owner:jadegate"}, "owner:jadegate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotArgs []string
			runner := func(_, _ string, args ...string) ([]byte, error) {
				gotArgs = args
				return []byte(`{"id":"bd-x","title":"t","status":"open","issue_type":"task","created_at":"2025-01-15T10:30:00Z"}`), nil
			}
			s := beads.NewBdStore("/city", runner, beads.WithBdStoreOwnerLabel("owner:citadel"))
			if _, err := s.Create(beads.Bead{Title: "t", Labels: tt.labels}); err != nil {
				t.Fatal(err)
			}
			got, ok := bdCreateLabelsArg(t, gotArgs)
			if !ok || got != tt.want {
				t.Fatalf("--labels = %q (present=%v) in %q, want %q", got, ok, strings.Join(gotArgs, " "), tt.want)
			}
		})
	}
}

func TestBdStoreCreateWithoutAnOwnerLabelIsUnchanged(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"id":"bd-x","title":"t","status":"open","issue_type":"task","created_at":"2025-01-15T10:30:00Z"}`), nil
	}
	s := beads.NewBdStore("/city", runner, beads.WithBdStoreOwnerLabel(""))
	if _, err := s.Create(beads.Bead{Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := bdCreateLabelsArg(t, gotArgs); ok {
		t.Fatalf("args = %q, want no --labels without an owner", gotArgs)
	}
}

// TestBdStoreApplyGraphPlanStampsTheOwnerLabel: the option promises every
// create, and a graph apply is a bulk create through `bd create --graph`.
func TestBdStoreApplyGraphPlanStampsTheOwnerLabel(t *testing.T) {
	var captured beads.GraphApplyPlan
	runner := func(_, _ string, args ...string) ([]byte, error) {
		data, err := os.ReadFile(args[2])
		if err != nil {
			t.Fatalf("reading graph plan: %v", err)
		}
		if err := json.Unmarshal(data, &captured); err != nil {
			t.Fatalf("unmarshal graph plan: %v", err)
		}
		return []byte(`{"ids":{"root":"bd-root","step":"bd-step","foreign":"bd-foreign"}}`), nil
	}
	s := beads.NewBdStore(t.TempDir(), runner, beads.WithBdStoreOwnerLabel("owner:citadel"))
	plan := &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{
		{Key: "root", Title: "root"},
		{Key: "step", Title: "step", Labels: []string{"pool:x"}},
		{Key: "foreign", Title: "foreign", Labels: []string{"owner:jadegate"}},
	}}
	if _, err := s.ApplyGraphPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(captured.Nodes))
	for _, n := range captured.Nodes {
		got = append(got, strings.Join(n.Labels, ","))
	}
	want := []string{"owner:citadel", "pool:x,owner:citadel", "owner:jadegate"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("node labels sent to bd = %v, want %v", got, want)
	}
	if plan.Nodes[0].Labels != nil || !reflect.DeepEqual(plan.Nodes[1].Labels, []string{"pool:x"}) {
		t.Fatalf("caller's plan was mutated: %+v", plan.Nodes)
	}
}

// TestBdStoreCreateUnderAnOwnedParentInheritsThatOwner: bd copies a parent's
// labels onto the child, so a child of another city's bead must carry that
// owner and not a second one; the store reads the parent first.
func TestBdStoreCreateUnderAnOwnedParentInheritsThatOwner(t *testing.T) {
	tests := []struct {
		name         string
		parentLabels string // JSON array body for show --json
		parentErr    bool
		want         string
	}{
		{"parent owned by another city", `"hold:mayor","owner:jadegate"`, false, "owner:jadegate"},
		{"parent without an owner", `"hold:mayor"`, false, "owner:citadel"},
		{"parent unreadable: the creator's owner", "", true, "owner:citadel"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var createArgs []string
			runner := func(_, _ string, args ...string) ([]byte, error) {
				switch args[0] {
				case "show":
					if tt.parentErr {
						return nil, errors.New("bd show: boom")
					}
					return []byte(`[{"id":"P","title":"parent","status":"open","issue_type":"epic","created_at":"2026-09-01T00:00:00Z","labels":[` + tt.parentLabels + `]}]`), nil
				case "create":
					createArgs = args
					return []byte(`{"id":"bd-x","title":"t","status":"open","issue_type":"task","created_at":"2026-09-01T00:00:00Z"}`), nil
				}
				return nil, errors.New("unexpected: " + strings.Join(args, " "))
			}
			s := beads.NewBdStore("/city", runner, beads.WithBdStoreOwnerLabel("owner:citadel"))
			if _, err := s.Create(beads.Bead{Title: "t", ParentID: "P"}); err != nil {
				t.Fatal(err)
			}
			got, ok := bdCreateLabelsArg(t, createArgs)
			if !ok || got != tt.want {
				t.Fatalf("--labels = %q (present=%v), want %q; args=%q", got, ok, tt.want, strings.Join(createArgs, " "))
			}
		})
	}
}

// TestBdStoreCreateExplicitOwnerUnderAForeignParentIsExclusive: bd would copy
// the parent's owner onto a child that names its own; the store turns
// inheritance off for that create and carries the parent's other labels
// itself, so the bead ends with exactly the owner it was given.
func TestBdStoreCreateExplicitOwnerUnderAForeignParentIsExclusive(t *testing.T) {
	var createArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		switch args[0] {
		case "show":
			return []byte(`[{"id":"P","title":"parent","status":"open","issue_type":"epic","created_at":"2026-09-01T00:00:00Z","labels":["hold:mayor","owner:jadegate"]}]`), nil
		case "create":
			createArgs = args
			return []byte(`{"id":"bd-x","title":"t","status":"open","issue_type":"task","created_at":"2026-09-01T00:00:00Z"}`), nil
		}
		return nil, errors.New("unexpected: " + strings.Join(args, " "))
	}
	s := beads.NewBdStore("/city", runner, beads.WithBdStoreOwnerLabel("owner:citadel"))
	if _, err := s.Create(beads.Bead{Title: "t", ParentID: "P", Labels: []string{"owner:boomtown"}}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(createArgs, " ")
	if got, _ := bdCreateLabelsArg(t, createArgs); got != "owner:boomtown,hold:mayor" {
		t.Fatalf("--labels = %q, want the explicit owner plus the parent's non-owner labels; args=%q", got, joined)
	}
	if !strings.Contains(joined, "--no-inherit-labels") {
		t.Fatalf("args = %q, want --no-inherit-labels so bd does not copy the parent's owner", joined)
	}
}

// TestBdStoreCreateExplicitOwnerStaysExclusiveWhenTheParentIsUnreadable: the
// parent could not be read, so nothing is known about the owner bd would copy
// onto the child. A child that names its own owner keeps bd's copying off
// regardless — the one operand bd cannot move — and ends with exactly the
// owner it was given; the parent's other labels cannot be carried, since they
// could not be read.
func TestBdStoreCreateExplicitOwnerStaysExclusiveWhenTheParentIsUnreadable(t *testing.T) {
	var createArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		switch args[0] {
		case "show":
			return nil, errors.New("bd show: boom")
		case "create":
			createArgs = args
			return []byte(`{"id":"bd-x","title":"t","status":"open","issue_type":"task","created_at":"2026-09-01T00:00:00Z"}`), nil
		}
		return nil, errors.New("unexpected: " + strings.Join(args, " "))
	}
	s := beads.NewBdStore("/city", runner, beads.WithBdStoreOwnerLabel("owner:citadel"))
	if _, err := s.Create(beads.Bead{Title: "t", ParentID: "P", Labels: []string{"owner:boomtown"}}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(createArgs, " ")
	if got, _ := bdCreateLabelsArg(t, createArgs); got != "owner:boomtown" {
		t.Fatalf("--labels = %q, want exactly the explicit owner; args=%q", got, joined)
	}
	if !strings.Contains(joined, "--no-inherit-labels") {
		t.Fatalf("args = %q, want --no-inherit-labels: the parent is unreadable, so its owner must not be copied", joined)
	}
}
