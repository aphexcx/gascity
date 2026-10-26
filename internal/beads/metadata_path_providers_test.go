package beads

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

func TestBdMetadataPathPageBoundsBackingRead(t *testing.T) {
	for _, label := range []bool{false, true} {
		t.Run(fmt.Sprint(label), func(t *testing.T) {
			calls := 0
			store := NewBdStore("/fixture", func(_, _ string, args ...string) ([]byte, error) {
				calls++
				if len(args) != 3 || args[0] != "sql" || !strings.HasSuffix(args[1], "ORDER BY page_id LIMIT 256") {
					t.Fatalf("unbounded command: %v", args)
				}
				if !strings.Contains(args[1], "JSON_EXTRACT") || !strings.Contains(args[1], "INSTR(") || !strings.Contains(args[1], "wisps") {
					t.Fatalf("unscoped query: %s", args[1])
				}
				if !strings.Contains(args[1], "NOT EXISTS (SELECT 1 FROM wisps other") {
					t.Fatalf("canonical wisp must shadow colliding issue: %s", args[1])
				}
				if calls == 2 {
					if !strings.Contains(args[1], metadataSQLLiteral("73657373696F6E2D323535", false)) {
						t.Fatalf("missing last-ID cursor: %s", args[1])
					}
					return []byte("[]"), nil
				}
				var rows []map[string]string
				for i := 0; i < 256; i++ {
					rows = append(rows, map[string]string{"id": fmt.Sprintf("session-%03d", i), "issue_type": "session", "status": "closed", "metadata_json": `{"work_dir":"./shared","template":"removed"}`})
				}
				return json.Marshal(rows)
			})
			q := MetadataPathQuery{Type: "session", Keys: []string{"gc.work_dir", "work_dir"}, BaseDir: "/city", Paths: []string{"/city/shared"}}
			if label {
				q.Type = ""
				q.Label = "gc:session"
			}
			page, err := store.ReadMetadataPathPage(context.Background(), q)
			if err != nil || page.Complete || len(page.Beads) != 256 {
				t.Fatalf("page=%+v err=%v", page, err)
			}
			q.Cursor = page.Next
			page, err = store.ReadMetadataPathPage(context.Background(), q)
			if err != nil || !page.Complete || calls != 2 {
				t.Fatalf("complete=%v calls=%d err=%v", page.Complete, calls, err)
			}
		})
	}
}

func TestNativeMetadataPathPageBoundsBackingAndContinuesTies(t *testing.T) {
	calls := 0
	connector := metadataPageConnector{query: func(query string) (driver.Rows, error) {
		calls++
		if !strings.HasSuffix(query, "ORDER BY page_id LIMIT 256") || !strings.Contains(query, "NOT EXISTS (SELECT 1 FROM wisps other") || !strings.Contains(query, "JSON_EXTRACT") {
			t.Fatalf("unbounded or noncanonical SQL: %s", query)
		}
		start, end := 0, 256
		if calls == 2 {
			if !strings.Contains(query, metadataSQLLiteral("73657373696F6E2D323535", false)) {
				t.Fatalf("wrong ID cursor: %s", query)
			}
			start, end = 256, 257
		}
		rows := &metadataPageRows{}
		for i := start; i < end; i++ {
			rows.values = append(rows.values, []driver.Value{fmt.Sprintf("session-%03d", i), "session", "closed", `{"work_dir":"shared"}`, "", int64(0)})
		}
		return rows, nil
	}}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { _ = db.Close() })
	storage := &metadataPageStorage{Storage: &nativeDoltStorageSpy{searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
		t.Fatal("merged SearchIssues cannot safely page canonical collisions")
		return nil, nil
	}}, db: db}
	store := newNativeDoltStoreForTest(storage)
	q := MetadataPathQuery{Type: "session", Keys: []string{"work_dir"}, BaseDir: "/city", Paths: []string{"/city/shared"}}
	first, err := store.ReadMetadataPathPage(context.Background(), q)
	if err != nil || first.Complete || len(first.Beads) != 256 {
		t.Fatalf("first rows=%d complete=%v err=%v", len(first.Beads), first.Complete, err)
	}
	q.Cursor = first.Next
	last, err := store.ReadMetadataPathPage(context.Background(), q)
	if err != nil || !last.Complete || len(last.Beads) != 1 {
		t.Fatalf("last=%+v err=%v", last, err)
	}
}

