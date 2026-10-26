//go:build gascity_native_beads

package beads

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestDoltliteMetadataPathPagesPreserveLegacyAndDurableWisps(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()
	writer := openTestDoltliteWriter(t, store.db)
	defer writer.Close() //nolint:errcheck
	for i := 0; i < 520; i++ {
		table, labels, deps := "issues", "labels", "dependencies"
		if i%2 == 1 {
			table, labels, deps = "wisps", "wisp_labels", "wisp_dependencies"
		}
		insertTestDoltliteIssue(t, writer, table, labels, deps, testDoltliteIssue{ID: fmt.Sprintf("owner-%04d", i), Title: "owner", IssueType: "session", Status: "closed", Labels: []string{"gc:session"}, Metadata: map[string]string{"work_dir": "./retired/../shared", "template": "removed"}, NoHistory: i%2 == 1})
	}
	// A row outside this leg must not shadow its durable wisp owner.
	insertTestDoltliteIssue(t, writer, "issues", "labels", "dependencies", testDoltliteIssue{ID: "collision", Title: "unrelated", IssueType: "task", Status: "closed"})
	insertTestDoltliteIssue(t, writer, "wisps", "wisp_labels", "wisp_dependencies", testDoltliteIssue{ID: "collision", Title: "owner", IssueType: "session", Status: "closed", Labels: []string{"gc:session"}, Metadata: map[string]string{"work_dir": "shared"}, NoHistory: true})
	for _, q := range []MetadataPathQuery{{Type: "session"}, {Label: "gc:session"}} {
		q.Keys = []string{"gc.work_dir", "work_dir"}
		q.BaseDir = "/city"
		q.Paths = []string{"/city/shared"}
		count := 0
		for pages := 0; ; pages++ {
			if pages > 4 {
				t.Fatal("paging did not finish")
			}
			page, err := store.ReadMetadataPathPage(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Beads) > 256 {
				t.Fatal("page exceeds cap")
			}
			count += len(page.Beads)
			if page.Complete {
				break
			}
			q.Cursor = page.Next
		}
		if count != 521 {
			t.Fatalf("owners=%d want 521", count)
		}
	}
}

func TestDoltliteMetadataLedgerPagesResolveCanonicalCollisionsBeforeLimit(t *testing.T) {
	store, closeStore := newTestDoltliteReadStore(t)
	defer closeStore()
	writer := openTestDoltliteWriter(t, store.db)
	defer writer.Close() //nolint:errcheck
	now := time.Now().UTC()
	for i := 0; i < 257; i++ {
		insertTestDoltliteIssue(t, writer, "issues", "labels", "dependencies", testDoltliteIssue{ID: fmt.Sprintf("owner-%04d", i), Title: "owner", IssueType: "session", Status: "closed", CreatedAt: now.Add(-time.Duration(i) * time.Second), Labels: []string{"gc:session"}, Metadata: map[string]string{"work_dir": "shared"}})
	}
	// A much older canonical wisp must not move the cursor past unread issues.
	insertTestDoltliteIssue(t, writer, "wisps", "wisp_labels", "wisp_dependencies", testDoltliteIssue{ID: "owner-0000", Title: "canonical", IssueType: "session", Status: "closed", CreatedAt: now.Add(-time.Hour), NoHistory: true, Labels: []string{"gc:session"}, Metadata: map[string]string{"work_dir": "shared", "canonical": "yes"}})
	insertTestDoltliteIssue(t, writer, "wisps", "wisp_labels", "wisp_dependencies", testDoltliteIssue{ID: "ephemeral", Title: "ephemeral", IssueType: "session", Status: "closed", Ephemeral: true, Labels: []string{"gc:session"}, Metadata: map[string]string{"work_dir": "shared"}})
	insertTestDoltliteIssue(t, writer, "issues", "labels", "dependencies", testDoltliteIssue{ID: "repairable", Title: "untyped owner", IssueType: "task", Status: "closed", Labels: []string{"gc:session"}, Metadata: map[string]string{"work_dir": "shared"}})
	insertTestDoltliteIssue(t, writer, "issues", "labels", "dependencies", testDoltliteIssue{ID: "repairable-null", Title: "null type owner", IssueType: "task", Status: "closed", Labels: []string{"gc:session"}, Metadata: map[string]string{"work_dir": "shared"}})
	// Restricting the label leg must not resurrect the stale untyped copy of
	// the typed canonical wisp. The independent repairable row remains visible.
	if _, err := writer.Exec("UPDATE issues SET issue_type='' WHERE id IN ('repairable', 'owner-0000')"); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec("UPDATE issues SET issue_type=NULL WHERE id='repairable-null'"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []MetadataPathQuery{{Type: "session"}, {Label: "gc:session"}, {Type: "session", IncludeEphemeral: true}, {Label: "gc:session", IncludeEphemeral: true}, {Label: "gc:session", UntypedOnly: true}} {
		q.Keys, q.BaseDir, q.Paths = []string{"work_dir"}, "/city", []string{"/city/shared"}
		seen := map[string]Bead{}
		for pageNum := 0; ; pageNum++ {
			if pageNum > 2 {
				t.Fatal("paging did not finish")
			}
			cursor, err := decodeMetadataPathCursor(q.Cursor)
			if err != nil {
				t.Fatal(err)
			}
			query := metadataPathLedgerSQL(q, cursor, true, []metadataPathTable{{"wisps", "wisp_labels", "COALESCE(i.ephemeral,0)"}, {"issues", "labels", "COALESCE(i.ephemeral,0)"}})
			rows, err := store.db.QueryContext(context.Background(), query)
			if err != nil {
				t.Fatal(err)
			}
			page, err := readMetadataPathSQLRows(q, rows, cursor, nil)
			_ = rows.Close()
			if err != nil {
				t.Fatal(err)
			}
			if q.UntypedOnly && !page.Complete {
				t.Fatal("typed rows consumed the repairable backing page")
			}
			for _, b := range page.Beads {
				if _, exists := seen[b.ID]; exists {
					t.Fatalf("duplicate %s", b.ID)
				}
				seen[b.ID] = b
			}
			if page.Complete {
				break
			}
			q.Cursor = page.Next
		}
		if q.UntypedOnly {
			if len(seen) != 2 || seen["repairable"].ID == "" || seen["repairable-null"].ID == "" {
				t.Fatalf("repairable owners=%v; typed canonical row must still shadow its stale untyped copy", seen)
			}
			continue
		}
		want := 257
		if q.Label != "" {
			want += 2
		}
		if q.IncludeEphemeral {
			want++
			if !seen["ephemeral"].Ephemeral {
				t.Fatal("ephemeral owner lost")
			}
		}
		if len(seen) != want || seen["owner-0000"].Metadata["canonical"] != "yes" || seen["owner-0256"].ID == "" {
			t.Fatalf("canonical ownership incomplete: %d owners, first=%+v, last=%+v", len(seen), seen["owner-0000"], seen["owner-0256"])
		}
	}
}
