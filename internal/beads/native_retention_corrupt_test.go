package beads

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

func TestNativeRetentionCorruptRowsRefillAndReport(t *testing.T) {
	for _, limit := range []int{0, 2} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			var all []*beadslib.Issue
			for _, id := range []string{"a", "b", "c", "d"} {
				metadata := []byte("{}")
				if id == "a" || id == "b" {
					metadata = []byte("[]")
				}
				all = append(all, &beadslib.Issue{ID: id, Title: "order:sample", Status: beadslib.StatusClosed, IssueType: beadslib.TypeTask, CreatedAt: at, Metadata: metadata, Labels: []string{"order-tracking"}})
			}
			storage := &nativeDoltStorageSpy{searchIssues: func(_ context.Context, _ string, f beadslib.IssueFilter) ([]*beadslib.Issue, error) {
				if f.SkipWisps {
					return nil, nil
				}
				var selected []*beadslib.Issue
				for _, row := range all {
					if f.AfterCreatedAt == nil || row.ID > f.AfterID {
						selected = append(selected, row)
					}
				}
				return backingSortLimitForTest(selected, f), nil
			}}
			rows, err := newNativeDoltStoreForTest(storage).List(ListQuery{Status: "closed", Label: "order-tracking", Sort: SortCreatedDescIDAsc, Limit: limit, TierMode: TierBoth})
			var ids []string
			for _, row := range rows {
				ids = append(ids, row.ID)
			}
			if !slices.Equal(ids, []string{"c", "d"}) {
				t.Errorf("usable rows=%v, want c,d beyond the corrupt backing page", ids)
			}
			if !IsPartialResult(err) || !strings.Contains(fmt.Sprint(err), "a") || !strings.Contains(fmt.Sprint(err), "b") {
				t.Errorf("missing identified partial-result warning: %v", err)
			}
		})
	}
}
