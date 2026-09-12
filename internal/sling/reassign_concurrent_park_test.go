package sling

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// unfencedStore hides the MemStore's conditional writer: the plain path.
type unfencedStore struct{ beads.Store }

// TestReopenForReassignClearsAParkWrittenAfterTheRead: --reassign read a
// bead carrying four failures and no park; the pool's fifth failure parked
// it before the write. On a fencing store the stale write is refused, the
// row re-read and the park cleared; on a plain store the write names the
// whole record family, so the park written meanwhile is cleared too.
func TestReopenForReassignClearsAParkWrittenAfterTheRead(t *testing.T) {
	four := map[string]string{beadmeta.RoutedToMetadataKey: "worker", beadmeta.StartFailuresMetadataKey: "4", beadmeta.StartFailedAtMetadataKey: "2026-09-12T02:00:00Z", beadmeta.StartFailureMetadataKey: "boom", beadmeta.StartBackoffUntilMetadataKey: "2026-09-12T02:01:20Z"}
	park := map[string]string{beadmeta.StartFailuresMetadataKey: "5", beadmeta.ParkedAtMetadataKey: "2026-09-12T02:02:00Z", beadmeta.ParkReasonMetadataKey: "boom", beadmeta.ParkFailuresMetadataKey: "5", beadmeta.ParkIDMetadataKey: "cafe"}
	for _, tc := range []struct {
		name string
		wrap func(beads.Store) beads.Store
	}{
		{"fenced", func(s beads.Store) beads.Store { return s }},
		{"unfenced", func(s beads.Store) beads.Store { return unfencedStore{s} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := beads.NewMemStore()
			b, err := mem.Create(beads.Bead{Title: "work", Type: "task", Status: "in_progress", Assignee: "worker", Metadata: four})
			if err != nil {
				t.Fatal(err)
			}
			store := tc.wrap(mem)
			stale, err := store.Get(b.ID) // --reassign's read: four failures, no park
			if err != nil {
				t.Fatal(err)
			}
			if _, fenced := beads.ConditionalWriterFor(store); fenced != (tc.name == "fenced") {
				t.Fatalf("fixture: fenced=%v", fenced)
			}
			if err := mem.SetMetadataBatch(b.ID, park); err != nil { // the fifth failure lands
				t.Fatal(err)
			}
			changed, err := reopenForReassignInStore(store, b.ID, stale)
			if err != nil {
				t.Fatalf("reopenForReassignInStore: %v", err)
			}
			if changed == "" {
				t.Fatal("something changed")
			}
			row, _ := mem.Get(b.ID)
			for _, key := range beadmeta.WorkStartFailureMetadataKeys {
				if row.Metadata[key] != "" {
					t.Fatalf("%s survived the reassign: %v", key, row.Metadata)
				}
			}
			if row.Assignee != "" || row.Status != "open" {
				t.Fatalf("reopened: assignee=%q status=%q", row.Assignee, row.Status)
			}
		})
	}
	// Nothing to clear: nothing written (no fence, no update).
	mem := beads.NewMemStore()
	b, _ := mem.Create(beads.Bead{Title: "clean", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
	before, _ := mem.Get(b.ID)
	if changed, err := reopenForReassignInStore(mem, b.ID, before); err != nil || changed != "" {
		t.Fatalf("an open, unassigned, unparked bead is left alone: %q err=%v", changed, err)
	}
	if after, _ := mem.Get(b.ID); after.Revision != before.Revision {
		t.Fatal("no write")
	}
}
