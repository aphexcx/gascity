//go:build gascity_native_beads

package beads

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ReadMetadataPathPage bounds the DoltLite SQL result before hydration and
// preserves legacy storage-tier layouts through the provider's schema probes.
func (s *DoltliteReadStore) ReadMetadataPathPage(ctx context.Context, q MetadataPathQuery) (MetadataPathPage, error) {
	if err := q.validate(); err != nil {
		return MetadataPathPage{}, err
	}
	cursor, err := decodeMetadataPathCursor(q.Cursor)
	if err != nil {
		return MetadataPathPage{}, err
	}
	var tables []metadataPathTable
	for _, table := range []doltliteTableSet{doltliteIssueTables, doltliteWispTables} {
		var name string
		err := s.db.QueryRowContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name=?", table.issues).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return MetadataPathPage{}, fmt.Errorf("probing metadata ownership table %s: %w", table.issues, err)
		}
		flags, err := s.storageFlagExprsFor(table)
		if err != nil {
			return MetadataPathPage{}, err
		}
		tables = append(tables, metadataPathTable{table.issues, table.labels, flags.ephemeral})
	}
	if len(tables) == 0 {
		return MetadataPathPage{}, fmt.Errorf("metadata ownership tables unavailable")
	}
	rows, err := s.db.QueryContext(ctx, metadataPathLedgerSQL(q, cursor, true, tables))
	if err != nil {
		return MetadataPathPage{}, err
	}
	defer rows.Close() //nolint:errcheck
	var found []Bead
	for rows.Next() {
		var b Bead
		var raw, pageID string
		if err := rows.Scan(&b.ID, &b.Type, &b.Status, &raw, &pageID); err != nil {
			return MetadataPathPage{}, err
		}
		var metadata StringMap
		if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
			return MetadataPathPage{}, err
		}
		b.Metadata = metadata
		if q.Label != "" {
			b.Labels = []string{q.Label}
		}
		found = append(found, b)
		cursor.ID = b.ID
	}
	if err := rows.Err(); err != nil {
		return MetadataPathPage{}, err
	}
	return finishMetadataPathPage(q, found, cursor)
}
