package beads

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// MetadataPathPageSize is the maximum backing rows read by one metadata page.
const MetadataPathPageSize = 256

// ErrMetadataPathPagingUnsupported means completeness cannot be established
// without an unbounded read. Callers must not interpret it as an empty result.
var ErrMetadataPathPagingUnsupported = errors.New("bounded metadata path paging unavailable")

// MetadataPathQuery selects a type or label and lexical directory ownership.
// Keys are in precedence order: the first nonblank value is authoritative.
// Relative metadata paths resolve against BaseDir; symlinks are not followed.
// Cursor is opaque and may only be reused with the same query and provider.
type MetadataPathQuery struct {
	Type, Label string
	Keys, Paths []string
	BaseDir     string
	Cursor      string
}

// MetadataPathPage contains matching rows from one bounded backing page.
// An empty page can still be incomplete (backing candidates were filtered).
// Only Complete proves exhaustion; otherwise Next must advance the cursor.
type MetadataPathPage struct {
	Beads    []Bead
	Next     string
	Complete bool
}

// MetadataPathPageReader reads at most MetadataPathPageSize backing rows per
// call, including closed durable records. It never falls back to unbounded List.
type MetadataPathPageReader interface {
	ReadMetadataPathPage(context.Context, MetadataPathQuery) (MetadataPathPage, error)
}

type metadataPathCursor struct {
	ID        string
	CreatedAt *time.Time `json:",omitempty"`
}

func (q MetadataPathQuery) validate() error {
	if (q.Type == "") == (q.Label == "") || len(q.Keys) == 0 || len(q.Paths) == 0 || !filepath.IsAbs(q.BaseDir) {
		return errors.New("metadata path page requires one type or label, keys, absolute base and paths")
	}
	for _, path := range q.Paths {
		if !filepath.IsAbs(path) {
			return errors.New("metadata path page requires absolute paths")
		}
	}
	return nil
}

func (q MetadataPathQuery) matches(b Bead) bool {
	if b.Ephemeral || (q.Type != "" && b.Type != q.Type) || (q.Label != "" && !slices.Contains(b.Labels, q.Label)) {
		return false
	}
	for _, key := range q.Keys {
		path := strings.TrimSpace(b.Metadata[key])
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(q.BaseDir, path)
		}
		path = filepath.Clean(path)
		for _, wanted := range q.Paths {
			if path == filepath.Clean(wanted) {
				return true
			}
		}
		return false
	}
	return false
}

func decodeMetadataPathCursor(value string) (metadataPathCursor, error) {
	var cursor metadataPathCursor
	if value == "" {
		return cursor, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err == nil {
		err = json.Unmarshal(raw, &cursor)
	}
	if err != nil || cursor.ID == "" {
		return cursor, errors.New("invalid metadata path cursor")
	}
	return cursor, nil
}

func finishMetadataPathPage(q MetadataPathQuery, rows []Bead, cursor metadataPathCursor) (MetadataPathPage, error) {
	return finishMetadataPathPageAtLimit(q, rows, cursor, MetadataPathPageSize)
}

func finishMetadataPathPageAtLimit(q MetadataPathQuery, rows []Bead, cursor metadataPathCursor, limit int) (MetadataPathPage, error) {
	if len(rows) > limit {
		return MetadataPathPage{}, errors.New("metadata path backing page exceeded row bound")
	}
	page := MetadataPathPage{Complete: len(rows) < limit}
	for _, b := range rows {
		if q.matches(b) {
			page.Beads = append(page.Beads, b)
		}
	}
	if !page.Complete {
		if cursor.ID == "" {
			return MetadataPathPage{}, errors.New("metadata path page has no continuation")
		}
		raw, err := json.Marshal(cursor)
		if err != nil {
			return MetadataPathPage{}, err
		}
		page.Next = base64.RawURLEncoding.EncodeToString(raw)
		if page.Next == q.Cursor {
			return MetadataPathPage{}, errors.New("metadata path page did not advance")
		}
	}
	return page, nil
}

// ReadMetadataPathPage reads bounded lexical ownership candidates from memory.
func (m *MemStore) ReadMetadataPathPage(ctx context.Context, q MetadataPathQuery) (MetadataPathPage, error) {
	if err := q.validate(); err != nil {
		return MetadataPathPage{}, err
	}
	cursor, err := decodeMetadataPathCursor(q.Cursor)
	if err != nil {
		return MetadataPathPage{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return MetadataPathPage{}, err
	}
	var rows []Bead
	for _, b := range m.beads {
		if b.ID > cursor.ID && q.matches(b) {
			rows = append(rows, b)
		}
	}
	slices.SortFunc(rows, func(a, b Bead) int { return strings.Compare(a.ID, b.ID) })
	if len(rows) > MetadataPathPageSize {
		rows = rows[:MetadataPathPageSize]
	}
	for i := range rows {
		rows[i] = cloneBead(rows[i])
	}
	if len(rows) > 0 {
		cursor.ID = rows[len(rows)-1].ID
	}
	return finishMetadataPathPage(q, rows, cursor)
}

// ReadMetadataPathPage bypasses the active-only cache so history stays visible.
func (c *CachingStore) ReadMetadataPathPage(ctx context.Context, q MetadataPathQuery) (MetadataPathPage, error) {
	reader, ok := c.backing.(MetadataPathPageReader)
	if !ok {
		return MetadataPathPage{}, fmt.Errorf("%T: %w", c.backing, ErrMetadataPathPagingUnsupported)
	}
	return reader.ReadMetadataPathPage(ctx, q)
}
