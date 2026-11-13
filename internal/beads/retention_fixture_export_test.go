package beads

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	beadslib "github.com/steveyegge/beads"
	sqlite "modernc.org/sqlite"
)

// NewNativeRetentionFixtureForTest counts physical storage results, including
// canonical-tier probes, rather than the logical pages returned by List.
func NewNativeRetentionFixtureForTest(t *testing.T, rows []Bead, corrupt map[string][]byte) (Store, func() (int, int)) {
	t.Helper()
	var all []*beadslib.Issue
	for _, b := range rows {
		issue, err := nativeIssueFromBead(b)
		if err != nil {
			t.Fatal(err)
		}
		if raw, ok := corrupt[b.ID]; ok {
			issue.Metadata = raw
		}
		all = append(all, issue)
	}
	all = backingSortLimitForTest(all, beadslib.IssueFilter{SortBy: "created"})
	fetched, largest := 0, 0
	storage := &nativeDoltStorageSpy{searchIssues: func(_ context.Context, _ string, f beadslib.IssueFilter) ([]*beadslib.Issue, error) {
		var candidates []*beadslib.Issue
		for _, row := range all {
			if f.SkipWisps {
				continue
			} // This fixture stores every row in the wisp table.
			if len(f.IDs) > 0 && !slices.Contains(f.IDs, row.ID) {
				continue
			}
			if f.ParentID != nil && !slices.ContainsFunc(row.Dependencies, func(d *beadslib.Dependency) bool {
				return d.Type == beadslib.DepParentChild && d.DependsOnID == *f.ParentID
			}) {
				continue
			}
			candidates = append(candidates, row)
		}
		page := filterNativeIssuesForTest(candidates, f)
		fetched += len(page)
		largest = max(largest, len(page))
		return page, nil
	}}
	return newNativeDoltStoreForTest(storage), func() (int, int) { return fetched, largest }
}

type retentionCountingConnector struct {
	path string
	rows *atomic.Int64
}

func (c retentionCountingConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := (&sqlite.Driver{}).Open(c.path)
	if err != nil {
		return nil, err
	}
	return retentionCountingConn{Conn: conn, rows: c.rows}, nil
}
func (c retentionCountingConnector) Driver() driver.Driver { return &sqlite.Driver{} }

type retentionCountingConn struct {
	driver.Conn
	rows *atomic.Int64
}

func (c retentionCountingConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
	if err != nil {
		return nil, err
	}
	if strings.Contains(q, " FROM beads b") || len(rows.Columns()) >= 10 {
		return retentionCountingRows{Rows: rows, rows: c.rows}, nil
	}
	return rows, nil
}

type retentionCountingRows struct {
	driver.Rows
	rows *atomic.Int64
}

func (r retentionCountingRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	if err == nil {
		r.rows.Add(1)
	}
	return err
}

// NewSQLiteRetentionFixtureForTest seeds in one transaction and counts actual
// driver rows read by List before its residual filtering and logical limit.
func NewSQLiteRetentionFixtureForTest(t *testing.T, rows []Bead) (Store, func() (int, int)) {
	t.Helper()
	opened, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := opened.(*SQLiteStore)
	t.Cleanup(func() {
		if err := s.CloseStore(); err != nil {
			t.Error(err)
		}
	})
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range rows {
		if err := s.upsertBeadTx(context.Background(), tx, b); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.readDB.Close(); err != nil {
		t.Fatal(err)
	}
	var fetched atomic.Int64
	s.readDB = sql.OpenDB(retentionCountingConnector{path: s.path, rows: &fetched})
	return s, func() (int, int) { return int(fetched.Load()), 0 }
}
