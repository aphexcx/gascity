package federation_test

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/federation"
)

func TestOwnerLabelIsUnsetWithoutAnIdentity(t *testing.T) {
	for _, identity := range []string{"", "  "} {
		if label, ok := federation.OwnerLabel(identity); ok || label != "" {
			t.Fatalf("OwnerLabel(%q) = (%q, %v), want (\"\", false)", identity, label, ok)
		}
	}
	label, ok := federation.OwnerLabel("citadel")
	if !ok || label != "owner:citadel" {
		t.Fatalf("OwnerLabel(citadel) = (%q, %v), want (owner:citadel, true)", label, ok)
	}
}

func TestEnsureOwnerLabelAppendsOnlyWhenNoOwnerIsPresent(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   []string
	}{
		{"nil labels", nil, []string{"owner:citadel"}},
		{"user labels keep their order", []string{"b", "a"}, []string{"b", "a", "owner:citadel"}},
		{"an explicit foreign owner is respected", []string{"owner:jadegate"}, []string{"owner:jadegate"}},
		{"the same owner is not duplicated", []string{"x", "owner:citadel"}, []string{"x", "owner:citadel"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := federation.EnsureOwnerLabel(tt.labels, "owner:citadel")
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("EnsureOwnerLabel(%v) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}

func TestEnsureOwnerLabelDoesNotAliasItsInput(t *testing.T) {
	in := make([]string, 1, 4)
	in[0] = "a"
	got := federation.EnsureOwnerLabel(in, "owner:citadel")
	got[0] = "changed"
	if in[0] != "a" {
		t.Fatal("EnsureOwnerLabel returned a slice that aliases its input")
	}
}

func TestOwnerOfReadsTheFirstOwnerLabel(t *testing.T) {
	if got := federation.OwnerOf(nil); got != "" {
		t.Fatalf("OwnerOf(nil) = %q, want empty", got)
	}
	if got := federation.OwnerOf([]string{"pool:x", "hold:mayor"}); got != "" {
		t.Fatalf("OwnerOf(no owner) = %q, want empty", got)
	}
	if got := federation.OwnerOf([]string{"pool:x", "owner:jadegate", "owner:citadel"}); got != "jadegate" {
		t.Fatalf("OwnerOf = %q, want jadegate", got)
	}
	if !federation.HasOwnerLabel([]string{"owner:citadel"}) || federation.HasOwnerLabel([]string{"owners:x"}) {
		t.Fatal("HasOwnerLabel must match the owner: prefix exactly")
	}
}

func TestValidateIdentity(t *testing.T) {
	for _, ok := range []string{"", "citadel", "jade-gate", "a1", "0", "x-"} {
		if err := federation.ValidateIdentity(ok); err != nil {
			t.Errorf("ValidateIdentity(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"Citadel", "-x", "city name", "under_score", "a.b", "café", " citadel"} {
		if err := federation.ValidateIdentity(bad); err == nil {
			t.Errorf("ValidateIdentity(%q) = nil, want an error", bad)
		}
	}
}

// The refusal side (jadegate's half) reads handoff:<identity> to let an
// explicit cross-city sling through; the spelling lives here with owner:.
func TestHandoffLabels(t *testing.T) {
	if got := federation.HandoffLabel("citadel"); got != "handoff:citadel" {
		t.Fatalf("HandoffLabel(citadel) = %q, want handoff:citadel", got)
	}
	labels := []string{"owner:jadegate", "handoff:citadel", "pool:x", "handoff:boomtown"}
	if got := federation.HandoffTargets(labels); !reflect.DeepEqual(got, []string{"citadel", "boomtown"}) {
		t.Fatalf("HandoffTargets = %v, want [citadel boomtown]", got)
	}
	if !federation.HasHandoffTo(labels, "citadel") || federation.HasHandoffTo(labels, "jadegate") {
		t.Fatal("HasHandoffTo must match the identity exactly")
	}
	if federation.HasHandoffTo(labels, "") || federation.HandoffTargets(nil) != nil {
		t.Fatal("an empty identity never matches and no labels yield no targets")
	}
}
