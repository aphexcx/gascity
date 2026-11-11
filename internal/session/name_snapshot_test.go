package session

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

type nameSnapshotStore struct {
	beads.Store
	queries  []beads.ListQuery
	failRead bool
}

func (s *nameSnapshotStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.queries = append(s.queries, q)
	if s.failRead {
		return nil, errors.New("reservation source unavailable")
	}
	return s.Store.List(q)
}

func TestNameAvailabilitySnapshot(t *testing.T) {
	rows := []beads.Bead{
		{ID: "closed", Type: BeadType, Status: "closed", Metadata: map[string]string{"session_name": "held", "session_origin": "unknown"}},
		{ID: "manual", Type: BeadType, Status: "closed", Metadata: map[string]string{"session_name": "released", "session_origin": "manual"}},
		{ID: "legacy", Labels: []string{LabelSession}, Status: "closed", Metadata: map[string]string{"session_name": "legacy-held"}},
		{ID: "alias", Type: BeadType, Status: "open", Metadata: map[string]string{"alias": "live-alias"}},
	}
	source := &nameSnapshotStore{Store: beads.NewMemStoreFrom(0, rows, nil)}
	snapshot, err := NewNameAvailabilitySnapshot(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"held", "released", "legacy-held", "live-alias", "free"} {
		// Use a separate store: the legacy live checker may repair its input.
		want := EnsureSessionNameAvailableWithConfigForOwner(beads.NewMemStoreFrom(0, rows, nil), nil, name, "", "")
		got := snapshot.Check(nil, name, "", "")
		if (got == nil) != (want == nil) || (got != nil && got.Error() != want.Error()) {
			t.Errorf("%q: snapshot=%v direct=%v", name, got, want)
		}
	}
	if len(source.queries) != 1 {
		t.Errorf("queries=%d, want one snapshot regardless of names", len(source.queries))
	}
	legacy, err := source.Get("legacy")
	if err != nil || legacy.Type != "" {
		t.Errorf("snapshot repaired the authoritative store: %+v, %v", legacy, err)
	}
	// A failed read must not turn a partial snapshot into an available name.
	source.failRead = true
	if snapshot, err := NewNameAvailabilitySnapshot(source); err == nil || snapshot != nil {
		t.Fatalf("failed source returned snapshot=%v err=%v", snapshot, err)
	}
}

func TestNameAvailabilitySnapshotPreservesHolderOrder(t *testing.T) {
	for _, status := range []string{"open", "closed"} {
		t.Run(status, func(t *testing.T) {
			rows := []beads.Bead{
				{ID: "legacy-first", Labels: []string{LabelSession}, Status: status, Metadata: map[string]string{"session_name": "held"}},
				{ID: "canonical-second", Type: BeadType, Status: "closed", Metadata: map[string]string{"session_name": "held"}},
			}
			snapshot, err := NewNameAvailabilitySnapshot(beads.NewMemStoreFrom(0, rows, nil))
			if err != nil {
				t.Fatal(err)
			}
			want := EnsureSessionNameAvailableWithConfigForOwner(beads.NewMemStoreFrom(0, rows, nil), nil, "held", "", "")
			got := snapshot.Check(nil, "held", "", "")
			if got == nil || want == nil || got.Error() != want.Error() {
				t.Fatalf("holder precedence changed: snapshot=%v direct=%v", got, want)
			}
		})
	}
}
