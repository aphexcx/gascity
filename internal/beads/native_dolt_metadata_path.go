package beads

import (
	"context"
	"fmt"

	beadslib "github.com/steveyegge/beads"
)

// ReadMetadataPathPage uses the native store's compound keyset and server-side
// limit. Metadata paths are resolved after each bounded page because the
// native filter does not expose lexical-path predicates.
func (s *NativeDoltStore) ReadMetadataPathPage(ctx context.Context, q MetadataPathQuery) (MetadataPathPage, error) {
	if err := q.validate(); err != nil {
		return MetadataPathPage{}, err
	}
	cursor, err := decodeMetadataPathCursor(q.Cursor)
	if err != nil {
		return MetadataPathPage{}, err
	}
	var page MetadataPathPage
	err = s.withReadRetry(func(readCtx context.Context, storage beadslib.Storage) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		pageCtx, cancel := context.WithCancel(readCtx)
		defer cancel()
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
		ephemeral := false
		// SearchIssues reads issues and wisps independently before merging.
		// Half the cap per physical leg keeps their combined read bounded.
		// Disable ID prefetch and label hydration to avoid extra row reads.
		const limit = MetadataPathPageSize / 2
		filter := beadslib.IssueFilter{Limit: limit, SortBy: "created", Ephemeral: &ephemeral, AfterCreatedAt: cursor.CreatedAt, AfterID: cursor.ID, NoIDShrink: true, SkipLabels: true, SkipCounts: true, Lite: true}
		if q.Type != "" {
			value := beadslib.IssueType(q.Type)
			filter.IssueType = &value
		}
		if q.Label != "" {
			filter.Labels = []string{q.Label}
		}
		issues, err := storage.SearchIssues(pageCtx, "", filter)
		if err != nil {
			return err
		}
		s.noteRows(len(issues))
		var rows []Bead
		for _, issue := range issues {
			if issue == nil || issue.ID == "" {
				return fmt.Errorf("metadata path page contains an empty issue")
			}
			b, err := beadFromNativeIssue(issue)
			if err != nil {
				return err
			}
			if q.Label != "" {
				b.Labels = []string{q.Label}
			}
			rows = append(rows, b)
		}
		next := cursor
		if len(issues) > 0 {
			last := issues[len(issues)-1]
			next.ID = last.ID
			next.CreatedAt = &last.CreatedAt
		}
		page, err = finishMetadataPathPageAtLimit(q, rows, next, limit)
		return err
	})
	if err != nil {
		return MetadataPathPage{}, fmt.Errorf("reading native metadata path page: %w", err)
	}
	return page, nil
}