func TestNativeMetadataPathPageFailsClosedWithoutSQL(t *testing.T) {
	store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{})
	page, err := store.ReadMetadataPathPage(context.Background(), MetadataPathQuery{Type: "session", Keys: []string{"work_dir"}, BaseDir: "/city", Paths: []string{"/city/shared"}})
	if !errors.Is(err, ErrMetadataPathPagingUnsupported) || page.Complete {
		t.Fatalf("unsupported native backend: %+v, %v", page, err)
	}
}

func TestNativeMetadataPathPageWitnessesFilteredRows(t *testing.T) {
	db := sql.OpenDB(metadataPageConnector{query: func(string) (driver.Rows, error) {
		return &metadataPageRows{values: [][]driver.Value{{"other", "session", "closed", `{"work_dir":"/elsewhere/shared"}`, "", int64(0)}}}, nil
	}})
	t.Cleanup(func() { _ = db.Close() })
	store := newNativeDoltStoreForTest(&metadataPageStorage{Storage: &nativeDoltStorageSpy{}, db: db})
	page, err := store.ReadMetadataPathPage(context.Background(), MetadataPathQuery{Type: "session", Keys: []string{"work_dir"}, BaseDir: "/city", Paths: []string{"/city/shared"}})
	if err != nil || !page.Complete || len(page.Beads) != 0 || !store.SawRows() {
		t.Fatalf("filtered page lost backing witness: %+v, %v, saw rows=%v", page, err, store.SawRows())
	}
}

type metadataPageStorage struct {
	beadslib.Storage
	db *sql.DB
}

func (s *metadataPageStorage) DB() *sql.DB { return s.db }

type metadataPageConnector struct {
	query func(string) (driver.Rows, error)
}

func (c metadataPageConnector) Connect(context.Context) (driver.Conn, error) {
	return metadataPageConn{c}, nil
}
func (c metadataPageConnector) Driver() driver.Driver { return metadataPageDriver{} }

type metadataPageDriver struct{}

func (metadataPageDriver) Open(string) (driver.Conn, error) { return nil, driver.ErrSkip }

type metadataPageConn struct{ metadataPageConnector }

func (c metadataPageConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (c metadataPageConn) Close() error                        { return nil }
func (c metadataPageConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }
func (c metadataPageConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	return c.query(query)
}

type metadataPageRows struct{ values [][]driver.Value }

func (r *metadataPageRows) Columns() []string {
	return []string{"id", "issue_type", "status", "metadata_json", "page_id", "ephemeral"}
}
func (r *metadataPageRows) Close() error { return nil }
func (r *metadataPageRows) Next(dest []driver.Value) error {
	if len(r.values) == 0 {
		return io.EOF
	}
	copy(dest, r.values[0])
	r.values = r.values[1:]
	return nil
}

func TestBdMetadataPathPageRejectsMalformedAndOversizedPages(t *testing.T) {
	for _, data := range []string{`[{"id":"session-1","metadata_json":"broken"}]`, `{"unexpected":"envelope"}`, `null`} {
		store := NewBdStore("/fixture", func(string, string, ...string) ([]byte, error) { return []byte(data), nil })
		page, err := store.ReadMetadataPathPage(context.Background(), MetadataPathQuery{Type: "session", Keys: []string{"work_dir"}, BaseDir: "/city", Paths: []string{"/city/shared"}})
		if err == nil || page.Complete {
			t.Fatalf("invalid page authorized absence: %+v %v", page, err)
		}
	}
	if page, err := finishMetadataPathPage(MetadataPathQuery{}, make([]Bead, 257), metadataPathCursor{}); err == nil || page.Complete {
		t.Fatal("oversized backing page accepted")
	}
}
