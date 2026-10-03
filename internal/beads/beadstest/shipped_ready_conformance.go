package beadstest

import (
	"context"
	"errors"
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
	t.Run("ReadyLimitExcludesShippedBeforeCounting", func(t *testing.T) {
		s := newStore()
		first, err := s.Create(beads.Bead{Title: "first work", Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		second, err := s.Create(beads.Bead{Title: "second work", Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		query := beads.ReadyQuery{Limit: 1}
		before, err := s.Ready(query)
		if err != nil || len(before) != 1 {
			t.Fatalf("Ready(limit=1) before shipment = %v, %v; want one bead", before, err)
		}
		shipped := before[0].ID
		want := first.ID
		if shipped == first.ID {
			want = second.ID
		}
		if err := s.SetMetadataBatch(shipped, map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped}); err != nil {
			t.Fatal(err)
		}
		check := func(rows []beads.Bead, err error) {
			t.Helper()
			if err != nil || len(rows) != 1 || rows[0].ID != want {
				t.Fatalf("Ready(limit=1) after shipment = %v, %v; want runnable bead %s", rows, err, want)
			}
		}
		check(s.Ready(query))
		if reader, ok := s.(beads.ContextReadyReader); ok {
			rows, err := reader.ReadyContext(context.Background(), query)
			if !errors.Is(err, beads.ErrReadyContextUnsupported) {
				check(rows, err)
			}
		}
	})
}
