package session

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// WorkDirOwners is authoritative only when Complete is true. Incomplete
// history cannot establish that a directory has no other transcript owner.
type WorkDirOwners struct {
	Infos    []Info
	Complete bool
}

// ListWorkDirOwners returns sessions whose persisted working directory
// resolves lexically to a requested path, including closed and removed owners.
// Policy-store composition also preserves its existing ephemeral visibility.
// Both canonical type and repairable label legs must finish. Backing reads are
// capped at 256 rows per page; unsupported paging and partial reads fail closed.
func (s *Store) ListWorkDirOwners(ctx context.Context, cityPath string, paths []string) (WorkDirOwners, error) {
	result := WorkDirOwners{}
	if len(paths) == 0 || s == nil || s.store.Store == nil {
		result.Complete = true
		return result, nil
	}
	reader, ok := s.store.Store.(beads.MetadataPathPageReader)
	if !ok {
		return result, fmt.Errorf("listing work directory owners: %w", beads.ErrMetadataPathPagingUnsupported)
	}
	seen := map[string]int{}
	// Read repairable owners before typed sessions: RepairType only moves rows
	// from the former set into the latter, so a repair between reads cannot hide
	// an owner. The type leg includes sessions regardless of labels and wins
	// cross-leg ID collisions, including stale labeled physical copies.
	for _, q := range []beads.MetadataPathQuery{{Label: LabelSession, UntypedOnly: true}, {Type: BeadType}} {
		q.Keys = []string{beadmeta.WorkDirMetadataKey, beadmeta.LegacyWorkDirMetadataKey}
		q.BaseDir, q.Paths = cityPath, paths
		cursors := map[string]bool{}
		legIDs := map[string]bool{}
		for {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			page, err := reader.ReadMetadataPathPage(ctx, q)
			if err != nil {
				return result, fmt.Errorf("listing work directory owners: %w", err)
			}
			if len(page.Beads) > beads.MetadataPathPageSize {
				return result, fmt.Errorf("work directory owner page exceeded %d rows", beads.MetadataPathPageSize)
			}
			for _, b := range page.Beads {
				// A repeated ID inside one leg can be a stale physical copy
				// or a concurrently moved cursor key. Neither proves ownership.
				if b.ID == "" || legIDs[b.ID] {
					return result, fmt.Errorf("work directory owner query incomplete: repeated or empty ID %q", b.ID)
				}
				legIDs[b.ID] = true
				if IsSessionBeadOrRepairable(b) {
					info := infoFromPersistedBead(b)
					if index, exists := seen[b.ID]; exists {
						// Within-leg duplicates already failed above. This is the
						// typed projection replacing its earlier repairable copy.
						result.Infos[index] = info
					} else {
						seen[b.ID] = len(result.Infos)
						result.Infos = append(result.Infos, info)
					}
				}
			}
			if page.Complete {
				break
			}
			if page.Next == "" || cursors[page.Next] {
				return result, fmt.Errorf("work directory owner query incomplete: cursor did not advance")
			}
			cursors[page.Next] = true
			q.Cursor = page.Next
		}
	}
	result.Complete = true
	return result, nil
}
