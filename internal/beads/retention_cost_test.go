package beads_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	beadsexec "github.com/gastownhall/gascity/internal/beads/exec"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
)

type retentionScanCounter struct {
	beads.Store
	size, fetched int
}

func (s *retentionScanCounter) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.fetched += s.size // MemStore and FileStore inspect every stored row per List.
	return s.Store.List(q)
}

type retentionFixture func(*testing.T, []beads.Bead) (beads.Store, func() (int, int))

var retentionExtraFixtures = map[string]retentionFixture{}

func retentionFixtures() map[string]retentionFixture {
	fixtures := map[string]retentionFixture{
		"bd": func(t *testing.T, rows []beads.Bead) (beads.Store, func() (int, int)) {
			type wireRow struct {
				beads.Bead
				IssueType string `json:"issue_type"`
			}
			fetched := 0
			store := beads.NewBdStore(t.TempDir(), func(_, _ string, args ...string) ([]byte, error) {
				if args[0] == "query" {
					return []byte("[]"), nil
				}
				if args[0] != "list" {
					return nil, fmt.Errorf("unexpected command %v", args)
				}
				label := ""
				for _, arg := range args {
					if strings.HasPrefix(arg, "--label=") {
						label = strings.TrimPrefix(arg, "--label=")
					}
				}
				var selected []wireRow
				for _, b := range rows {
					if label == "" || slices.Contains(b.Labels, label) {
						selected = append(selected, wireRow{Bead: b, IssueType: b.Type})
					}
				}
				fetched += len(selected)
				return json.Marshal(selected)
			})
			return store, func() (int, int) { return fetched, 0 }
		},
		"memory": func(_ *testing.T, rows []beads.Bead) (beads.Store, func() (int, int)) {
			s := &retentionScanCounter{Store: beads.NewMemStoreFrom(len(rows), rows, nil), size: len(rows)}
			return s, func() (int, int) { return s.fetched, 0 }
		},
		"file": func(t *testing.T, rows []beads.Bead) (beads.Store, func() (int, int)) {
			fs := fsys.NewFake()
			payload, err := json.Marshal(struct {
				Seq   int          `json:"seq"`
				Beads []beads.Bead `json:"beads"`
			}{len(rows), rows})
			if err != nil {
				t.Fatal(err)
			}
			fs.Files["/fixture/beads.json"] = payload
			store, err := beads.OpenFileStore(fs, "/fixture/beads.json")
			if err != nil {
				t.Fatal(err)
			}
			s := &retentionScanCounter{Store: store, size: len(rows)}
			return s, func() (int, int) { return s.fetched, 0 }
		},
		"sqlite": beads.NewSQLiteRetentionFixtureForTest,
		"native": func(t *testing.T, rows []beads.Bead) (beads.Store, func() (int, int)) {
			return beads.NewNativeRetentionFixtureForTest(t, rows, nil)
		},
		"exec": func(t *testing.T, rows []beads.Bead) (beads.Store, func() (int, int)) {
			dir := t.TempDir()
			type wireRow struct {
				beads.Bead
				ParentID string `json:"parent_id"`
			}
			wire := make([]wireRow, len(rows))
			for i, b := range rows {
				wire[i] = wireRow{Bead: b, ParentID: b.ParentID}
			}
			payload, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "rows.json"), payload, 0o600); err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(dir, "provider")
			// The script emits an unbounded candidate set; the exec adapter owns
			// filtering. Each emitted result is independently counted at the source.
			body := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$ROW_COUNT\" >> \"$FIXTURE_DIR/counts\"\ncat \"$FIXTURE_DIR/rows.json\"\n"
			if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			s := beadsexec.NewStore(script)
			s.SetEnv(map[string]string{"FIXTURE_DIR": dir, "ROW_COUNT": strconv.Itoa(len(rows))})
			return s, func() (int, int) {
				data, err := os.ReadFile(filepath.Join(dir, "counts"))
				if err != nil {
					t.Fatal(err)
				}
				n := 0
				for _, line := range strings.Fields(string(data)) {
					count, err := strconv.Atoi(line)
					if err != nil {
						t.Fatal(err)
					}
					n += count
				}
				return n, 0
			}
		},
	}
	for name, factory := range retentionExtraFixtures {
		fixtures[name] = factory
	}
	return fixtures
}

