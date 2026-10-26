package beads

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// metadataPathCandidateSQL is deliberately a superset. Any absolute lexical
// path resolving to a target contains its final component, even with whitespace
// or dot segments; every relative path remains a candidate. Matching either
// key avoids imposing SQL TRIM's narrower whitespace rules on key precedence.
// Exact normalization and precedence are applied after the bounded page.
func metadataPathCandidateSQL(q MetadataPathQuery, value func(string) string) (string, []any) {
	var clauses []string
	var args []any
	for _, key := range q.Keys {
		expr := value(key)
		parts := []string{expr + " NOT LIKE '/%'"}
		for _, path := range q.Paths {
			parts = append(parts, "INSTR("+expr+", ?) > 0")
			args = append(args, filepath.Base(filepath.Clean(path)))
		}
		clauses = append(clauses, "("+strings.Join(parts, " OR ")+")")
	}
	return "(" + strings.Join(clauses, " OR ") + ")", args
}

func metadataJSONPath(key string) string {
	quoted, _ := json.Marshal(key)
	return "$." + string(quoted)
}

func metadataSQLLiteral(value string, sqlite bool) string {
	if value == "" {
		return "''"
	}
	encoded := hex.EncodeToString([]byte(value))
	if sqlite {
		return "CAST(X'" + encoded + "' AS TEXT)"
	}
	return "CONVERT(X'" + encoded + "' USING utf8mb4)"
}

// ReadMetadataPathPage pushes the candidate scope, bytewise cursor and row cap
// into SQLite before decoding any bead payloads.
func (s *SQLiteStore) ReadMetadataPathPage(ctx context.Context, q MetadataPathQuery) (MetadataPathPage, error) {
	if err := s.ensureOpen(); err != nil {
		return MetadataPathPage{}, err
	}
	if err := q.validate(); err != nil {
		return MetadataPathPage{}, err
	}
	cursor, err := decodeMetadataPathCursor(q.Cursor)
	if err != nil {
		return MetadataPathPage{}, err
	}
	where := []string{"b.tier = 'main'", "HEX(b.id) > ?"}
	args := []any{strings.ToUpper(hex.EncodeToString([]byte(cursor.ID)))}
	if q.Type != "" {
		where = append(where, "b.issue_type = ?")
		args = append(args, q.Type)
	}
	if q.Label != "" {
		where = append(where, "EXISTS (SELECT 1 FROM labels l WHERE l.bead_id=b.id AND l.label=?)")
		args = append(args, q.Label)
	}
	candidate, candidateArgs := metadataPathCandidateSQL(q, func(key string) string {
		return "(SELECT m.meta_value FROM metadata m WHERE m.bead_id=b.id AND m.meta_key=" + metadataSQLLiteral(key, true) + ")"
	})
	where = append(where, candidate)
	args = append(args, candidateArgs...)
	sqlText := "SELECT b.bead_json FROM beads b WHERE " + strings.Join(where, " AND ") + " ORDER BY HEX(b.id) LIMIT 256"
	rows, err := s.readDB.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return MetadataPathPage{}, fmt.Errorf("reading metadata path page: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var found []Bead
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return MetadataPathPage{}, err
		}
		var b Bead
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			return MetadataPathPage{}, err
		}
		found = append(found, b)
		cursor.ID = b.ID
	}
	if err := rows.Err(); err != nil {
		return MetadataPathPage{}, err
	}
	return finishMetadataPathPage(q, found, cursor)
}

// metadataPathDoltSQL is owned by the bd provider, not the session domain. It
// reads only a bounded projection, including durable no-history wisps. The
// canonical wisp wins an ID collision, matching bd's JSON list union.
func metadataPathDoltSQL(q MetadataPathQuery, cursor metadataPathCursor) string {
	return metadataPathLedgerSQL(q, cursor, false, []metadataPathTable{
		{"wisps", "wisp_labels", "COALESCE(i.ephemeral,0)"},
		{"issues", "labels", "COALESCE(i.ephemeral,0)"},
	})
}

type metadataPathTable struct{ name, labels, ephemeral string }

