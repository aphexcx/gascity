package beads_test

import (
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
