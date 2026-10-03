package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// Delete only after List has returned its snapshot, reproducing a convoy that
// disappears between discovery and the authoritative ownership read.
type convoyDeletedAfterListStore struct {
	*beads.MemStore
	deleteIDs []string
}

func (s *convoyDeletedAfterListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.MemStore.List(q)
	if err != nil {
		return nil, err
	}
	for _, id := range s.deleteIDs {
		if err := s.Delete(id); err != nil {
			return nil, err
		}
	}
	s.deleteIDs = nil
	return rows, nil
}

func TestConvoyCheckContinuesAfterUnreadableConvoy(t *testing.T) {
	for _, jsonOut := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%t", jsonOut), func(t *testing.T) {
			store := &convoyDeletedAfterListStore{MemStore: beads.NewMemStore()}
			missing, err := store.Create(beads.Bead{Title: "removed batch", Type: "convoy"})
			requireNoError(t, err)
			healthy, err := store.Create(beads.Bead{Title: "finished batch", Type: "convoy"})
			requireNoError(t, err)
			child, err := store.Create(beads.Bead{Title: "finished task", ParentID: healthy.ID})
			requireNoError(t, err)
			requireNoError(t, store.Close(child.ID))
			store.deleteIDs = []string{missing.ID}
			var stdout, stderr bytes.Buffer
			code := doConvoyCheckAcrossStoresJSON([]convoyStoreView{{store: store}}, events.Discard, jsonOut, &stdout, &stderr)
			if code != 0 {
				t.Errorf("exit = %d, want 0; stderr=%s", code, &stderr)
			}
			got, err := store.Get(healthy.ID)
			requireNoError(t, err)
			if got.Status != "closed" {
				t.Errorf("readable convoy status = %q, want closed", got.Status)
			}
			if strings.Count(stderr.String(), "\n") != 2 || !strings.Contains(stderr.String(), missing.ID) || !strings.Contains(stderr.String(), "1 unreadable convoy(s)") {
				t.Errorf("stderr = %q, want one convoy diagnostic and skipped count", stderr.String())
			}
			if jsonOut {
				var result convoyActionResult
				requireNoError(t, json.Unmarshal(stdout.Bytes(), &result))
				if !result.OK || result.Closed == nil || *result.Closed != 1 || strings.Count(stdout.String(), "\n") != 1 {
					t.Errorf("stdout = %q, want one successful JSON result with closed=1", stdout.String())
				}
			} else if !strings.Contains(stdout.String(), "1 convoy(s) auto-closed") {
				t.Errorf("stdout = %q, want one closed convoy", stdout.String())
			}
		})
	}
}

func TestConvoyCheckFailsWhenAllConvoysUnreadable(t *testing.T) {
	store := &convoyDeletedAfterListStore{MemStore: beads.NewMemStore()}
	for i := 0; i < 2; i++ {
		convoy, err := store.Create(beads.Bead{Title: "removed batch", Type: "convoy"})
		requireNoError(t, err)
		store.deleteIDs = append(store.deleteIDs, convoy.ID)
	}
	missingIDs := append([]string(nil), store.deleteIDs...)
	var stdout, stderr bytes.Buffer
	code := doConvoyCheckAcrossStoresJSON([]convoyStoreView{{store: store}}, events.Discard, true, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	for _, id := range missingIDs {
		if !strings.Contains(stderr.String(), id) {
			t.Errorf("stderr = %q, want one diagnostic for %s", stderr.String(), id)
		}
	}
	if strings.Count(stderr.String(), "\n") != 3 || !strings.Contains(stderr.String(), "2 unreadable convoy(s)") {
		t.Errorf("stderr = %q, want skipped count", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want no success result", stdout.String())
	}
}
