package federation_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/federation"
)

// TestMayClaim is the matching-rule spec for the refusal side of the owner
// label (jadegate jg-66rdw8, citadel gp-0uj): the one rule the hook claim,
// the reconciler's orphan release and the retired-session re-home all apply.
func TestMayClaim(t *testing.T) {
	cases := []struct {
		name     string
		labels   []string
		identity string
		ok       bool
	}{
		// The incident shape: citadel's bead seen from jadegate.
		{"foreign owner refused", []string{"owner:citadel"}, "jadegate", false},
		{"foreign owner among other labels refused", []string{"pool:x", "owner:citadel", "hold:mayor"}, "jadegate", false},
		// The sanctioned override: an explicit cross-city sling.
		{"foreign owner with handoff to this city ok", []string{"owner:citadel", "handoff:jadegate"}, "jadegate", true},
		{"foreign owner with handoff to a third city refused", []string{"owner:citadel", "handoff:boomtown"}, "jadegate", false},
		{"several handoffs, one naming this city, ok", []string{"owner:citadel", "handoff:boomtown", "handoff:jadegate"}, "jadegate", true},
		{"handoff alone (no owner) ok", []string{"handoff:jadegate"}, "jadegate", true},
		{"empty handoff value does not override", []string{"owner:citadel", "handoff:"}, "jadegate", false},
		// Own work and legacy work.
		{"own city ok", []string{"owner:jadegate"}, "jadegate", true},
		{"own city twice ok", []string{"owner:jadegate", "owner:jadegate"}, "jadegate", true},
		{"unlabeled ok", nil, "jadegate", true},
		{"empty labels ok", []string{}, "jadegate", true},
		{"unrelated labels ok", []string{"hold:mayor", "ownership-review", "owner", "owners:jadegate"}, "jadegate", true},
		// Two owners is a conflict, not a license; a bare owner: is malformed.
		{"mixed owner set refused", []string{"owner:citadel", "owner:jadegate"}, "jadegate", false},
		{"mixed owner set with handoff ok", []string{"owner:citadel", "owner:jadegate", "handoff:jadegate"}, "jadegate", true},
		{"malformed empty owner value refused", []string{"owner:"}, "jadegate", false},
		{"malformed empty owner value plus own owner refused", []string{"owner:", "owner:jadegate"}, "jadegate", false},
		// Exact lowercase strings: the identity is validated lowercase at config
		// load and the emit side spells the label verbatim, so neither the
		// prefix nor the value is case-folded or trimmed.
		{"owner value differing in case from the identity is foreign", []string{"owner:Jadegate"}, "jadegate", false},
		{"miscased prefix is not an owner label", []string{"Owner:citadel"}, "jadegate", true},
		{"padded label is not an owner label", []string{" owner:citadel"}, "jadegate", true},
		{"handoff differing in case does not override", []string{"owner:citadel", "handoff:Jadegate"}, "jadegate", false},
		{"identity is trimmed before comparing", []string{"owner:jadegate"}, " jadegate ", true},
		// Not federated: the fence is off.
		{"no identity, foreign owner ok", []string{"owner:citadel"}, "", true},
		{"no identity, malformed owner ok", []string{"owner:"}, "", true},
		{"blank identity, foreign owner ok", []string{"owner:citadel"}, "   ", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := federation.MayClaim(tc.labels, tc.identity)
			if ok != tc.ok {
				t.Fatalf("MayClaim(%q, %q) = (%v, %q), want ok=%v", tc.labels, tc.identity, ok, reason, tc.ok)
			}
			if ok && reason != "" {
				t.Fatalf("an allowed claim must carry no reason, got %q", reason)
			}
			if !ok && reason == "" {
				t.Fatalf("a refusal must say why")
			}
		})
	}
}

// TestMayClaimReasonNamesEveryFact pins the greppable reason: the owner
// value(s) as the bead spells them, this identity, and the exact handoff label
// that would have permitted the claim.
func TestMayClaimReasonNamesEveryFact(t *testing.T) {
	ok, reason := federation.MayClaim([]string{"p1", "owner:citadel", "owner:boomtown"}, "jadegate")
	if ok {
		t.Fatal("want refusal")
	}
	if want := "owner=citadel,boomtown this_identity=jadegate missing=handoff:jadegate"; reason != want {
		t.Fatalf("reason = %q, want %q", reason, want)
	}
	if _, reason := federation.MayClaim([]string{"owner:"}, "jadegate"); !strings.HasPrefix(reason, `owner="" `) {
		t.Fatalf("a malformed empty owner value must be visible in the reason, got %q", reason)
	}
}

// TestClaimRefusalLineIsOneGreppableLine pins the shared log line every
// refusing call site writes, and that hostile values — a bead id or label with
// a newline, quotes or spaces — cannot break it or forge a second line.
func TestClaimRefusalLineIsOneGreppableLine(t *testing.T) {
	_, reason := federation.MayClaim([]string{"owner:citadel"}, "jadegate")
	if got, want := federation.ClaimRefusalLine("hw-57b63", reason), "cross-city-fence refused bead=hw-57b63 owner=citadel this_identity=jadegate missing=handoff:jadegate"; got != want {
		t.Fatalf("ClaimRefusalLine = %q, want %q", got, want)
	}

	_, hostile := federation.MayClaim([]string{"owner:cita\ndel", `owner:boom"town`, "owner:two words"}, "jade gate")
	line := federation.ClaimRefusalLine("hw-evil\nforged=line", hostile)
	if strings.ContainsAny(line, "\n\r") {
		t.Fatalf("line contains a line break: %q", line)
	}
	for _, want := range []string{`bead="hw-evil\nforged=line"`, `owner="cita\ndel","boom\"town","two words"`, `this_identity="jade gate"`, `missing="handoff:jade gate"`} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q lacks %q", line, want)
		}
	}
}

// TestOwnersReadsEveryOwnerLabelInOrder: OwnerOf reads the first owner label;
// the refusal rule needs all of them, so a two-owner bead is refused rather
// than resolved by label order.
func TestOwnersReadsEveryOwnerLabelInOrder(t *testing.T) {
	if got := federation.Owners(nil); got != nil {
		t.Fatalf("Owners(nil) = %v, want nil", got)
	}
	if got := federation.Owners([]string{"pool:x", "hold:mayor"}); got != nil {
		t.Fatalf("Owners(no owner) = %v, want nil", got)
	}
	got := federation.Owners([]string{"pool:x", "owner:jadegate", "owner:", "owner:citadel"})
	if want := []string{"jadegate", "", "citadel"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Owners = %v, want %v", got, want)
	}
}
