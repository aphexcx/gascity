package beads

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

func TestNativeExactPagesFillAfterResidualParentFiltering(t *testing.T) {
	for _, multipleParents := range []bool{false, true} {
		t.Run(fmt.Sprintf("multiple_parents_%v", multipleParents), func(t *testing.T) {
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			var candidates []*beadslib.Issue
			for i := range 5 {
				b := &beadslib.Issue{ID: fmt.Sprintf("root.%02d", i), Status: beadslib.StatusOpen, CreatedAt: at}
				if multipleParents {
					b.Dependencies = []*beadslib.Dependency{
						{IssueID: b.ID, DependsOnID: "other", Type: beadslib.DepParentChild},
						{IssueID: b.ID, DependsOnID: "root", Type: beadslib.DepParentChild},
					}
				}
				candidates = append(candidates, b)
			}
			for i := range 3 {
				id := fmt.Sprintf("child-%02d", i)
				candidates = append(candidates, &beadslib.Issue{ID: id, Status: beadslib.StatusOpen, CreatedAt: at.Add(-time.Hour), Dependencies: []*beadslib.Dependency{{IssueID: id, DependsOnID: "root", Type: beadslib.DepParentChild}}})
			}
			calls := 0
			storage := &nativeDoltStorageSpy{searchIssues: func(_ context.Context, _ string, f beadslib.IssueFilter) ([]*beadslib.Issue, error) {
				calls++
				if f.Limit != 2 || f.ParentID == nil || *f.ParentID != "root" {
					t.Fatalf("backing query lost its bound or parent: limit=%d parent=%v", f.Limit, f.ParentID)
				}
				var page []*beadslib.Issue
				for _, b := range candidates {
					if f.AfterCreatedAt != nil && (b.CreatedAt.After(*f.AfterCreatedAt) || (b.CreatedAt.Equal(*f.AfterCreatedAt) && b.ID <= f.AfterID)) {
						continue
					}
					page = append(page, b)
				}
				return backingSortLimitForTest(page, f), nil
			}}
			store := newNativeDoltStoreForTest(storage)
			q := ListQuery{ParentID: "root", TierMode: TierBoth, Sort: SortCreatedDescIDAsc, Limit: 2}
			var ids []string
			for {
				page, err := store.List(q)
				if err != nil {
					t.Fatal(err)
				}
				for _, b := range page {
					ids = append(ids, b.ID)
				}
				if len(page) < q.Limit {
					break
				}
				last := page[len(page)-1]
				q.SeekAfter = &SeekBoundary{CreatedAt: last.CreatedAt, ID: last.ID}
				if len(ids) > 3 {
					t.Fatal("page did not advance")
				}
			}
			if fmt.Sprint(ids) != "[child-00 child-01 child-02]" {
				t.Fatalf("live children disappeared after residual parent filtering: %v (calls=%d)", ids, calls)
			}
		})
	}
}

func TestNativeExactPagePropagatesLaterReadFailure(t *testing.T) {
	want := errors.New("later page unavailable")
	calls := 0
	storage := &nativeDoltStorageSpy{searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
		calls++
		if calls == 1 {
			return []*beadslib.Issue{{ID: "root.0", Status: beadslib.StatusOpen, CreatedAt: time.Unix(1, 0)}}, nil
		}
		return nil, want
	}}
	_, err := newNativeDoltStoreForTest(storage).List(ListQuery{ParentID: "root", Sort: SortCreatedDescIDAsc, TierMode: TierBoth, Limit: 1})
	if !errors.Is(err, want) {
		t.Fatalf("error=%v, want later-page failure", err)
	}
}
