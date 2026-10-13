//go:build gascity_native_beads

package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/federation"
	_ "modernc.org/sqlite"
)

// TestDefaultScaleCheckCountsAndDemandFencesOwnershipOnTheDoltliteReadPath
// pins gf-c7n finding 1k (the gf-xid re-gate's Major at
// cmd/gc/build_desired_state.go:2061): with GC_NATIVE_DOLTLITE_BEADS=true the
// controller's demand count reads `ready` through DoltliteReadStore, whose
// ready projection skipped the labels, so the ownership fence of finding 1c
// (federation.MayClaim) saw another city's task as unowned and counted it as
// local demand; the seat it minted was refused at the authoritative claim
// and the pool cycled start/drain. Through the production store open
// (openOptimizedDoltliteStore) a foreign-owned task beside a local one now
// counts the local one only.
func TestDefaultScaleCheckCountsAndDemandFencesOwnershipOnTheDoltliteReadPath(t *testing.T) {
	const template = "hello-world/worker"
	foreign, _ := federation.OwnerLabel("jadegate")
	own, _ := federation.OwnerLabel("citadel")
	dir := newDoltliteDemandFixture(t, []doltliteDemandRow{
		{ID: "gc-foreign", Title: "another city's routed task", Labels: []string{foreign}},
		{ID: "gc-own", Title: "this city's routed task", Labels: []string{own}},
	}, template)

	t.Setenv(nativeDoltliteBeadsEnv, "true")
	backing := beads.NewBdStore(dir, func(string, string, ...string) ([]byte, error) {
		return nil, errors.New("the demand count must not shell out to bd on the DoltLite path")
	})
	store, ok := openOptimizedDoltliteStore(dir, backing)
	if !ok {
		t.Fatal("openOptimizedDoltliteStore refused the fixture; the test must run on the DoltLite read path")
	}
	if closer, ok := store.(interface{ CloseStore() error }); ok {
		defer closer.CloseStore() //nolint:errcheck // test cleanup
	}

	cfg := &config.City{
		Workspace:  config.Workspace{Name: "test-city"},
		Federation: config.FederationConfig{Identity: "citadel"},
		Agents:     []config.Agent{{Name: "worker", Dir: "hello-world"}},
	}
	counts, demand, partial, errs := defaultScaleCheckCountsAndDemand(cfg, []defaultScaleCheckTarget{{
		template: template,
		storeKey: "rig:hello-world",
		store:    store,
	}}, newWorkStartDeferralPass(time.Now(), nil))
	if len(errs) != 0 || len(partial) != 0 {
		t.Fatalf("errs = %v, partial = %v", errs, partial)
	}
	if got := counts[template]; got != 1 {
		t.Fatalf("counts[%q] = %d, want 1: the foreign-owned task must not be demand on the DoltLite read path (listed %v)", template, got, demand[template].WorkBeadIDs)
	}
	if ids := demand[template].WorkBeadIDs; len(ids) != 1 || ids[0] != "gc-own" {
		t.Fatalf("WorkBeadIDs = %v, want [gc-own]", ids)
	}
}

type doltliteDemandRow struct {
	ID     string
	Title  string
	Labels []string
}

// newDoltliteDemandFixture writes a DoltLite beads store (the metadata.json
// and the .beads/doltlite/hq.db the read store opens) holding open tasks
// routed to template, in the snapshot schema the read store's own tests use
// (internal/beads doltlite_read_store_test.go createTestDoltliteSchema).
func newDoltliteDemandFixture(t *testing.T, rows []doltliteDemandRow, template string) string {
	t.Helper()
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(filepath.Join(beadsDir, "doltlite"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"backend":"doltlite","database":"doltlite","dolt_database":"hq"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(beadsDir, "doltlite", "hq.db")+"?_busy_timeout=10000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck // test cleanup
	const issueColumns = `(
		id TEXT PRIMARY KEY, title TEXT, status TEXT, issue_type TEXT, priority INTEGER,
		created_at TEXT, updated_at TEXT, assignee TEXT, description TEXT, design TEXT,
		acceptance_criteria TEXT, notes TEXT, metadata TEXT, ephemeral INTEGER DEFAULT 0, no_history INTEGER DEFAULT 0)`
	const depColumns = `(issue_id TEXT, depends_on_id TEXT, depends_on_issue_id TEXT, depends_on_wisp_id TEXT, depends_on_external TEXT, type TEXT)`
	for _, stmt := range []string{
		`CREATE TABLE config (key TEXT PRIMARY KEY, value TEXT)`,
		`CREATE TABLE issues ` + issueColumns,
		`CREATE TABLE wisps ` + issueColumns,
		`CREATE TABLE labels (issue_id TEXT, label TEXT)`,
		`CREATE TABLE wisp_labels (issue_id TEXT, label TEXT)`,
		`CREATE TABLE dependencies ` + depColumns,
		`CREATE TABLE wisp_dependencies ` + depColumns,
		`INSERT INTO config (key, value) VALUES ('issue_prefix', 'gc')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("fixture schema: %v\n%s", err, stmt)
		}
	}
	now := time.Now().UTC()
	for i, row := range rows {
		metadata, err := json.Marshal(map[string]string{beadmeta.RoutedToMetadataKey: template})
		if err != nil {
			t.Fatal(err)
		}
		created := now.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		if _, err := db.Exec(`INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, assignee, description, design, acceptance_criteria, notes, metadata)
			VALUES (?, ?, 'open', 'task', 2, ?, ?, '', '', '', '', '', ?)`, row.ID, row.Title, created, created, string(metadata)); err != nil {
			t.Fatalf("fixture row %s: %v", row.ID, err)
		}
		for _, label := range row.Labels {
			if _, err := db.Exec(`INSERT INTO labels (issue_id, label) VALUES (?, ?)`, row.ID, label); err != nil {
				t.Fatalf("fixture label %s: %v", row.ID, err)
			}
		}
	}
	return dir
}
