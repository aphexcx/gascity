package beads

import (
	"context"
	"fmt"
	"strings"
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
			repairable, err := store.Create(Bead{Title: "repairable owner", Type: "task", Labels: []string{"gc:session"}, Metadata: map[string]string{"work_dir": "shared"}})
			if err != nil {
				t.Fatal(err)
			}
			empty := ""
			if err := store.Update(repairable.ID, UpdateOpts{Type: &empty}); err != nil {
				t.Fatal(err)
			}
			for _, query := range []MetadataPathQuery{
				{Type: "session"},
				{Label: "gc:session"},
				{Type: "session", IncludeEphemeral: true},
				{Label: "gc:session", IncludeEphemeral: true},
				{Label: "gc:session", UntypedOnly: true},
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
					if query.UntypedOnly && !page.Complete {
						t.Fatal("untyped filter must run before the backing page limit")
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
				if query.Label != "" {
					want++
				}
				if query.UntypedOnly {
					want = 1
				}
				if len(seen) != want {
					t.Fatalf("got %d owners", len(seen))
				}
			}
		})
	}
}

func TestMetadataPathCandidateCityRootSelective(t *testing.T) {
	store, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sqlite := store.(*SQLiteStore)
	t.Cleanup(func() { _ = sqlite.CloseStore() })
	for i := 0; i < 600; i++ {
		if _, err := store.Create(Bead{Title: "closed child", Type: "session", Status: "closed", Metadata: map[string]string{"work_dir": fmt.Sprintf("/city/rig/lane-%d", i)}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/city", "/city/", "."} {
		if _, err := store.Create(Bead{Title: "city owner", Type: "session", Status: "closed", Metadata: map[string]string{"work_dir": path}}); err != nil {
			t.Fatal(err)
		}
	}
	q := MetadataPathQuery{Type: "session", Keys: []string{"work_dir"}, BaseDir: "/city", Paths: []string{"/city"}}
	predicate, args := metadataPathCandidateSQL(q, func(string) string { return "m.meta_value" }, true)
	var count int
	if err := sqlite.readDB.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM metadata m WHERE m.meta_key='work_dir' AND "+predicate, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("SQL admitted %d candidates; want only 3 city owners, excluding 600 child directories", count)
	}
}

func TestMetadataPathCandidatePreservesLexicalMatches(t *testing.T) {
	store, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sqlite := store.(*SQLiteStore)
	t.Cleanup(func() { _ = sqlite.CloseStore() })
	for _, wanted := range []string{"/city/shared", "/outside/shared", "/"} {
		q := MetadataPathQuery{Type: "session", Keys: []string{"work_dir"}, BaseDir: "/city", Paths: []string{wanted}}
		predicate, args := metadataPathCandidateSQL(q, func(string) string { return "path" }, true)
		if strings.Contains(predicate, "INSTR(") {
			t.Fatal("candidate query uses substring search")
		}
		paths := []string{wanted, wanted + "/", wanted + "/.", wanted + "/retired/..", "/city/../" + wanted, "/city//shared", "./shared", "../outside/shared", "../", "unrelated/../../outside/shared", "\u2003" + wanted + "\u00a0", wanted + "\t"}
		for _, path := range paths {
			if !q.matches(Bead{Type: "session", Metadata: map[string]string{"work_dir": path}}) {
				continue
			}
			var candidate bool
			params := append(append([]any{}, args...), path)
			if err := sqlite.readDB.QueryRowContext(context.Background(), "SELECT "+predicate+" FROM (SELECT ? AS path)", params...).Scan(&candidate); err != nil {
				t.Fatal(err)
			}
			if !candidate {
				t.Errorf("lexical owner %q of %q excluded by SQL", path, wanted)
			}
		}
	}
}
