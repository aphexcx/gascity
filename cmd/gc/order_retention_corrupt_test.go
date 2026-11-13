package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
	beadslib "github.com/steveyegge/beads"
)

type retentionNativeStorage struct {
	beads.NativeStorage
	rows        map[string]*beadslib.Issue
	member      bool
	largestPage int
}

func (s *retentionNativeStorage) SearchIssues(_ context.Context, _ string, f beadslib.IssueFilter) ([]*beadslib.Issue, error) {
	var out []*beadslib.Issue
	for _, row := range s.rows {
		if f.SkipWisps && row.Ephemeral || f.Ephemeral != nil && row.Ephemeral != *f.Ephemeral {
			continue
		}
		if len(f.IDs) > 0 && !slices.Contains(f.IDs, row.ID) {
			continue
		}
		if f.Status != nil && row.Status != *f.Status || slices.Contains(f.ExcludeStatus, row.Status) {
			continue
		}
		match := true
		for _, label := range f.Labels {
			match = match && slices.Contains(row.Labels, label)
		}
		for _, label := range f.ExcludeLabels {
			match = match && !slices.Contains(row.Labels, label)
		}
		if !match {
			continue
		}
		if f.AfterCreatedAt != nil && (row.CreatedAt.After(*f.AfterCreatedAt) || row.CreatedAt.Equal(*f.AfterCreatedAt) && row.ID <= f.AfterID) {
			continue
		}
		if f.ParentID != nil && !slices.ContainsFunc(row.Dependencies, func(d *beadslib.Dependency) bool {
			return d.Type == beadslib.DepParentChild && d.DependsOnID == *f.ParentID
		}) {
			continue
		}
		// The membership case injects an unreadable projected member at the
		// storage boundary, independently of the JSON-based SQL predicate.
		if len(f.MetadataFields) > 0 && (!s.member || f.MetadataFields[beadmeta.RootBeadIDMetadataKey] != "run-00" || row.ID != "unreadable-row") {
			continue
		}
		out = append(out, row)
	}
	if !f.SkipWisps {
		canonical := make(map[string]*beadslib.Issue)
		for _, row := range out {
			if prior := canonical[row.ID]; prior == nil || row.Ephemeral {
				canonical[row.ID] = row
			}
		}
		out = out[:0]
		for _, row := range canonical {
			out = append(out, row)
		}
	}
	slices.SortFunc(out, func(a, b *beadslib.Issue) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	s.largestPage = max(s.largestPage, len(out))
	return out, nil
}

func (s *retentionNativeStorage) DeleteIssue(_ context.Context, id string) error {
	delete(s.rows, id)
	return nil
}

func (s *retentionNativeStorage) GetDependenciesWithMetadata(context.Context, string) ([]*beadslib.IssueWithDependencyMetadata, error) {
	return nil, nil
}

func (s *retentionNativeStorage) GetDependentsWithMetadata(context.Context, string) ([]*beadslib.IssueWithDependencyMetadata, error) {
	return nil, nil
}

func retentionProductionWrappers() map[string]func(beads.Store) beads.Store {
	return map[string]func(beads.Store) beads.Store{
		"bare":   func(s beads.Store) beads.Store { return s },
		"policy": func(s beads.Store) beads.Store { return wrapStoreWithBeadPolicies(s, nil) },
		"sweep":  func(s beads.Store) beads.Store { return orderTrackingSweepScopedStore{Store: s} },
		"sweep_policy": func(s beads.Store) beads.Store {
			return orderTrackingSweepScopedStore{Store: wrapStoreWithBeadPolicies(s, nil)}
		},
		"policy_cache": func(s beads.Store) beads.Store {
			return wrapStoreWithBeadPolicies(beads.NewCachingStore(s, nil), nil)
		},
		"emitting": func(s beads.Store) beads.Store { return &emittingClassStore{Store: s} },
		"sweep_policy_cache_emitting": func(s beads.Store) beads.Store {
			return orderTrackingSweepScopedStore{Store: wrapStoreWithBeadPolicies(beads.NewCachingStore(&emittingClassStore{Store: s}, nil), nil)}
		},
	}
}

func TestRetentionProductionWrappersBoundBackingPages(t *testing.T) {
	for name, wrap := range retentionProductionWrappers() {
		t.Run(name, func(t *testing.T) {
			storage := &retentionNativeStorage{rows: make(map[string]*beadslib.Issue)}
			for i := 0; i < 1001; i++ {
				id := fmt.Sprintf("run-%04d", i)
				storage.rows[id] = &beadslib.Issue{
					ID: id, Title: "order:sample", Status: beadslib.StatusClosed,
					IssueType: beadslib.TypeTask, CreatedAt: time.Unix(1, 0), Ephemeral: true,
					Metadata: []byte("{}"), Labels: []string{"order-tracking", "order-run:sample"},
					Dependencies: []*beadslib.Dependency{{IssueID: id, DependsOnID: "root", Type: beadslib.DepParentChild}},
				}
			}
			store := wrap(beads.NewNativeDoltStoreWithStorage(storage, "test"))
			runs, err := orders.NewStore(beads.OrdersStore{Store: store}).ClosedRunsForRetention()
			if err != nil || len(runs) != 1001 {
				t.Fatalf("history rows=%d err=%v", len(runs), err)
			}
			if storage.largestPage > 1000 {
				t.Errorf("history fetched %d rows in one backing result", storage.largestPage)
			}
			storage.largestPage = 0
			children, err := listOrderWispPages(beads.HandlesFor(store).Live, beads.ListQuery{ParentID: "root", IncludeClosed: true})
			if err != nil || len(children) != 1001 {
				t.Fatalf("descendant rows=%d err=%v", len(children), err)
			}
			if storage.largestPage > 1000 {
				t.Errorf("descendant guard fetched %d rows in one backing result", storage.largestPage)
			}
			// A closed durable copy must not make its open canonical wisp
			// eligible for retention when decorators preserve exact paging.
			now := time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)
			storage.rows = make(map[string]*beadslib.Issue)
			for i := 0; i < 10; i++ {
				id := fmt.Sprintf("recent-%02d", i)
				storage.rows[id] = &beadslib.Issue{ID: id, Title: "order:sample", Status: beadslib.StatusClosed, CreatedAt: now.Add(-time.Hour), Metadata: []byte("{}"), Ephemeral: true, Labels: []string{"order-tracking", "order-run:sample"}}
			}
			stale := &beadslib.Issue{ID: "overlap", Title: "order:sample", Status: beadslib.StatusClosed, CreatedAt: now.Add(-30 * 24 * time.Hour), Metadata: []byte("{}"), Labels: []string{"order-tracking", "order-run:sample"}}
			canonical := *stale
			canonical.Status, canonical.Ephemeral = beadslib.StatusOpen, true
			storage.rows["durable-copy"], storage.rows["canonical-copy"] = stale, &canonical
			eligible, err := countClosedOrderTrackingRetentionEligible([]beads.Store{store}, now, orderTrackingRetentionPolicy{retainLast: 10, deleteAfterClose: 7 * 24 * time.Hour}, nil)
			if err != nil || eligible != 0 {
				t.Errorf("open canonical wisp became eligible through a stale durable copy: count=%d err=%v", eligible, err)
			}
		})
	}
}

