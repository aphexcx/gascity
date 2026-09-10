package federation

import "testing"

func TestMayWriteAutomatically(t *testing.T) {
	tests := []struct {
		name     string
		labels   []string
		identity string
		wantOK   bool
		wantWhy  string
	}{
		{"not federated writes anything", []string{"owner:citadel"}, "", true, ""},
		{"not federated, blank identity", nil, "  ", true, ""},
		{"sole owner is this city", []string{"owner:citadel", "urgent"}, "citadel", true, ""},
		{"no owner label: no sole maintainer", []string{"urgent"}, "citadel", false, "owner=none this_identity=citadel rule=sole-owner"},
		{"foreign owner", []string{"owner:citadel"}, "jadegate", false, "owner=citadel this_identity=jadegate rule=sole-owner"},
		{"handoff to me does not license an automatic write", []string{"owner:citadel", "handoff:jadegate"}, "jadegate", false, "owner=citadel this_identity=jadegate rule=sole-owner"},
		{"handoff away does not revoke the owner's", []string{"owner:citadel", "handoff:jadegate"}, "citadel", true, ""},
		{"two owners, one mine", []string{"owner:citadel", "owner:jadegate"}, "citadel", false, "owner=citadel,jadegate this_identity=citadel rule=sole-owner"},
		{"bare owner label", []string{"owner:"}, "citadel", false, `owner="" this_identity=citadel rule=sole-owner`},
		{"case is not folded", []string{"owner:Citadel"}, "citadel", false, "owner=Citadel this_identity=citadel rule=sole-owner"},
		{"prefix case is not folded either", []string{"Owner:citadel"}, "citadel", false, "owner=none this_identity=citadel rule=sole-owner"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := MayWriteAutomatically(tc.labels, tc.identity)
			if ok != tc.wantOK || why != tc.wantWhy {
				t.Fatalf("MayWriteAutomatically(%v, %q) = (%v, %q), want (%v, %q)", tc.labels, tc.identity, ok, why, tc.wantOK, tc.wantWhy)
			}
		})
	}
}

// Exactly one city may write a row automatically: for every label set and
// every pair of identities, at most one identity is permitted.
func TestMayWriteAutomaticallyIsExclusive(t *testing.T) {
	labelSets := [][]string{
		nil,
		{"owner:citadel"},
		{"owner:citadel", "handoff:jadegate"},
		{"owner:citadel", "handoff:jadegate", "handoff:boomtown"},
		{"owner:citadel", "owner:jadegate"},
		{"handoff:jadegate"},
	}
	identities := []string{"citadel", "jadegate", "boomtown"}
	for _, labels := range labelSets {
		permitted := 0
		for _, id := range identities {
			if ok, _ := MayWriteAutomatically(labels, id); ok {
				permitted++
			}
		}
		if permitted > 1 {
			t.Fatalf("labels %v permit %d cities, want at most one", labels, permitted)
		}
	}
}

func TestRefusalLine(t *testing.T) {
	got := RefusalLine(" hw-1 ", "owner=citadel this_identity=jadegate rule=sole-owner")
	want := "cross-city-fence refused bead=hw-1 owner=citadel this_identity=jadegate rule=sole-owner"
	if got != want {
		t.Fatalf("RefusalLine = %q, want %q", got, want)
	}
	if ClaimRefusalLine("hw-1", "x") != RefusalLine("hw-1", "x") {
		t.Fatal("ClaimRefusalLine must be RefusalLine")
	}
}
