package sling

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestReopenForReassign_GraphStoreCopyIsTheActiveOne: a graph bead migrated
// into its class binding keeps a retained copy in the primary store. The
// ACTIVE copy — the one the pool's failed-start record and park live on — is
// the graph store's, so --reassign reopens and unparks THAT row, never the
// retained one (which would report success and leave the bead parked).
func TestReopenForReassign_GraphStoreCopyIsTheActiveOne(t *testing.T) {
	const id = "gg-migrated"
	row := func(title string) beads.Bead {
		return beads.Bead{ID: id, Title: title, Type: "task", Status: "in_progress", Assignee: "worker", Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:     "worker",
			beadmeta.ParkedAtMetadataKey:     "2026-09-12T02:00:00Z",
			beadmeta.ParkReasonMetadataKey:   "boom",
			beadmeta.ParkFailuresMetadataKey: "5",
		}}
	}
	primary := beads.NewMemStoreFrom(1, []beads.Bead{row("retained copy")}, nil)
	graph := beads.NewMemStoreFrom(1, []beads.Bead{row("active copy")}, nil)
	changed, err := reopenForReassign(id, SlingDeps{Store: primary, GraphStore: graph})
	if err != nil {
		t.Fatalf("reopenForReassign: %v", err)
	}
	if changed == "" {
		t.Fatal("the active copy was parked and in_progress: the reopen must report a change")
	}
	active, err := graph.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if active.Assignee != "" || active.Status != "open" || active.Metadata[beadmeta.ParkedAtMetadataKey] != "" {
		t.Fatalf("the graph store's copy must be reopened and unparked: status=%q assignee=%q meta=%v", active.Status, active.Assignee, active.Metadata)
	}
	retained, err := primary.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if retained.Metadata[beadmeta.ParkedAtMetadataKey] == "" || retained.Assignee != "worker" {
		t.Fatalf("the retained copy is not the one acted on: %v", retained.Metadata)
	}
	// A graph store that collapses onto the primary store (the single-store
	// city) is the primary store: the primary row is the one reopened.
	single := beads.NewMemStoreFrom(1, []beads.Bead{row("only copy")}, nil)
	if _, err := reopenForReassign(id, SlingDeps{Store: single, GraphStore: single}); err != nil {
		t.Fatalf("collapsed: %v", err)
	}
	only, _ := single.Get(id)
	if only.Metadata[beadmeta.ParkedAtMetadataKey] != "" {
		t.Fatalf("collapsed graph store: the primary row is unparked: %v", only.Metadata)
	}
	// A graph store read failure aborts, like a primary store read failure.
	if _, err := reopenForReassign(id, SlingDeps{Store: primary, GraphStore: &getErrStore{Store: beads.NewMemStore(), err: fmt.Errorf("backend unavailable")}}); err == nil {
		t.Fatal("a graph store read failure must abort the reopen")
	}
}
