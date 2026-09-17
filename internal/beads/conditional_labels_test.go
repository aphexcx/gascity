package beads_test

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

// In these stores labels and their revision share one atomic persistence unit.
// Two backfill writers using the same snapshot must never both add an owner.
func TestWholeBeadStoresFenceConditionalLabels(t *testing.T) {
	for _, kind := range []string{"memory", "file"} {
		t.Run(kind, func(t *testing.T) {
			var stores [2]beads.Store
			if kind == "memory" {
				stores[0] = beads.NewMemStore()
				stores[1] = stores[0]
			} else {
				path := filepath.Join(t.TempDir(), "beads.json")
				for i := range stores {
					var err error
					stores[i], err = beads.OpenFileStore(fsys.OSFS{}, path)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			b, err := stores[0].Create(beads.Bead{Title: "owner candidate", Labels: []string{"keep"}})
			if err != nil {
				t.Fatal(err)
			}
			var results [2]error
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i, owner := range []string{"owner:first", "owner:second"} {
				wg.Add(1)
				go func(i int, owner string) {
					defer wg.Done()
					<-start
					results[i] = stores[i].(beads.ConditionalWriter).UpdateIfMatch(b.ID, b.Revision, beads.UpdateOpts{Labels: []string{owner}})
				}(i, owner)
			}
			close(start)
			wg.Wait()
			wins, stale := 0, 0
			for _, err := range results {
				var mismatch *beads.PreconditionFailedError
				switch {
				case err == nil:
					wins++
				case errors.As(err, &mismatch):
					stale++
				default:
					t.Fatalf("conditional label: %v", err)
				}
			}
			if wins != 1 || stale != 1 {
				t.Fatalf("wins/stale = %d/%d, want 1/1", wins, stale)
			}
			got, err := stores[0].Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Labels) != 2 || got.Labels[0] != "keep" || got.Revision == b.Revision {
				t.Fatalf("labels/revision after race = %v/%d", got.Labels, got.Revision)
			}
		})
	}
}
