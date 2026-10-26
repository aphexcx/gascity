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

// ListWorkDirOwners returns durable sessions whose persisted working directory
// resolves lexically to a requested path, including closed and removed owners.
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
	seen := map[string]bool{}
	for _, q := range []beads.MetadataPathQuery{{Type: BeadType}, {Label: LabelSession}} {
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
				if !seen[b.ID] && IsSessionBeadOrRepairable(b) {
					seen[b.ID] = true
					result.Infos = append(result.Infos, infoFromPersistedBead(b))
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
