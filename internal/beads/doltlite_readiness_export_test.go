//go:build gascity_native_beads

package beads

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
)

// NewDoltliteReadinessFixture exposes the production read path to the shared
// readiness contract. The write methods only seed the SQLite snapshot; bd
// write semantics have their own provider tests.
func NewDoltliteReadinessFixture(t *testing.T) Store {
	t.Helper()
	s := newDoltliteStoreWithBulkIssues(t, 0)
	db := openTestDoltliteWriter(t, s.db)
	t.Cleanup(func() { _ = db.Close() })
	return &doltliteReadinessFixture{DoltliteReadStore: s, db: db, t: t}
}

type doltliteReadinessFixture struct {
	*DoltliteReadStore
	db   *sql.DB
	t    *testing.T
	next int
}

func (s *doltliteReadinessFixture) Create(b Bead) (Bead, error) {
	s.next++
	b.ID = fmt.Sprintf("gc-ready-%d", s.next)
	insertTestDoltliteIssue(s.t, s.db, "issues", "labels", "dependencies", testDoltliteIssue{
		ID: b.ID, Title: b.Title, Status: b.Status, IssueType: b.Type, Metadata: b.Metadata,
	})
	s.resetOrderRunCache()
	return s.Get(b.ID)
}

func (s *doltliteReadinessFixture) SetMetadataBatch(id string, kvs map[string]string) error {
	b, err := s.Get(id)
	if err != nil {
		return err
	}
	if b.Metadata == nil {
		b.Metadata = make(map[string]string)
	}
	for k, v := range kvs {
		b.Metadata[k] = v
	}
	raw, err := json.Marshal(b.Metadata)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec("UPDATE issues SET metadata = ? WHERE id = ?", string(raw), id); err != nil {
		return err
	}
	s.resetOrderRunCache()
	return nil
}