func retentionCostRows(n, groups int) []beads.Bead {
	return beadstest.RetentionRows(n, groups)
}

// Retention's caller must not turn one unbounded fetch into one per page.
// Native discovery may revisit its first page, so its total bound is 2*N;
// every other adapter must fetch/scan exactly once, at most N rows.
func TestRetentionBackingCostAcrossStores(t *testing.T) {
	for name, factory := range retentionFixtures() {
		t.Run(name, func(t *testing.T) {
			for _, shape := range []struct{ n, groups int }{{500, 5}, {10001, 1}, {61988, 67}} {
				t.Run(fmt.Sprintf("%d_%d", shape.n, shape.groups), func(t *testing.T) {
					rows := retentionCostRows(shape.n, shape.groups)
					store, cost := factory(t, rows)
					// Exercise capability forwarding through the production live handle.
					cached := beads.NewCachingStore(store, nil)
					runs, err := orders.NewStore(beads.OrdersStore{Store: cached}).ClosedRunsForRetention()
					if err != nil {
						t.Fatal(err)
					}
					ids := make([]string, 0, len(runs))
					for _, run := range runs {
						ids = append(ids, run.ID)
					}
					slices.Sort(ids)
					if len(ids) != len(rows) {
						t.Fatalf("got %d runs, want %d", len(ids), len(rows))
					}
					for i, id := range ids {
						if id != rows[i].ID {
							t.Fatalf("row %d=%s want %s", i, id, rows[i].ID)
						}
					}
					fetched, largest := cost()
					bound := shape.n
					if name == "native" {
						bound *= 2
						if largest > 1000 {
							t.Errorf("native backing page=%d", largest)
						}
					}
					t.Logf("N=%d fetched=%d largest=%d bound=%d", shape.n, fetched, largest, bound)
					if fetched < shape.n || fetched > bound {
						t.Errorf("backing fetch cost %d outside [%d, %d]", fetched, shape.n, bound)
					}
				})
			}
		})
	}
}

func TestRetentionDescendantBackingCostAcrossStores(t *testing.T) {
	for name, factory := range retentionFixtures() {
		t.Run(name, func(t *testing.T) {
			rows := retentionCostRows(1001, 1)
			for i := range rows {
				rows[i].ParentID = "root"
				rows[i].Status = "open"
			}
			store, cost := factory(t, rows)
			got, err := beads.ListAllPages(beads.HandlesFor(beads.NewCachingStore(store, nil)).Live, beads.ListQuery{ParentID: "root", IncludeClosed: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(rows) {
				t.Fatalf("got %d children, want %d", len(got), len(rows))
			}
			fetched, largest := cost()
			t.Logf("N=%d fetched=%d largest=%d", len(rows), fetched, largest)
			if fetched != len(rows) || name == "native" && largest > 1000 {
				t.Errorf("descendant backing fetch grew: rows=%d largest=%d", fetched, largest)
			}
		})
	}
}

func TestNativeRetentionCorruptHistoryThroughOrders(t *testing.T) {
	rows := retentionCostRows(2001, 1)
	store, _ := beads.NewNativeRetentionFixtureForTest(t, rows, map[string][]byte{rows[0].ID: []byte("[]"), rows[1000].ID: []byte("[]")})
	runs, err := orders.NewStore(beads.OrdersStore{Store: store}).ClosedRunsForRetention()
	if len(runs) != len(rows)-2 {
		t.Errorf("usable runs=%d, want %d", len(runs), len(rows)-2)
	}
	if !slices.Equal(beads.SkippedListRowIDs(err), []string{rows[0].ID, rows[1000].ID}) {
		t.Errorf("decode warnings=%v", err)
	}
	for _, run := range runs {
		if run.ID == rows[0].ID || run.ID == rows[1000].ID {
			t.Errorf("unreadable row became deletion candidate: %s", run.ID)
		}
	}
}
