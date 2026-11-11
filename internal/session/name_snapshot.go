package session

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// NameAvailabilitySnapshot checks diagnostic names against one session census.
// It is advisory only: creation must still check the authoritative store under
// the identifier locks. Repairs performed by the checker stay in memory.
type NameAvailabilitySnapshot struct {
	store *nameAvailabilitySnapshotStore
}

type nameAvailabilitySnapshotStore struct {
	*beads.MemStore
}

func (s *nameAvailabilitySnapshotStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	// The source already selected the visible tiers. Preserve every captured
	// reservation when the name checker queries the in-memory copy.
	query.TierMode = beads.TierBoth
	return s.MemStore.List(query)
}

// NewNameAvailabilitySnapshot reads reservations once in backend order, keeping
// both canonical and repairable sessions. A type/label union would reorder mixed
// holders and could turn an open-name conflict into a closed-name warning.
// Any incomplete read fails the snapshot instead of reporting names available.
func NewNameAvailabilitySnapshot(store beads.Store) (*NameAvailabilitySnapshot, error) {
	if store == nil {
		return nil, fmt.Errorf("reading session name reservations: store unavailable")
	}
	rows, err := store.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		return nil, fmt.Errorf("reading session name reservations: %w", err)
	}
	sessions := rows[:0]
	for _, row := range rows {
		if IsSessionBeadOrRepairable(row) {
			sessions = append(sessions, row)
		}
	}
	return &NameAvailabilitySnapshot{store: &nameAvailabilitySnapshotStore{
		MemStore: beads.NewMemStoreFrom(0, sessions, nil),
	}}, nil
}

// Check applies the authoritative name rules to this snapshot without further
// backend reads. The result describes the census, not a current reservation.
func (s *NameAvailabilitySnapshot) Check(cfg *config.City, name, selfID, selfOwner string) error {
	return EnsureSessionNameAvailableWithConfigForOwner(s.store, cfg, name, selfID, selfOwner)
}
