//go:build gascity_native_beads

package beads

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/federation"
)

// TestDoltliteReadStoreReadyCarriesLabelsForTheOwnershipFence pins gf-c7n
// finding 1k (the gf-xid re-gate's Major at cmd/gc/build_desired_state.go:2061):
// the DoltLite ready read served the controller's demand count a LABEL-FREE
// projection (SkipLabels), so federation.MayClaim saw every row as unowned
// and another city's task counted as this city's demand — a seat was minted,
// the hook's authoritative claim refused it, and the pool cycled start/drain.
// The ready read now carries the labels the ownership fence judges.
func TestDoltliteReadStoreReadyCarriesLabelsForTheOwnershipFence(t *testing.T) {
	now := time.Now().UTC()
	foreign, _ := federation.OwnerLabel("jadegate")
	own, _ := federation.OwnerLabel("citadel")
	store := newDoltliteStoreWithIssues(t, []testDoltliteIssue{
		{ID: "gc-foreign", Title: "another city's task", Status: "open", IssueType: "task", CreatedAt: now, Labels: []string{foreign}},
		{ID: "gc-own", Title: "this city's task", Status: "open", IssueType: "task", CreatedAt: now.Add(time.Second), Labels: []string{own, "p1"}},
		{ID: "gc-legacy", Title: "unowned legacy work", Status: "open", IssueType: "task", CreatedAt: now.Add(2 * time.Second)},
	})

	rows, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	labels := map[string][]string{}
	for _, b := range rows {
		labels[b.ID] = b.Labels
	}
	if len(labels) != 3 {
		t.Fatalf("Ready returned %v, want the three open tasks", testBeadIDs(rows))
	}
	if got := labels["gc-foreign"]; len(got) != 1 || got[0] != foreign {
		t.Errorf("gc-foreign labels = %v, want [%s] (the ready read must carry the owner label the fence judges)", got, foreign)
	}
	if got := labels["gc-own"]; len(got) != 2 || got[0] != own || got[1] != "p1" {
		t.Errorf("gc-own labels = %v, want [%s p1] sorted", got, own)
	}
	if got := labels["gc-legacy"]; len(got) != 0 {
		t.Errorf("gc-legacy labels = %v, want none", got)
	}
	for _, b := range rows {
		ok, _ := federation.MayClaim(b.Labels, "citadel")
		if want := b.ID != "gc-foreign"; ok != want {
			t.Errorf("MayClaim(%s, citadel) = %v, want %v", b.ID, ok, want)
		}
	}
	// The per-hash ready cache serves the same labels on the second read.
	again, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready (cached): %v", err)
	}
	for _, b := range again {
		if b.ID == "gc-foreign" && (len(b.Labels) != 1 || b.Labels[0] != foreign) {
			t.Errorf("cached Ready dropped the owner label: %v", b.Labels)
		}
	}
}
