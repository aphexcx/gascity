package beadstest

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// RunShippedReadyConformance verifies that shipped work stays out of Ready
// until its outcome is explicitly reset. Snapshot read providers may seed
// their fixtures through Create and SetMetadataBatch test adapters.
func RunShippedReadyConformance(t *testing.T, newStore func() beads.Store) {
	t.Helper()
	t.Run("ReadyExcludesShippedOpenWorkAndAllowsExplicitReset", func(t *testing.T) {
		s := newStore()
		work, err := s.Create(beads.Bead{Title: "ready for review", Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetMetadataBatch(work.ID, map[string]string{
			beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
			beadmeta.RoutedToMetadataKey:    "builder",
		}); err != nil {
			t.Fatal(err)
		}
		ready, err := s.Ready()
		if err != nil {
			t.Fatal(err)
		}
		if len(ready) != 0 {
			t.Fatalf("Ready() = %v, want no duplicate execution of shipped open work", titlesOf(ready))
		}
		if err := s.SetMetadataBatch(work.ID, map[string]string{beadmeta.WorkOutcomeMetadataKey: ""}); err != nil {
			t.Fatal(err)
		}
		ready, err = s.Ready()
		if err != nil {
			t.Fatal(err)
		}
		if len(ready) != 1 || ready[0].ID != work.ID {
			t.Fatalf("Ready() = %v, want explicitly reset work %s", titlesOf(ready), work.ID)
		}
	})
}
