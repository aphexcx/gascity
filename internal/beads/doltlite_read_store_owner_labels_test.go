//go:build gascity_native_beads

package beads

import (
	"fmt"
	"reflect"
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

// TestDoltliteReadStoreReadyHydratesLabelsBeyondTheVariableLimit pins the
// codex round-1 finding on 1k: hydrating labels bound one SQL variable per
// returned row, and modernc SQLite refuses a statement with more than 32766
// variables, so an unlimited ready read of a queue past that size — the
// controller's demand read — failed with "too many SQL variables" where the
// label-free read had succeeded. The ids are now bound in chunks.
func TestDoltliteReadStoreReadyHydratesLabelsBeyondTheVariableLimit(t *testing.T) {
	store := newDoltliteStoreWithIssues(t, nil)
	writer := openTestDoltliteWriter(t, store.db)
	defer writer.Close() //nolint:errcheck // test cleanup
	const rows = 32766 + 10
	tx, err := writer.Begin()
	if err != nil {
		t.Fatal(err)
	}
	issue, err := tx.Prepare(`INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, assignee, description, design, acceptance_criteria, notes, metadata, ephemeral, no_history) VALUES (?, ?, 'open', 'task', 2, ?, ?, '', '', '', '', '', '{}', 0, 0)`)
	if err != nil {
		t.Fatal(err)
	}
	label, err := tx.Prepare(`INSERT INTO labels (issue_id, label) VALUES (?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Format(time.RFC3339Nano)
	for i := 0; i < rows; i++ {
		id := fmt.Sprintf("gc-big-%05d", i)
		if _, err := issue.Exec(id, "big queue "+id, created, created); err != nil {
			t.Fatal(err)
		}
		if _, err := label.Exec(id, "owner:citadel"); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready over %d rows: %v", rows, err)
	}
	if len(got) != rows {
		t.Fatalf("Ready returned %d rows, want %d", len(got), rows)
	}
	for _, b := range got {
		if len(b.Labels) != 1 || b.Labels[0] != "owner:citadel" {
			t.Fatalf("%s labels = %v, want [owner:citadel] on every row", b.ID, b.Labels)
		}
	}
}

// TestDoltliteHydrateLabelsChunksTheIDs pins the chunking itself with a
// chunk smaller than the row set: every row's labels arrive, none twice.
func TestDoltliteHydrateLabelsChunksTheIDs(t *testing.T) {
	saved := doltliteLabelHydrationChunk
	doltliteLabelHydrationChunk = 2
	defer func() { doltliteLabelHydrationChunk = saved }()
	now := time.Now().UTC()
	var issues []testDoltliteIssue
	for i := 0; i < 5; i++ {
		issues = append(issues, testDoltliteIssue{ID: fmt.Sprintf("gc-chunk-%d", i), Title: "chunked", Status: "open", IssueType: "task", CreatedAt: now.Add(time.Duration(i) * time.Second), Labels: []string{"p1", fmt.Sprintf("n:%d", i)}})
	}
	store := newDoltliteStoreWithIssues(t, issues)
	got, err := store.Ready()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("Ready returned %d rows, want 5", len(got))
	}
	for i, b := range got {
		want := []string{fmt.Sprintf("n:%d", i), "p1"}
		if !reflect.DeepEqual(b.Labels, want) {
			t.Errorf("%s labels = %v, want %v", b.ID, b.Labels, want)
		}
	}
}
