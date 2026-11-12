package main

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
)

type retentionBoundedStore struct{ beads.Store }

func (s retentionBoundedStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Status == "closed" && (q.Label == "order-tracking" || strings.HasPrefix(q.Label, "order-run:")) && (q.Limit <= 0 || q.Limit > 1000) {
		return nil, fmt.Errorf("unbounded retention query: limit %d", q.Limit)
	}
	return s.Store.List(q)
}

type retentionLiveGuardStore struct{ beads.Store }

func (s retentionLiveGuardStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if (len(q.Metadata) > 0 || q.ParentID != "") && (q.Limit <= 0 || q.Limit > 1000) {
		return nil, errors.New("live-descendant guard issued an unbounded read")
	}
	if len(q.Metadata) > 0 && q.IncludesClosed() {
		return nil, errors.New("live-descendant guard read closed history")
	}
	return s.Store.List(q)
}

func TestRetentionLiveGuardSkipsClosedHistoryWithoutStrandingWork(t *testing.T) {
	for _, status := range []string{"open", "in_progress", "blocked", "deferred"} {
		t.Run(status, func(t *testing.T) {
			rows := []beads.Bead{
				{ID: "root", Status: "closed"},
				{ID: "middle", Status: "closed", ParentID: "root"},
				{ID: "live", Status: status, ParentID: "middle"},
			}
			store := retentionLiveGuardStore{beads.NewMemStoreFrom(3, rows, nil)}
			if err := deleteWorkflowBead(store, "root"); !errors.Is(err, errWorkflowDeleteLiveDescendants) {
				t.Fatalf("delete error=%v, want live-descendant refusal", err)
			}
		})
	}
}

func TestRetentionLiveGuardDoesNotStopAtPageBoundary(t *testing.T) {
	for _, metadata := range []bool{false, true} {
		t.Run(fmt.Sprintf("metadata_%v", metadata), func(t *testing.T) {
			rows := []beads.Bead{{ID: "root", Status: "closed"}}
			for i := 0; i < 1001; i++ {
				b := beads.Bead{ID: fmt.Sprintf("child-%04d", i), Status: "open", CreatedAt: time.Unix(1, 0)}
				if metadata {
					b.Metadata = map[string]string{beadmeta.RootBeadIDMetadataKey: "root"}
				} else {
					b.ParentID = "root"
				}
				rows = append(rows, b)
			}
			store := retentionLiveGuardStore{beads.NewMemStoreFrom(len(rows), rows, nil)}
			ids, err := storeOpenDescendantIDs(store, "root", func(b beads.Bead) bool { return b.ID != "child-1000" })
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(ids, []string{"child-1000"}) {
				t.Fatalf("guard lost live work after the first page: %v", ids)
			}
		})
	}
}

// The old whole-history algorithm is the oracle. The count, complete sweep,
// and interrupted/resumed sweep must select exactly the same IDs.
func TestRetentionPagingPreservesEligibleSet(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	policy := orderTrackingRetentionPolicy{retainLast: 10, deleteAfterClose: 7 * 24 * time.Hour}
	var rows []beads.Bead
	for group, count := range []int{5, 10, 107, 41, 23, 25} {
		name := fmt.Sprintf("sample-%d", group)
		for i := 0; i < count; i++ {
			created := now.Add(-time.Duration(24*20+i/3) * time.Hour)
			updated := now.Add(-time.Duration(24*10-i%97) * time.Hour)
			if i%11 == 0 {
				updated = now.Add(-policy.deleteAfterClose)
			}
			if i%13 == 0 {
				updated = now.Add(-time.Hour)
			}
			if i%17 == 0 {
				updated = time.Time{}
			}
			labels := []string{"order-tracking", "order-run:" + name}
			title := "order:" + name
			if group == 4 {
				labels = labels[:1]
			} // Historical title-only records.
			if group == 5 {
				labels = labels[:1]
				title = "legacy"
			}
			if group == 3 && i%5 == 0 {
				labels = append(labels, "order-run:sample-2")
			}
			rows = append(rows, beads.Bead{ID: fmt.Sprintf("record-%d-%05d", group, i), Title: title, Status: "closed", CreatedAt: created, UpdatedAt: updated, Labels: labels, Ephemeral: i%2 == 0})
		}
	}
	rows = append(rows, beads.Bead{ID: "open-record", Title: "order:sample-2", Status: "open", CreatedAt: now, Labels: []string{"order-tracking", "order-run:sample-2"}})
	rows = append(rows, beads.Bead{ID: "unrelated-record", Status: "closed", CreatedAt: now, Labels: []string{"order-run:sample-2"}})
	for _, filtered := range []bool{false, true} {
		t.Run(fmt.Sprintf("filtered_%v", filtered), func(t *testing.T) {
			var only map[string]struct{}
			if filtered {
				only = map[string]struct{}{"sample-2": {}, "sample-4": {}}
			}
			want := oldRetentionEligibleIDs(rows, now, policy, only)
			for _, interrupt := range []bool{false, true} {
				t.Run(fmt.Sprintf("interrupt_%v", interrupt), func(t *testing.T) {
					mem := beads.NewMemStoreFrom(len(rows), rows, nil)
					store := retentionBoundedStore{mem}
					count, err := countClosedOrderTrackingRetentionEligible([]beads.Store{store}, now, policy, only)
					if err != nil {
						t.Fatal(err)
					}
					if count != len(want) {
						t.Fatalf("count=%d, want %d", count, len(want))
					}
					if interrupt {
						if _, err := sweepClosedOrderTrackingRetentionBounded(store, now, policy, only, 17); err != nil {
							t.Fatal(err)
						}
					}
					if _, err := sweepClosedOrderTrackingRetention(store, now, policy, only); err != nil {
						t.Fatal(err)
					}
					remaining, err := mem.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
					if err != nil {
						t.Fatal(err)
					}
					present := map[string]bool{}
					for _, b := range remaining {
						present[b.ID] = true
					}
					var deleted []string
					for _, b := range rows {
						if !present[b.ID] {
							deleted = append(deleted, b.ID)
						}
					}
					slices.Sort(deleted)
					if !slices.Equal(deleted, want) {
						t.Fatalf("deleted %d rows, want identical eligible set of %d", len(deleted), len(want))
					}
				})
			}
		})
	}
}

func oldRetentionEligibleIDs(rows []beads.Bead, now time.Time, policy orderTrackingRetentionPolicy, only map[string]struct{}) []string {
	groups := map[string][]beads.Bead{}
	for _, b := range rows {
		if b.Status != "closed" || !slices.Contains(b.Labels, "order-tracking") {
			continue
		}
		name, _ := orders.NameFromTrackingBead(b)
		if len(only) > 0 {
			if _, ok := only[name]; !ok {
				continue
			}
		}
		groups[name] = append(groups[name], b)
	}
	reference := func(b beads.Bead) time.Time {
		if !b.UpdatedAt.IsZero() {
			return b.UpdatedAt
		}
		return b.CreatedAt
	}
	var result []string
	for _, group := range groups {
		sort.Slice(group, func(i, j int) bool {
			a, b := reference(group[i]), reference(group[j])
			if a.Equal(b) {
				return group[i].ID > group[j].ID
			}
			return a.After(b)
		})
		for i, b := range group {
			if i >= max(10, policy.retainLast) && reference(b).Before(now.Add(-policy.deleteAfterClose)) {
				result = append(result, b.ID)
			}
		}
	}
	slices.Sort(result)
	return result
}