func TestRetentionCorruptRowsKeepRootsAndContinue(t *testing.T) {
	for _, mode := range []string{"history", "child", "member"} {
		for _, bounded := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/bounded_%v", mode, bounded), func(t *testing.T) {
				now := time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)
				var rows []beads.Bead
				for i := 0; i < 13; i++ {
					rows = append(rows, beads.Bead{ID: fmt.Sprintf("run-%02d", i), Title: "order:sample", Status: "closed", CreatedAt: now.Add(-15 * 24 * time.Hour), Labels: []string{"order-tracking", "order-run:sample"}})
				}
				bad := beads.Bead{ID: "unreadable-row", Status: "open", ParentID: "run-00"}
				if mode == "history" {
					bad.Status = "closed"
					bad.ParentID = ""
					bad.Title = "order:sample"
					bad.Labels = []string{"order-tracking", "order-run:sample"}
				}
				rows = append(rows, bad)
				storage := &retentionNativeStorage{rows: map[string]*beadslib.Issue{}, member: mode == "member"}
				for _, b := range rows {
					row := &beadslib.Issue{ID: b.ID, Title: b.Title, Status: beadslib.Status(b.Status), IssueType: beadslib.TypeTask, CreatedAt: b.CreatedAt, Labels: b.Labels, Ephemeral: true, Metadata: []byte("{}")}
					if b.ID == bad.ID {
						row.Metadata = []byte("[]")
					}
					if b.ParentID != "" {
						row.Dependencies = []*beadslib.Dependency{{IssueID: b.ID, DependsOnID: b.ParentID, Type: beadslib.DepParentChild}}
					}
					storage.rows[b.ID] = row
				}
				store := beads.NewNativeDoltStoreWithStorage(storage, "test")
				var logs bytes.Buffer
				previous := log.Writer()
				log.SetOutput(&logs)
				t.Cleanup(func() { log.SetOutput(previous) })
				policy := orderTrackingRetentionPolicy{retainLast: 10, deleteAfterClose: 7 * 24 * time.Hour}
				if mode == "history" {
					count, err := countClosedOrderTrackingRetentionEligible([]beads.Store{store}, now, policy, nil)
					if err != nil || count != 3 {
						t.Errorf("preview count=%d err=%v, want 3", count, err)
					}
					if strings.Count(logs.String(), bad.ID) != 1 {
						t.Errorf("preview warnings=%s", logs.String())
					}
					logs.Reset()
				}
				var deleted int
				var err error
				if bounded {
					deleted, err = sweepClosedOrderTrackingRetentionBounded(store, now, policy, nil, 100)
				} else {
					deleted, err = sweepClosedOrderTrackingRetention(store, now, policy, nil)
				}
				want := 2
				if mode == "history" {
					want = 3
				}
				if err != nil || deleted != want {
					t.Errorf("deleted=%d err=%v, want %d and no pass failure", deleted, err, want)
				}
				if _, exists := storage.rows[bad.ID]; !exists {
					t.Error("unreadable row deleted")
				}
				if mode != "history" {
					if _, exists := storage.rows["run-00"]; !exists {
						t.Error("unsafe root deleted")
					}
				}
				if strings.Count(logs.String(), bad.ID) != 1 || !strings.Contains(logs.String(), "1 unreadable") {
					t.Errorf("warning must count and identify the row once per pass: %s", logs.String())
				}
			})
		}
	}
}
