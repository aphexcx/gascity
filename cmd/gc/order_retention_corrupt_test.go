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
	beadslib "github.com/steveyegge/beads"
)

type retentionNativeStorage struct {
	beads.NativeStorage
	rows   map[string]*beadslib.Issue
	member bool
}

func (s *retentionNativeStorage) SearchIssues(_ context.Context, _ string, f beadslib.IssueFilter) ([]*beadslib.Issue, error) {
	var out []*beadslib.Issue
	if f.SkipWisps {
		return nil, nil
	}
	for _, row := range s.rows {
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
	slices.SortFunc(out, func(a, b *beadslib.Issue) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
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
