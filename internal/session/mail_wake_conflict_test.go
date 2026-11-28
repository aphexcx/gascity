package session

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// mailWakeMovedRevision moves the session record between the reservation's
// read and its revision-fenced write.
type mailWakeMovedRevision struct{ *beads.MemStore }

func (s mailWakeMovedRevision) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	if err := s.SetMetadata(id, "unrelated", "write"); err != nil {
		return err
	}
	return s.MemStore.UpdateIfMatch(id, expectedRevision, opts)
}

// mailWakeLostValueCAS loses every compare-and-set of the history key.
type mailWakeLostValueCAS struct{ mailWakeWithoutRevisions }

func (mailWakeLostValueCAS) CompareAndSetMetadataKey(string, string, string, string) (bool, error) {
	return false, nil
}

// A reservation that lost a race says so, whichever fence detected it, and
// writes nothing. A reservation refused for another reason does not look like
// a race.
func TestRecordMailWakeReportsLostRace(t *testing.T) {
	entry := MailWakeEntry{Sender: "city:alpha", At: waitStoreNow}
	for name, wrap := range map[string]func(*beads.MemStore) beads.Store{
		"history read before another reservation": func(mem *beads.MemStore) beads.Store { return mem },
		"record moved before the fenced write":    func(mem *beads.MemStore) beads.Store { return mailWakeMovedRevision{mem} },
		"history key swapped by another writer":   func(mem *beads.MemStore) beads.Store { return mailWakeLostValueCAS{mailWakeWithoutRevisions{mem}} },
	} {
		t.Run(name, func(t *testing.T) {
			mem := beads.NewMemStore()
			b, err := mem.Create(beads.Bead{Type: BeadType, Labels: []string{LabelSession}})
			if err != nil {
				t.Fatal(err)
			}
			first := MailWakeEntry{Sender: "human", At: waitStoreNow}
			if err := waitStoreOver(mem).RecordMailWake(b.ID, nil, first); err != nil {
				t.Fatal(err)
			}
			store := waitStoreOver(wrap(mem))
			seen, err := store.MailWakeHistory(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if name == "history read before another reservation" {
				seen = nil
			}
			if err := store.RecordMailWake(b.ID, seen, entry); !errors.Is(err, ErrMailWakeHistoryChanged) {
				t.Fatalf("lost race reported as %v", err)
			}
			after, err := waitStoreOver(mem).MailWakeHistory(b.ID)
			if err != nil || len(after) != 1 || after[0].Sender != "human" {
				t.Fatalf("history after the lost race: %v, %v", after, err)
			}
			// Reading again and retrying is all a caller needs to do.
			if err := waitStoreOver(mem).RecordMailWake(b.ID, after, entry); err != nil {
				t.Fatalf("retry against the fresh history: %v", err)
			}
		})
	}

	mem := beads.NewMemStore()
	b, err := mem.Create(beads.Bead{Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{"state": "suspended"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitStoreOver(mem).RecordMailWake(b.ID, nil, entry); err == nil || errors.Is(err, ErrMailWakeHistoryChanged) {
		t.Fatalf("an ineligible session was reported as %v", err)
	}
}