func metadataPathLedgerSQL(q MetadataPathQuery, cursor metadataPathCursor, sqlite bool, tables []metadataPathTable) string {
	literal := func(value string) string { return metadataSQLLiteral(value, sqlite) }
	var legs []string
	for index, table := range tables {
		where := []string{table.ephemeral + "=0", "HEX(i.id)>" + literal(strings.ToUpper(hex.EncodeToString([]byte(cursor.ID))))}
		if q.Type != "" {
			where = append(where, "i.issue_type="+literal(q.Type))
		}
		if q.Label != "" {
			where = append(where, "EXISTS (SELECT 1 FROM "+table.labels+" l WHERE l.issue_id=i.id AND l.label="+literal(q.Label)+")")
		}
		candidate, args := metadataPathCandidateSQL(q, func(key string) string {
			expr := "JSON_EXTRACT(i.metadata," + literal(metadataJSONPath(key)) + ")"
			if sqlite {
				return expr
			}
			return "JSON_UNQUOTE(" + expr + ")"
		})
		for _, arg := range args {
			candidate = strings.Replace(candidate, "?", literal(arg.(string)), 1)
		}
		// Malformed metadata is an error, never evidence that no owner exists.
		where = append(where, "(NOT JSON_VALID(i.metadata) OR "+candidate+")")
		if index > 0 {
			shadow := "other.id=i.id AND " + strings.ReplaceAll(tables[0].ephemeral, "i.", "other.") + "=0"
			if q.Type != "" {
				shadow += " AND other.issue_type=" + literal(q.Type)
			}
			if q.Label != "" {
				shadow += " AND EXISTS (SELECT 1 FROM " + tables[0].labels + " ol WHERE ol.issue_id=other.id AND ol.label=" + literal(q.Label) + ")"
			}
			where = append(where, "NOT EXISTS (SELECT 1 FROM "+tables[0].name+" other WHERE "+shadow+")")
		}
		castType := "CHAR"
		if sqlite {
			castType = "TEXT"
		}
		legs = append(legs, "SELECT i.id, i.issue_type, i.status, CAST(i.metadata AS "+castType+") AS metadata_json, HEX(i.id) AS page_id FROM "+table.name+" i WHERE "+strings.Join(where, " AND "))
	}
	return strings.Join(legs, " UNION ALL ") + " ORDER BY page_id LIMIT 256"
}

// ReadMetadataPathPage uses a bounded SQL projection instead of List, whose
// client-side filters intentionally strip native limits for other callers.
func (s *BdStore) ReadMetadataPathPage(ctx context.Context, q MetadataPathQuery) (MetadataPathPage, error) {
	if err := q.validate(); err != nil {
		return MetadataPathPage{}, err
	}
	if err := ctx.Err(); err != nil {
		return MetadataPathPage{}, err
	}
	cursor, err := decodeMetadataPathCursor(q.Cursor)
	if err != nil {
		return MetadataPathPage{}, err
	}
	out, err := s.runBDTransientRead("sql", metadataPathDoltSQL(q, cursor), "--json")
	if err != nil {
		return MetadataPathPage{}, fmt.Errorf("reading bd metadata path page: %w", err)
	}
	var rows []struct {
		ID       string `json:"id"`
		Type     string `json:"issue_type"`
		Status   string `json:"status"`
		Metadata string `json:"metadata_json"`
	}
	if err := json.Unmarshal(extractJSON(out), &rows); err != nil {
		return MetadataPathPage{}, err
	}
	if rows == nil {
		return MetadataPathPage{}, fmt.Errorf("metadata path page is null")
	}
	s.noteServerRows(len(rows))
	var found []Bead
	for _, row := range rows {
		if row.ID == "" {
			return MetadataPathPage{}, fmt.Errorf("metadata path page contains an empty ID")
		}
		var metadata StringMap
		if err := json.Unmarshal([]byte(row.Metadata), &metadata); err != nil {
			return MetadataPathPage{}, fmt.Errorf("metadata for %s: %w", row.ID, err)
		}
		b := Bead{ID: row.ID, Type: row.Type, Status: row.Status, Metadata: metadata}
		if q.Label != "" {
			b.Labels = []string{q.Label}
		}
		found = append(found, b)
		cursor.ID = b.ID
	}
	if err := ctx.Err(); err != nil {
		return MetadataPathPage{}, err
	}
	return finishMetadataPathPage(q, found, cursor)
}

// ReadMetadataPathPage refuses to inherit the in-memory capability: refreshing
// this archive requires decoding the entire file and cannot honor a page bound.
func (fs *FileStore) ReadMetadataPathPage(context.Context, MetadataPathQuery) (MetadataPathPage, error) {
	return MetadataPathPage{}, ErrMetadataPathPagingUnsupported
}
