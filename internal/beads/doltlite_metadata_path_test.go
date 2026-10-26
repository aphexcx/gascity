//go:build gascity_native_beads

package beads

import (
	"context"
	"fmt"
	"testing"
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
