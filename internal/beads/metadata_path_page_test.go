package beads

import (
	"context"
	"fmt"
	"testing"
)

func TestMetadataPathPageContract(t *testing.T) {
	for _, backend := range []string{"mem", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store Store
			if backend == "mem" {
				store = NewMemStore()
			} else {
				s, err := OpenSQLiteStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = s.(*SQLiteStore).CloseStore() })
				store = s
			}
			for i := 0; i < 520; i++ {
				_, err := store.Create(Bead{Title: fmt.Sprint(i), Type: "session", Labels: []string{"gc:session"}, Metadata: map[string]string{"work_dir": "./shared", "template": "removed"}})
				if err != nil {
					t.Fatal(err)
				}
			}
			for _, metadata := range []map[string]string{
				{"work_dir_canonical": "\u2003", "work_dir": " /city/retired/../shared "},
				{"work_dir_canonical": "/city/shared/../shared", "work_dir": "/elsewhere"},
				{"work_dir_canonical": "/elsewhere", "work_dir": "/city/shared"},
				{"work_dir": "/elsewhere/shared"},
			} {
				if _, err := store.Create(Bead{Title: "lexical path", Type: "session", Labels: []string{"gc:session"}, Metadata: metadata}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.Create(Bead{Title: "ephemeral owner", Type: "session", Ephemeral: true, Labels: []string{"gc:session"}, Metadata: map[string]string{"work_dir": "shared"}}); err != nil {
				t.Fatal(err)
			}
			for _, query := range []MetadataPathQuery{
				{Type: "session"},
				{Label: "gc:session"},
				{Type: "session", IncludeEphemeral: true},
				{Label: "gc:session", IncludeEphemeral: true},
			} {
				query.Keys = []string{"work_dir_canonical", "work_dir"}
				query.BaseDir, query.Paths = "/city", []string{"/city/shared"}
				reader, ok := store.(MetadataPathPageReader)
				if !ok {
					t.Fatalf("%T lacks bounded metadata paging", store)
				}
				seen := map[string]bool{}
				for pages := 0; ; pages++ {
					if pages > 5 {
						t.Fatal("page did not advance")
					}
					page, err := reader.ReadMetadataPathPage(context.Background(), query)
					if err != nil {
						t.Fatal(err)
					}
					if len(page.Beads) > 256 {
						t.Fatalf("page has %d rows", len(page.Beads))
					}
					for _, b := range page.Beads {
						if seen[b.ID] {
							t.Fatalf("duplicate %s", b.ID)
						}
						seen[b.ID] = true
					}
					if page.Complete {
						break
					}
					if page.Next == "" || page.Next == query.Cursor {
						t.Fatal("incomplete page has no progress")
					}
					query.Cursor = page.Next
				}
				want := 522
				if query.IncludeEphemeral {
					want++
				}
				if len(seen) != want {
					t.Fatalf("got %d owners", len(seen))
				}
			}
		})
	}
}
