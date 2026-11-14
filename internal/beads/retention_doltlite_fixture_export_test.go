//go:build gascity_native_beads

package beads

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// NewDoltliteRetentionFixtureForTest counts hydrated rows at the SQL driver.
func NewDoltliteRetentionFixtureForTest(t *testing.T, rows []Bead) (Store, func() (int, int)) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	createTestDoltliteSchema(t, db)
	for _, statement := range []string{
		"CREATE INDEX fixture_labels ON wisp_labels(issue_id,label)",
		"CREATE INDEX fixture_deps ON wisp_dependencies(issue_id,type)",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range rows {
		_, err = tx.Exec("INSERT INTO wisps (id,title,status,issue_type,priority,created_at,updated_at,assignee,description,design,acceptance_criteria,notes,metadata,ephemeral,no_history) VALUES (?,?,?,?,0,?,?,'','','','','','{}',1,0)", b.ID, b.Title, b.Status, b.Type, b.CreatedAt.Format(time.RFC3339Nano), b.CreatedAt.Format(time.RFC3339Nano))
		if err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		for _, label := range b.Labels {
			if _, err := tx.Exec("INSERT INTO wisp_labels(issue_id,label) VALUES (?,?)", b.ID, label); err != nil {
				_ = tx.Rollback()
				t.Fatal(err)
			}
		}
		if b.ParentID != "" {
			if _, err := tx.Exec("INSERT INTO wisp_dependencies(issue_id,depends_on_id,type) VALUES (?,?,'parent-child')", b.ID, b.ParentID); err != nil {
				_ = tx.Rollback()
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var fetched atomic.Int64
	counted := sql.OpenDB(retentionCountingConnector{path: path, rows: &fetched})
	t.Cleanup(func() {
		if err := counted.Close(); err != nil {
			t.Error(err)
		}
	})
	store := &DoltliteReadStore{BdStore: NewBdStore(dir, nil), db: counted}
	return store, func() (int, int) { return int(fetched.Load()), 0 }
}
