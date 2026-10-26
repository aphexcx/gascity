package session

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// Ownership must finish both bounded legs before absence can authorize attribution.
func TestListWorkDirOwnersPreservesHistoryAndBoundsPages(t *testing.T) {
	mem := beads.NewMemStore()
	for i := 0; i < 600; i++ {
		b := beads.Bead{Title: "historical owner", Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{
			"template": "removed-template", "work_dir": "./lanes/../shared", "session_name": fmt.Sprint(i),
		}}
		if i%2 == 0 {
			b.Type = ""
		}
		created, err := mem.Create(b)
		if err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			empty := ""
			if err := mem.Update(created.ID, beads.UpdateOpts{Type: &empty}); err != nil {
				t.Fatal(err)
			}
		}
		if err := mem.Close(created.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, metadata := range []map[string]string{
		{"work_dir": "/elsewhere"},
		{"work_dir": "shared", "gc.work_dir": "/elsewhere"},
	} {
		if _, err := mem.Create(beads.Bead{Title: "unrelated", Type: BeadType, Metadata: metadata}); err != nil {
			t.Fatal(err)
		}
	}
	spy := &ownershipPageSpy{MemStore: mem}
	got, err := NewStore(beads.SessionStore{Store: spy}).ListWorkDirOwners(context.Background(), "/city", []string{"/city/shared"})
	if err != nil || !got.Complete || len(got.Infos) != 600 {
		t.Fatalf("owners=%d complete=%v err=%v", len(got.Infos), got.Complete, err)
	}
	if spy.calls < 4 || spy.maxRows > 256 {
		t.Fatalf("calls=%d maximum rows=%d", spy.calls, spy.maxRows)
	}
	for _, info := range got.Infos {
		if !info.Closed || info.Template != "removed-template" {
			t.Fatalf("lost history: %+v", info)
		}
	}
}

type ownershipPageSpy struct {
	*beads.MemStore
	calls, maxRows int
	typeRows       int
	labelRows      int
	failAt         int
	repeatID       bool
	afterRead      func()
}

func (s *ownershipPageSpy) ReadMetadataPathPage(ctx context.Context, q beads.MetadataPathQuery) (beads.MetadataPathPage, error) {
	s.calls++
	if s.repeatID {
		return beads.MetadataPathPage{Beads: []beads.Bead{{ID: "colliding-owner", Type: BeadType, Metadata: map[string]string{"work_dir": "shared"}}}, Next: "next", Complete: q.Cursor != ""}, nil
	}
	if s.calls == s.failAt {
		return beads.MetadataPathPage{}, errors.New("archive unavailable")
	}
	p, err := s.MemStore.ReadMetadataPathPage(ctx, q)
	if s.afterRead != nil {
		s.afterRead()
	}
	if q.Label != "" {
		s.labelRows += len(p.Beads)
	} else {
		s.typeRows += len(p.Beads)
	}
	if len(p.Beads) > s.maxRows {
		s.maxRows = len(p.Beads)
	}
	return p, err
}

func TestListWorkDirOwnersUnionReadsTypedAndRepairableOnce(t *testing.T) {
	mem := beads.NewMemStore()
	want := map[string]bool{}
	for _, shape := range []struct {
		kind     string
		hasLabel bool
	}{{BeadType, true}, {BeadType, false}, {"", true}} {
		b := beads.Bead{Title: "owner", Type: BeadType, Metadata: map[string]string{"work_dir": "shared"}}
		if shape.hasLabel {
			b.Labels = []string{LabelSession}
		}
		created, err := mem.Create(b)
		if err != nil {
			t.Fatal(err)
		}
		if shape.kind == "" {
			if err := mem.Update(created.ID, beads.UpdateOpts{Type: &shape.kind}); err != nil {
				t.Fatal(err)
			}
		}
		want[created.ID] = true
	}
	spy := &ownershipPageSpy{MemStore: mem}
	got, err := NewStore(beads.SessionStore{Store: spy}).ListWorkDirOwners(context.Background(), "/city", []string{"/city/shared"})
	if err != nil || !got.Complete || len(got.Infos) != len(want) {
		t.Fatalf("owners=%d complete=%v err=%v", len(got.Infos), got.Complete, err)
	}
	for _, info := range got.Infos {
		if !want[info.ID] {
			t.Fatalf("unexpected or duplicate owner %q", info.ID)
		}
		delete(want, info.ID)
	}
	if len(want) != 0 || spy.typeRows != 2 || spy.labelRows != 1 {
		t.Fatalf("missing=%v type rows=%d label rows=%d; want two typed owners and one repairable owner", want, spy.typeRows, spy.labelRows)
	}
}

func TestListWorkDirOwnersKeepsOwnerDuringTypeRepair(t *testing.T) {
	for _, tc := range []struct {
		name      string
		afterPage int
	}{{"before_queries", 0}, {"between_legs", 1}, {"after_queries", 2}} {
		t.Run(tc.name, func(t *testing.T) {
			mem := beads.NewMemStore()
			owner, err := mem.Create(beads.Bead{Title: "repairable owner", Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{"work_dir": "shared"}})
			if err != nil {
				t.Fatal(err)
			}
			empty := ""
			if err := mem.Update(owner.ID, beads.UpdateOpts{Type: &empty}); err != nil {
				t.Fatal(err)
			}
			store := NewStore(beads.SessionStore{Store: mem})
			repair := func() {
				if err := store.RepairType(owner.ID); err != nil {
					t.Fatal(err)
				}
			}
			spy := &ownershipPageSpy{MemStore: mem}
			spy.afterRead = func() {
				if spy.calls == tc.afterPage {
					repair()
				}
			}
			if tc.afterPage == 0 {
				repair()
			}
			got, err := NewStore(beads.SessionStore{Store: spy}).ListWorkDirOwners(context.Background(), "/city", []string{"/city/shared"})
			if err != nil || !got.Complete || len(got.Infos) != 1 || got.Infos[0].ID != owner.ID {
				t.Fatalf("owner lost or duplicated during type repair: %+v, %v", got, err)
			}
		})
	}
}

func TestListWorkDirOwnersIncompleteIsNotAbsence(t *testing.T) {
	spy := &ownershipPageSpy{MemStore: beads.NewMemStore(), failAt: 2}
	got, err := NewStore(beads.SessionStore{Store: spy}).ListWorkDirOwners(context.Background(), "/city", []string{"/city/shared"})
	if err == nil || got.Complete {
		t.Fatalf("incomplete query: %+v, %v", got, err)
	}
}

func TestListWorkDirOwnersDuplicateWithinLegIsIncomplete(t *testing.T) {
	spy := &ownershipPageSpy{MemStore: beads.NewMemStore(), repeatID: true}
	got, err := NewStore(beads.SessionStore{Store: spy}).ListWorkDirOwners(context.Background(), "/city", []string{"/city/shared"})
	if err == nil || got.Complete {
		t.Fatalf("cross-page collision authorized absence: %+v, %v", got, err)
	}
	if spy.calls != 2 {
		t.Fatalf("collision should stop the first leg after two pages, calls=%d", spy.calls)
	}
}
