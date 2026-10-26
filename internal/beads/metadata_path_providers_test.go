package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

func TestBdMetadataPathPageBoundsBackingRead(t *testing.T) {
	for _, label := range []bool{false, true} {
		t.Run(fmt.Sprint(label), func(t *testing.T) {
			calls := 0
			store := NewBdStore("/fixture", func(_, _ string, args ...string) ([]byte, error) {
				calls++
				if len(args) != 3 || args[0] != "sql" || !strings.HasSuffix(args[1], "ORDER BY page_id LIMIT 256") {
					t.Fatalf("unbounded command: %v", args)
				}
				if !strings.Contains(args[1], "JSON_EXTRACT") || !strings.Contains(args[1], "INSTR(") || !strings.Contains(args[1], "wisps") {
					t.Fatalf("unscoped query: %s", args[1])
				}
				if !strings.Contains(args[1], "NOT EXISTS (SELECT 1 FROM wisps other") {
					t.Fatalf("canonical wisp must shadow colliding issue: %s", args[1])
				}
				if calls == 2 {
					if !strings.Contains(args[1], metadataSQLLiteral("73657373696F6E2D323535", false)) {
						t.Fatalf("missing last-ID cursor: %s", args[1])
					}
					return []byte("[]"), nil
				}
				var rows []map[string]string
				for i := 0; i < 256; i++ {
					rows = append(rows, map[string]string{"id": fmt.Sprintf("session-%03d", i), "issue_type": "session", "status": "closed", "metadata_json": `{"work_dir":"./shared","template":"removed"}`})
				}
				return json.Marshal(rows)
			})
			q := MetadataPathQuery{Type: "session", Keys: []string{"gc.work_dir", "work_dir"}, BaseDir: "/city", Paths: []string{"/city/shared"}}
			if label {
				q.Type = ""
				q.Label = "gc:session"
			}
			page, err := store.ReadMetadataPathPage(context.Background(), q)
			if err != nil || page.Complete || len(page.Beads) != 256 {
				t.Fatalf("page=%+v err=%v", page, err)
			}
			q.Cursor = page.Next
			page, err = store.ReadMetadataPathPage(context.Background(), q)
			if err != nil || !page.Complete || calls != 2 {
				t.Fatalf("complete=%v calls=%d err=%v", page.Complete, calls, err)
			}
		})
	}
}

func TestNativeMetadataPathPageBoundsBackingAndContinuesTies(t *testing.T) {
	instant := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	calls := 0
	storage := &nativeDoltStorageSpy{searchIssues: func(_ context.Context, _ string, f beadslib.IssueFilter) ([]*beadslib.Issue, error) {
		calls++
		// SearchIssues queries both issues and wisps before merging results.
		if f.Limit != 128 || f.SortBy != "created" || f.SortDesc || f.Ephemeral == nil || *f.Ephemeral || !f.NoIDShrink || !f.SkipLabels || !f.SkipCounts || !f.Lite {
			t.Fatalf("unbounded or unordered filter: %+v", f)
		}
		if calls == 2 {
			if f.AfterID != "session-127" || f.AfterCreatedAt == nil || !f.AfterCreatedAt.Equal(instant) {
				t.Fatalf("wrong compound cursor: %+v", f)
			}
			return []*beadslib.Issue{{ID: "session-128", IssueType: "session", Status: "closed", CreatedAt: instant, Metadata: json.RawMessage(`{"work_dir":"shared"}`)}}, nil
		}
		var rows []*beadslib.Issue
		for i := 0; i < 128; i++ {
			rows = append(rows, &beadslib.Issue{ID: fmt.Sprintf("session-%03d", i), IssueType: "session", Status: "closed", CreatedAt: instant, Metadata: json.RawMessage(`{"work_dir":"shared"}`)})
		}
		return rows, nil
	}}
	store := newNativeDoltStoreForTest(storage)
	q := MetadataPathQuery{Type: "session", Keys: []string{"work_dir"}, BaseDir: "/city", Paths: []string{"/city/shared"}}
	first, err := store.ReadMetadataPathPage(context.Background(), q)
	if err != nil || first.Complete || len(first.Beads) != 128 {
		t.Fatalf("first rows=%d complete=%v err=%v", len(first.Beads), first.Complete, err)
	}
	q.Cursor = first.Next
	last, err := store.ReadMetadataPathPage(context.Background(), q)
	if err != nil || !last.Complete || len(last.Beads) != 1 {
		t.Fatalf("last=%+v err=%v", last, err)
	}
}

func TestBdMetadataPathPageRejectsMalformedAndOversizedPages(t *testing.T) {
	for _, data := range []string{`[{"id":"session-1","metadata_json":"broken"}]`, `{"unexpected":"envelope"}`, `null`} {
		store := NewBdStore("/fixture", func(string, string, ...string) ([]byte, error) { return []byte(data), nil })
		page, err := store.ReadMetadataPathPage(context.Background(), MetadataPathQuery{Type: "session", Keys: []string{"work_dir"}, BaseDir: "/city", Paths: []string{"/city/shared"}})
		if err == nil || page.Complete {
			t.Fatalf("invalid page authorized absence: %+v %v", page, err)
		}
	}
	if page, err := finishMetadataPathPage(MetadataPathQuery{}, make([]Bead, 257), metadataPathCursor{}); err == nil || page.Complete {
		t.Fatal("oversized backing page accepted")
	}
}
