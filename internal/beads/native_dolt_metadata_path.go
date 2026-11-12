package beads

import (
	"context"
	"database/sql"
	"fmt"

	beadslib "github.com/steveyegge/beads"
)

// ReadMetadataPathPage resolves physical-table collisions before applying the
// shared SQL page bound. Native backends without SQL access fail closed.
func (s *NativeDoltStore) ReadMetadataPathPage(ctx context.Context, q MetadataPathQuery) (MetadataPathPage, error) {
	if err := q.validate(); err != nil {
		return MetadataPathPage{}, err
	}
	cursor, err := decodeMetadataPathCursor(q.Cursor)
	if err != nil {
		return MetadataPathPage{}, err
	}
	var page MetadataPathPage
	err = s.withReadRetry(ctx, "metadata-path", func(readCtx context.Context, storage beadslib.Storage) error {
		accessor, ok := storage.(interface{ DB() *sql.DB })
		if !ok || accessor.DB() == nil {
			return ErrMetadataPathPagingUnsupported
		}
		rows, err := accessor.DB().QueryContext(readCtx, metadataPathDoltSQL(q, cursor))
		if err != nil {
			return err
		}
		defer rows.Close() //nolint:errcheck
		page, err = readMetadataPathSQLRows(q, rows, cursor, func() { s.noteRows(1) })
		return err
	})
	if err != nil {
		return MetadataPathPage{}, fmt.Errorf("reading native metadata path page: %w", err)
	}
	return page, nil
}
