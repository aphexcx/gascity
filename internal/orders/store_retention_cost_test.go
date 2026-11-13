package orders

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// Counts rows returned by the backing command, before BdStore cuts the page.
func TestClosedRetentionFallbackBackingCost(t *testing.T) {
	for _, shape := range []struct{ n, orders int }{{500, 5}, {10001, 1}, {61988, 67}} {
		t.Run(fmt.Sprintf("%d_rows_%d_orders", shape.n, shape.orders), func(t *testing.T) {
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			type issue struct {
				ID        string    `json:"id"`
				Title     string    `json:"title"`
				Status    string    `json:"status"`
				IssueType string    `json:"issue_type"`
				CreatedAt time.Time `json:"created_at"`
				Labels    []string  `json:"labels"`
			}
			rows := make([]issue, shape.n)
			for i := range rows {
				name := fmt.Sprintf("sample-%02d", i%shape.orders)
				rows[i] = issue{fmt.Sprintf("sample-%06d", i), "order:" + name, "closed", "task", at, []string{"order-tracking", "order-run:" + name}}
			}
			fetched, calls := 0, 0
			store := beads.NewBdStore(t.TempDir(), func(_, _ string, args ...string) ([]byte, error) {
				calls++
				if args[0] == "query" {
					return []byte("[]"), nil
				}
				if args[0] != "list" {
					return nil, fmt.Errorf("unexpected command %v", args)
				}
				label := ""
				for _, arg := range args {
					if strings.HasPrefix(arg, "--label=") {
						label = strings.TrimPrefix(arg, "--label=")
					}
				}
				var selected []issue
				for _, row := range rows {
					if label == "" || slices.Contains(row.Labels, label) {
						selected = append(selected, row)
					}
				}
				fetched += len(selected)
				return json.Marshal(selected)
			})
			runs, err := NewStore(beads.OrdersStore{Store: store}).ClosedRunsForRetention()
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			for _, run := range runs {
				if seen[run.ID] {
					t.Fatalf("duplicate %s", run.ID)
				}
				seen[run.ID] = true
			}
			if len(seen) != len(rows) {
				t.Fatalf("got %d runs, want %d", len(seen), len(rows))
			}
			t.Logf("backing rows=%d N=%d calls=%d", fetched, shape.n, calls)
			if fetched > shape.n || calls > 2 {
				t.Fatalf("fallback repeated the backing fetch: rows=%d calls=%d; want at most N=%d rows and 2 tier commands", fetched, calls, shape.n)
			}
		})
	}
}
