package beads

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

func TestNativeListExactBackingPages(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var all []*beadslib.Issue
	for i := 0; i < 9; i++ {
		all = append(all, &beadslib.Issue{ID: fmt.Sprintf("page-%02d", i), Title: "sample", Status: beadslib.StatusClosed, IssueType: beadslib.TypeTask, CreatedAt: at.Add(-time.Duration(i/5) * time.Hour), Labels: []string{"order-tracking"}})
	}
	storage := &nativeDoltStorageSpy{searchIssues: func(_ context.Context, _ string, f beadslib.IssueFilter) ([]*beadslib.Issue, error) {
		if f.Limit != 3 || f.SortBy != "created" || f.SortDesc {
			t.Fatalf("unbounded or incompatible backing page: limit=%d sort=%s reverse=%v", f.Limit, f.SortBy, f.SortDesc)
		}
		var candidates []*beadslib.Issue
		for _, b := range all {
			if len(f.IDs) > 0 && !slices.Contains(f.IDs, b.ID) {
				continue
			}
			if f.AfterCreatedAt != nil && (b.CreatedAt.After(*f.AfterCreatedAt) || (b.CreatedAt.Equal(*f.AfterCreatedAt) && b.ID <= f.AfterID)) {
				continue
			}
			candidates = append(candidates, b)
		}
		return backingSortLimitForTest(candidates, f), nil
	}}
	store := newNativeDoltStoreForTest(storage)
	q := ListQuery{Status: "closed", Label: "order-tracking", TierMode: TierBoth, Limit: 3, Sort: SortOrder("created_desc_id_asc")}
	var got []string
	for {
		rows, err := store.List(q)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range rows {
			got = append(got, b.ID)
		}
		if len(rows) < q.Limit {
			break
		}
		last := rows[len(rows)-1]
		q.SeekAfter = &SeekBoundary{CreatedAt: last.CreatedAt, ID: last.ID}
		if len(got) > len(all) {
			t.Fatal("page did not advance")
		}
	}
	if len(got) != len(all) {
		t.Fatalf("got %v", got)
	}
	for i, id := range got {
		if id != all[i].ID {
			t.Fatalf("row %d = %s, want %s", i, id, all[i].ID)
		}
	}
}

func TestNativeListCanOmitUnusedDependencies(t *testing.T) {
	filter := nativeIssueFilterFromListQuery(ListQuery{Label: "sample", SkipDependencies: true})
	if filter.IncludeDependencies {
		t.Fatal("retention projection still hydrates unused dependency edges")
	}
	filter = nativeIssueFilterFromListQuery(ListQuery{Label: "sample"})
	if !filter.IncludeDependencies {
		t.Fatal("ordinary List lost dependency hydration")
	}
}

func TestNativeExactPageRejectsCorruptRows(t *testing.T) {
	storage := &nativeDoltStorageSpy{searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
		return []*beadslib.Issue{{ID: "corrupt", Title: "sample", Status: beadslib.StatusClosed, Metadata: []byte("invalid JSON")}}, nil
	}}
	_, err := newNativeDoltStoreForTest(storage).List(ListQuery{Status: "closed", Sort: SortCreatedDescIDAsc, Limit: 1, TierMode: TierBoth})
	if err == nil {
		t.Fatal("dropping a corrupt row silently turns a full page into a false end of history")
	}
}

func TestNativeCanonicalPageOnlyHydratesDurableCandidates(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	wisp := &beadslib.Issue{ID: "wisp", CreatedAt: at}
	durable := &beadslib.Issue{ID: "overlap", CreatedAt: at}
	canonical := &beadslib.Issue{ID: durable.ID, CreatedAt: at.Add(-time.Hour)}
	storage := &nativeDoltStorageSpy{searchIssues: func(_ context.Context, _ string, f beadslib.IssueFilter) ([]*beadslib.Issue, error) {
		if f.Limit != len(f.IDs) || f.Limit == 0 {
			t.Fatalf("lookup is not bounded by requested IDs: %+v", f)
		}
		if f.SkipWisps {
			return []*beadslib.Issue{durable}, nil
		}
		if !slices.Equal(f.IDs, []string{durable.ID}) {
			t.Fatalf("rehydrated rows with no durable copy: %v", f.IDs)
		}
		return []*beadslib.Issue{canonical}, nil
	}}
	got, err := canonicalNativePage(context.Background(), storage, []*beadslib.Issue{wisp, durable}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != wisp {
		t.Fatalf("canonical row should wait for its own creation boundary: %v", got)
	}
}
