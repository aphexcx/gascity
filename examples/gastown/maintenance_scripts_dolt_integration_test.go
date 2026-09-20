//go:build integration || dolt_integration

package gastown_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReaperWorkflowRootCleanupRealDoltSemantics(t *testing.T) {
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skipf("dolt not found: %v", err)
	}
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skipf("timeout not found: %v", err)
	}

	cityDir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "dolt")
	for _, db := range []string{"citydb", "rigdb"} {
		dbDir := filepath.Join(dataDir, db)
		if err := os.MkdirAll(dbDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dbDir, err)
		}
		runDoltForMaintenanceTest(t, doltPath, dbDir, "init", "--name", "Gas City", "--email", "test@example.com")
		runDoltSQLForMaintenanceTest(t, doltPath, dbDir, maintenanceReaperSchemaSQL())
	}

	runDoltSQLForMaintenanceTest(t, doltPath, filepath.Join(dataDir, "citydb"), maintenanceReaperCitySeedSQL())
	runDoltForMaintenanceTest(t, doltPath, filepath.Join(dataDir, "citydb"), "add", ".")
	runDoltForMaintenanceTest(t, doltPath, filepath.Join(dataDir, "citydb"), "commit", "-m", "seed city workflow roots")

	runDoltSQLForMaintenanceTest(t, doltPath, filepath.Join(dataDir, "rigdb"), maintenanceReaperRigSeedSQL())
	runDoltForMaintenanceTest(t, doltPath, filepath.Join(dataDir, "rigdb"), "add", ".")
	runDoltForMaintenanceTest(t, doltPath, filepath.Join(dataDir, "rigdb"), "commit", "-m", "seed rig workflow roots")

	port := startDoltServerForMaintenanceTest(t, doltPath, dataDir)
	waitForDoltServerForMaintenanceTest(t, doltPath, port, "citydb")
	writeCityBeadsMetadata(t, cityDir, "citydb")
	rigDir := filepath.Join(cityDir, "rigs", "rig-with-db-alias")
	writeCityBeadsMetadata(t, rigDir, "rigdb")
	writeSiteRigBinding(t, cityDir, "rig-with-db-alias", rigDir)

	binDir := t.TempDir()
	bdLog := filepath.Join(t.TempDir(), "bd.log")
	anomalyLog := filepath.Join(t.TempDir(), "anomalies.log")
	escalateStatusLog := filepath.Join(t.TempDir(), "escalate-status.log")
	escalatePath := filepath.Join(binDir, "escalate")
	// Record the real hook's status without replacing its timeout behavior.
	writeExecutable(t, escalatePath, `#!/bin/sh
"$CORE_ESCALATE_SCRIPT" "$@"
status=$?
printf '%s\n' "$status" > "$ESCALATE_STATUS_LOG"
exit "$status"
`)
	if err := os.Symlink(doltPath, filepath.Join(binDir, "dolt")); err != nil {
		t.Fatalf("Symlink(dolt): %v", err)
	}
	writeRealDoltBdDouble(t, filepath.Join(binDir, "bd"))
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
case "$1 $2" in
  "mail send")
    printf '%s\n' "$*" >> "$ANOMALY_LOG"
    if [ "${ESCALATE_SEND_TIMEOUT:-}" = 1 ]; then
      # Simulate a store blocked before persisting mail. The real hook's
      # one-second timeout is the behavior under test, not a readiness wait.
      exec sleep 5
    fi
    exit "${ESCALATE_EXIT_STATUS:-0}"
    ;;
  "session prune")
    printf '{"count":0}\n'
    ;;
esac
exit 0
`)

	env := map[string]string{
		"BD_CALL_LOG":                        bdLog,
		"ANOMALY_LOG":                        anomalyLog,
		"GC_ESCALATE_SCRIPT":                 escalatePath,
		"CORE_ESCALATE_SCRIPT":               coreScriptPath("escalate.sh"),
		"ESCALATE_STATUS_LOG":                escalateStatusLog,
		"GC_ESCALATION_RECIPIENT":            "test-recipient",
		"GC_ESCALATE_SEND_TIMEOUT_SECS":      "1",
		"GC_ESCALATE_TIMEOUT_IS_UNCONFIRMED": "",
		"GC_CITY":                            cityDir,
		"GC_CITY_PATH":                       cityDir,
		"GC_DOLT_HOST":                       "127.0.0.1",
		"GC_DOLT_PORT":                       fmt.Sprintf("%d", port),
		"GC_DOLT_USER":                       "root",
		"GC_DOLT_PASSWORD":                   "",
		"FAKE_RIG_LIST_JSON":                 `{"rigs":[{"name":"rig-with-db-alias","hq":false}]}`,
		"FAKE_SCOPE_DBS":                     "city=citydb rig:rig-with-db-alias=rigdb",
		"PATH":                               binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	// Even successful escalation during a dry run must not consume the notification.
	statePath := filepath.Join(cityDir, ".beads", "reaper-system-skips-state.tsv")
	env["ESCALATE_EXIT_STATUS"] = "0"
	env["GC_REAPER_DRY_RUN"] = "1"
	runScript(t, coreScriptPath("reaper.sh"), env)
	dryAnomalies, err := os.ReadFile(anomalyLog)
	if err != nil || !strings.Contains(string(dryAnomalies), "citydb: 1 stale system issues skipped (gc: labels)") {
		t.Fatalf("dry run did not escalate the skip anomaly: %v\n%s", err, dryAnomalies)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("dry run created skip-count state: %v", err)
	}
	env["GC_REAPER_DRY_RUN"] = ""
	if err := os.WriteFile(anomalyLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env["ESCALATE_EXIT_STATUS"] = "1"
	runScript(t, coreScriptPath("reaper.sh"), env)
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("failed escalation created skip-count state: %v", err)
	}
	// A timeout must also leave the baseline available for retry, even though
	// callers without the opt-in retain the hook's best-effort success status.
	env["ESCALATE_EXIT_STATUS"] = "0"
	env["ESCALATE_SEND_TIMEOUT"] = "1"
	legacyPath := filepath.Join(binDir, "legacy-escalate")
	writeExecutable(t, legacyPath, `#!/bin/sh
exec "$GC_ESCALATE_SCRIPT" --subject "legacy timeout" --message "test"
`)
	legacyOut, err := runScriptResult(t, legacyPath, env)
	if err != nil || !strings.Contains(string(legacyOut), "wake exceeded 1s") {
		t.Fatalf("legacy timeout must return success: %v\n%s", err, legacyOut)
	}
	t.Log("legacy caller: real escalation hook timed out and returned 0")
	if err := os.WriteFile(anomalyLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runScriptResult(t, coreScriptPath("reaper.sh"), env)
	if err != nil {
		t.Fatalf("reaper failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Errorf("timed-out escalation created skip-count state: %v", err)
	}
	status, err := os.ReadFile(escalateStatusLog)
	if err != nil || string(status) != "124\n" {
		t.Errorf("timed-out real hook status = %q, want 124: %v", status, err)
	}
	t.Log("run A: real escalation hook timed out; citydb baseline must remain absent")

	bdData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v", err)
	}
	if !strings.Contains(string(bdData), " issue-close --reason stale inactive workflow root auto-closed by reaper") {
		t.Fatalf("reaper did not close city workflow issue root through bd close:\n%s", bdData)
	}

	cityWispStatuses := queryMaintenanceStatusByID(t, doltPath, port, "citydb", "wisps")
	requireMaintenanceStatuses(t, cityWispStatuses, map[string]string{
		"wisp-close":               "closed",
		"wisp-city-store-root":     "closed",
		"wisp-cross-store-root":    "open",
		"wisp-held":                "blocked",
		"wisp-non-root-workflow":   "open",
		"wisp-recent-root":         "open",
		"wisp-nested-root":         "open",
		"wisp-subroot":             "closed",
		"wisp-live-grandchild":     "in_progress",
		"wisp-recent-closed-child": "closed",
	})

	cityIssueStatuses := queryMaintenanceStatusByID(t, doltPath, port, "citydb", "issues")
	requireMaintenanceStatuses(t, cityIssueStatuses, map[string]string{
		"issue-city-store-root":   "closed",
		"issue-close":             "closed",
		"issue-cross-store-root":  "open",
		"issue-held":              "blocked",
		"issue-dep-root":          "open",
		"issue-dep-live":          "in_progress",
		"issue-non-root-workflow": "open",
	})

	// Step 5 must preserve system records without changing ordinary stale closes.
	t.Run("stale system issues", func(t *testing.T) {
		if cityIssueStatuses["stale-binding"] != "open" {
			t.Errorf("stale gc:extmsg-binding status = %q, want open", cityIssueStatuses["stale-binding"])
		}
		if cityIssueStatuses["stale-plain"] != "closed" {
			t.Errorf("plain stale issue status = %q, want closed", cityIssueStatuses["stale-plain"])
		}
		if !strings.Contains(string(bdData), "close stale-plain --reason stale:auto-closed by reaper") {
			t.Errorf("plain stale issue missing stale close reason:\n%s", bdData)
		}
		if strings.Contains(string(bdData), "close stale-binding ") {
			t.Errorf("system record was sent to bd close:\n%s", bdData)
		}
		// Multiple system labels on one record must still count as one skip.
		if !strings.Contains(string(out), "skipped_system_issues:1,") {
			t.Errorf("summary missing one system skip:\n%s", out)
		}
		anomalies, err := os.ReadFile(anomalyLog)
		if err != nil {
			t.Fatalf("ReadFile(anomalies): %v", err)
		}
		if !strings.Contains(string(anomalies), "citydb: 1 stale system issues skipped (gc: labels)") {
			t.Errorf("anomaly record missing one system skip:\n%s", anomalies)
		}
	})

	rigWispStatuses := queryMaintenanceStatusByID(t, doltPath, port, "rigdb", "wisps")
	requireMaintenanceStatuses(t, rigWispStatuses, map[string]string{
		"rig-wisp-close":            "closed",
		"rig-wisp-store-root":       "closed",
		"rig-wisp-other-store-root": "open",
	})

	rigIssueStatuses := queryMaintenanceStatusByID(t, doltPath, port, "rigdb", "issues")
	requireMaintenanceStatuses(t, rigIssueStatuses, map[string]string{
		"rig-issue-preserve": "open",
	})

	// A steady population must stay visible in the summary without sending
	// another anomaly; a changed population must notify again.
	t.Run("system skip count changes", func(t *testing.T) {
		runServerSQL := func(query string) {
			t.Helper()
			runDoltForMaintenanceTest(t, doltPath, dataDir,
				"--host", "127.0.0.1", "--port", fmt.Sprint(port), "--user", "root", "--no-tls", "--use-db", "citydb",
				"sql", "-q", query)
		}
		readAnomalies := func() string {
			t.Helper()
			data, err := os.ReadFile(anomalyLog)
			if err != nil {
				t.Fatal(err)
			}
			return string(data)
		}
		runAndCheck := func(wantCount int, wantAnomaly bool) {
			t.Helper()
			before := readAnomalies()
			out, err := runScriptResult(t, coreScriptPath("reaper.sh"), env)
			if err != nil {
				t.Fatalf("reaper failed: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), fmt.Sprintf("skipped_system_issues:%d,", wantCount)) {
				t.Errorf("summary missing %d system skips:\n%s", wantCount, out)
			}
			newAnomalies := strings.TrimPrefix(readAnomalies(), before)
			gotAnomaly := strings.Contains(newAnomalies, "stale system issues skipped (gc: labels)")
			if gotAnomaly != wantAnomaly {
				t.Errorf("system skip anomaly = %t, want %t; new anomalies:\n%s", gotAnomaly, wantAnomaly, newAnomalies)
			}
			if wantAnomaly && !strings.Contains(newAnomalies, fmt.Sprintf("citydb: %d stale system issues skipped (gc: labels)", wantCount)) {
				t.Errorf("anomaly missing changed citydb count %d:\n%s", wantCount, newAnomalies)
			}
			t.Logf("summary skips=%d; new skip anomaly=%t; dry run=%q", wantCount, gotAnomaly, env["GC_REAPER_DRY_RUN"])
		}
		// Retry the same population after delivery recovers, then suppress repeats.
		env["ESCALATE_SEND_TIMEOUT"] = ""
		runAndCheck(1, true)
		state, err := os.ReadFile(statePath)
		if err != nil || string(state) != "citydb\t1\n" {
			t.Errorf("recovered escalation did not save baseline: %v; state=%q", err, state)
		}
		t.Log("run B: escalation recovered; citydb baseline must be one")
		runAndCheck(1, false)
		t.Log("run C: unchanged population must not repeat the skip anomaly")
		runServerSQL(`INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata)
VALUES ('stale-binding-added', 'new routing record', 'open', 'task', 2, DATE_SUB(NOW(), INTERVAL 800 HOUR), DATE_SUB(NOW(), INTERVAL 800 HOUR), '', '{}');
INSERT INTO labels (issue_id, label) VALUES ('stale-binding-added', 'gc:extmsg-binding');`)
		runAndCheck(2, true)

		// Dry runs must not overwrite an existing baseline either.
		before, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatal(err)
		}
		runServerSQL("UPDATE issues SET status='closed' WHERE id IN ('stale-binding', 'stale-binding-added')")
		env["GC_REAPER_DRY_RUN"] = "1"
		runAndCheck(0, true)
		after, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Errorf("dry run changed skip-count state: before=%q after=%q", before, after)
		}
		env["GC_REAPER_DRY_RUN"] = ""
		runAndCheck(0, true)
		runAndCheck(0, false)
	})
}

// writeRealDoltBdDouble installs a bd double that applies the verbs the
// reaper sends through `gc bd` (close, update --set-metadata) to the scope's
// database on the real dolt sql-server, so the test observes the end state of
// the reaper's real-Dolt selections. The scope's database comes from
// FAKE_SCOPE_DBS via the fake gc route (GC_FAKE_SCOPE). Every call is logged to
// BD_CALL_LOG.
func writeRealDoltBdDouble(t *testing.T, path string) {
	t.Helper()
	writeExecutable(t, path, `#!/bin/sh
set -e
printf '%s\n' "$*" >> "$BD_CALL_LOG"
db=""
for pair in ${FAKE_SCOPE_DBS:-}; do
  case "$pair" in
    "${GC_FAKE_SCOPE:-city}="*) db="${pair#*=}" ;;
  esac
done
run_sql() {
  DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}" dolt --host "$GC_DOLT_HOST" --port "$GC_DOLT_PORT" --user "$GC_DOLT_USER" --no-tls --use-db "$db" sql -q "$1"
}
verb="$1"
shift
ids=""
sets=""
while [ $# -gt 0 ]; do
  case "$1" in
    --reason) shift ;;
    --force|--json) ;;
    --set-metadata)
      key="${2%%=*}"
      value="${2#*=}"
      sets="$sets, '\$.\"$key\"', '$value'"
      shift
      ;;
    -*) ;;
    *) ids="$ids $1" ;;
  esac
  shift
done
case "$verb" in
  prune)
    printf '{"pruned_count":0}\n'
    ;;
  purge)
    printf '{"purged_count":0}\n'
    ;;
  backup)
    printf '{"backup":{},"dolt":{"configured":false}}\n'
    ;;
  close|update)
    # Report every applied id in bd's --json array shape.
    sep=""
    printf '['
    for id in $ids; do
      if [ "$verb" = close ]; then
        run_sql "UPDATE issues SET status='closed', closed_at=NOW() WHERE id='$id'; UPDATE wisps SET status='closed', closed_at=NOW() WHERE id='$id'; CALL DOLT_COMMIT('-Am', 'test bd close')" >/dev/null 2>&1 || true
      else
        run_sql "UPDATE issues SET metadata = JSON_SET(COALESCE(metadata, JSON_OBJECT())$sets) WHERE id='$id'; UPDATE wisps SET metadata = JSON_SET(COALESCE(metadata, JSON_OBJECT())$sets) WHERE id='$id'; CALL DOLT_COMMIT('-Am', 'test bd update')" >/dev/null 2>&1 || true
      fi
      printf '%s{"id":"%s"}' "$sep" "$id"
      sep=","
    done
    printf ']\n'
    ;;
esac
exit 0
`)
}

func maintenanceReaperSchemaSQL() string {
	return `
CREATE TABLE wisps (
  id VARCHAR(64) PRIMARY KEY,
  title VARCHAR(255),
  status VARCHAR(32),
  issue_type VARCHAR(32),
  priority BIGINT,
  created_at DATETIME(6),
  updated_at DATETIME(6),
  closed_at DATETIME(6),
  assignee VARCHAR(255),
  description LONGTEXT,
  metadata JSON
);
CREATE TABLE issues (
  id VARCHAR(64) PRIMARY KEY,
  title VARCHAR(255),
  status VARCHAR(32),
  issue_type VARCHAR(32),
  priority BIGINT,
  created_at DATETIME(6),
  updated_at DATETIME(6),
  closed_at DATETIME(6),
  assignee VARCHAR(255),
  description LONGTEXT,
  metadata JSON
);
CREATE TABLE dependencies (
  issue_id VARCHAR(64),
  depends_on_issue_id VARCHAR(64),
  depends_on_wisp_id VARCHAR(64),
  depends_on_external VARCHAR(64),
  type VARCHAR(32)
);
CREATE TABLE wisp_dependencies (
  issue_id VARCHAR(64),
  depends_on_issue_id VARCHAR(64),
  depends_on_wisp_id VARCHAR(64),
  depends_on_external VARCHAR(64),
  type VARCHAR(32)
);
CREATE TABLE labels (
  issue_id VARCHAR(64),
  label VARCHAR(255)
);
CREATE TABLE wisp_labels (
  issue_id VARCHAR(64),
  label VARCHAR(255)
);
`
}

func maintenanceReaperCitySeedSQL() string {
	return `
INSERT INTO wisps (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES
  ('wisp-close', 'closeable root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('wisp-city-store-root', 'closeable city-store root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_store_ref":"city:test-city"}'),
  ('wisp-cross-store-root', 'cross-store root preserved', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_store_ref":"rig:other"}'),
  ('wisp-held', 'held root', 'blocked', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('wisp-non-root-workflow', 'non-root topology bead preserved', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_bead_id":"wisp-nested-root"}'),
  ('wisp-recent-root', 'recent descendant root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('wisp-recent-closed-child', 'recent closed child', 'closed', 'task', 2, '2026-01-01 00:00:00', NOW(), '', '{"gc.root_bead_id":"wisp-recent-root"}'),
  ('wisp-nested-root', 'nested root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('wisp-subroot', 'nested subroot', 'closed', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.root_bead_id":"wisp-nested-root"}'),
  ('wisp-live-grandchild', 'live nested child', 'in_progress', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{}');
INSERT INTO wisp_dependencies (issue_id, depends_on_wisp_id, type) VALUES
  ('wisp-subroot', 'wisp-nested-root', 'tracks'),
  ('wisp-live-grandchild', 'wisp-subroot', 'tracks');
INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES
  ('issue-close', 'closeable city issue root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('issue-city-store-root', 'closeable city-store issue root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_store_ref":"city:test-city"}'),
  ('issue-cross-store-root', 'cross-store issue root preserved', 'open', 'task', 1, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_store_ref":"rig:other"}'),
  ('issue-held', 'held city issue root', 'blocked', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('issue-non-root-workflow', 'non-root issue topology bead preserved', 'open', 'task', 1, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_bead_id":"issue-dep-root"}'),
  ('issue-dep-root', 'dependency-protected issue root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('issue-dep-live', 'live issue dependency child', 'in_progress', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{}');
INSERT INTO dependencies (issue_id, depends_on_issue_id, type) VALUES
  ('issue-dep-live', 'issue-dep-root', 'blocks');
INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES
  ('stale-binding', 'live routing record', 'open', 'task', 2, DATE_SUB(NOW(), INTERVAL 800 HOUR), DATE_SUB(NOW(), INTERVAL 800 HOUR), '', '{}'),
  ('stale-plain', 'ordinary backlog issue', 'open', 'task', 2, DATE_SUB(NOW(), INTERVAL 800 HOUR), DATE_SUB(NOW(), INTERVAL 800 HOUR), '', '{}');
INSERT INTO labels (issue_id, label) VALUES
  ('stale-binding', 'gc:extmsg-binding'),
  ('stale-binding', 'gc:system-fixture'),
  ('stale-binding', 'owner:test'),
  ('stale-plain', 'owner:test');
`
}

func maintenanceReaperRigSeedSQL() string {
	return `
INSERT INTO wisps (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES
  ('rig-wisp-close', 'closeable non-city wisp root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}'),
  ('rig-wisp-store-root', 'closeable rig-store wisp root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_store_ref":"rig:rig-with-db-alias"}'),
  ('rig-wisp-other-store-root', 'other rig-store root preserved', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow","gc.root_store_ref":"rig:other"}');
INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES
  ('rig-issue-preserve', 'non-city issue root', 'open', 'task', 2, '2026-01-01 00:00:00', '2026-01-01 00:00:00', '', '{"gc.kind":"workflow"}');
`
}

func runDoltForMaintenanceTest(t *testing.T, doltPath, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, doltPath, args...)
	cmd.Dir = dir
	// Server commands use the fixture's passwordless root account; local
	// commands must not receive a password without an explicit user.
	if len(args) > 0 && args[0] == "--host" {
		cmd.Env = append(os.Environ(), "DOLT_CLI_PASSWORD=")
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dolt %s failed in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

func runDoltSQLForMaintenanceTest(t *testing.T, doltPath, dir, query string) string {
	t.Helper()
	return runDoltForMaintenanceTest(t, doltPath, dir, "sql", "-q", query)
}

func startDoltServerForMaintenanceTest(t *testing.T, doltPath, dataDir string) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("Close listener: %v", err)
	}

	logPath := filepath.Join(dataDir, "sql-server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("Create(%s): %v", logPath, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, doltPath, "sql-server",
		"-H", "127.0.0.1",
		"-P", fmt.Sprintf("%d", port),
		"--data-dir", dataDir,
		"--loglevel", "warning",
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("Start dolt sql-server: %v", err)
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-waitCh:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-waitCh
		}
		_ = logFile.Close()
	})
	return port
}

func waitForDoltServerForMaintenanceTest(t *testing.T, doltPath string, port int, db string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var lastOut []byte
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cmd := exec.CommandContext(ctx, doltPath,
			"--host", "127.0.0.1",
			"--port", fmt.Sprintf("%d", port),
			"--user", "root",
			"--no-tls",
			"--use-db", db,
			"sql", "-q", "SELECT 1",
		)
		cmd.Env = append(os.Environ(), "DOLT_CLI_PASSWORD=")
		lastOut, lastErr = cmd.CombinedOutput()
		cancel()
		if lastErr == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("dolt sql-server did not become ready on port %d: %v\n%s", port, lastErr, lastOut)
}

func queryMaintenanceStatusByID(t *testing.T, doltPath string, port int, db string, table string) map[string]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, doltPath,
		"--host", "127.0.0.1",
		"--port", fmt.Sprintf("%d", port),
		"--user", "root",
		"--no-tls",
		"--use-db", db,
		"sql", "-r", "csv", "-q", fmt.Sprintf("SELECT id,status FROM %s ORDER BY id", table),
	)
	cmd.Env = append(os.Environ(), "DOLT_CLI_PASSWORD=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("query %s.%s statuses: %v\n%s", db, table, err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "id,status" {
		t.Fatalf("unexpected status output for %s.%s:\n%s", db, table, out)
	}
	statuses := make(map[string]string)
	for _, line := range lines[1:] {
		fields := strings.Split(line, ",")
		if len(fields) != 2 {
			t.Fatalf("unexpected status row for %s.%s: %q\nfull output:\n%s", db, table, line, out)
		}
		statuses[fields[0]] = fields[1]
	}
	return statuses
}

func requireMaintenanceStatuses(t *testing.T, got map[string]string, want map[string]string) {
	t.Helper()
	for id, wantStatus := range want {
		if got[id] != wantStatus {
			t.Fatalf("status[%s] = %q, want %q\nall statuses: %#v", id, got[id], wantStatus, got)
		}
	}
}

// TestReaperStaleIssueCloseSkipsDurableExtmsgRecordsRealDolt runs the real
// reaper.sh Step 5 stale-issue query against a real Dolt sql-server. Durable
// extmsg protocol records (group roots, participants, bindings, memberships,
// transcript state and entries) must survive the stale sweep while an ordinary
// stale task is still closed, and the query itself must not fail (#6380).
//
// A hyphenated rig database (my-rig) pins that the correlated label probe
// binds to each database's own labels table: decoy labels in the other
// database must not change which rows are stale candidates, and the query
// must not fail for rig databases either.
func TestReaperStaleIssueCloseSkipsDurableExtmsgRecordsRealDolt(t *testing.T) {
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skipf("dolt not found: %v", err)
	}

	durableLabels := map[string]string{
		"ext-group":            "gc:extmsg-group",
		"ext-participant":      "gc:extmsg-participant",
		"ext-binding":          "gc:extmsg-binding",
		"ext-membership":       "gc:extmsg-membership",
		"ext-transcript-state": "gc:extmsg-transcript-state",
		"ext-transcript":       "gc:extmsg-transcript",
	}

	var seed strings.Builder
	seed.WriteString("INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES\n")
	seed.WriteString("  ('ord-stale', 'ordinary stale task', 'open', 'task', 2, '2020-01-01 00:00:00', '2020-01-01 00:00:00', '', '{}')")
	for id := range durableLabels {
		fmt.Fprintf(&seed, ",\n  ('%s', 'durable extmsg record', 'open', 'task', 2, '2020-01-01 00:00:00', '2020-01-01 00:00:00', '', '{}')", id)
	}
	seed.WriteString(";\nINSERT INTO labels (issue_id, label) VALUES\n  ('ord-stale', 'unrelated-label')")
	for id, label := range durableLabels {
		fmt.Fprintf(&seed, ",\n  ('%s', '%s')", id, label)
	}
	// Decoy: a durable label for the rig's ordinary row lives only in citydb.
	seed.WriteString(",\n  ('rig-ord', 'gc:extmsg-group');\n")
	// A live task blocked by a wisp: its dependency row has a NULL
	// depends_on_issue_id. Before the NOT IN guard, that NULL turned the whole
	// active-dependency exclusion list to NULL and no stale issue was closed.
	seed.WriteString("INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES\n" +
		"  ('live-on-wisp', 'live task blocked by a wisp', 'open', 'task', 2, NOW(), NOW(), '', '{}');\n" +
		"INSERT INTO dependencies (issue_id, depends_on_issue_id, depends_on_wisp_id, depends_on_external, type) VALUES\n" +
		"  ('live-on-wisp', NULL, 'some-wisp', NULL, 'blocks');\n")

	// my-rig: rig-ext and rig-ext2 are durable via my-rig's own labels; rig-ord
	// is ordinary there. The decoy label for the city's ord-stale lives only in
	// my-rig. The asymmetric counts (1 ordinary vs 2 durable) make a probe
	// against the wrong database's labels table visible in the candidate count.
	rigSeed := `
INSERT INTO issues (id, title, status, issue_type, priority, created_at, updated_at, assignee, metadata) VALUES
  ('rig-ord', 'ordinary stale rig task', 'open', 'task', 2, '2020-01-01 00:00:00', '2020-01-01 00:00:00', '', '{}'),
  ('rig-ext', 'durable extmsg rig record', 'open', 'task', 2, '2020-01-01 00:00:00', '2020-01-01 00:00:00', '', '{}'),
  ('rig-ext2', 'durable extmsg rig record', 'open', 'task', 2, '2020-01-01 00:00:00', '2020-01-01 00:00:00', '', '{}');
INSERT INTO labels (issue_id, label) VALUES
  ('rig-ext', 'gc:extmsg-group'),
  ('rig-ext2', 'gc:extmsg-binding'),
  ('ord-stale', 'gc:extmsg-group');
`

	cityDir := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "dolt")
	for db, dbSeed := range map[string]string{"citydb": seed.String(), "my-rig": rigSeed} {
		dbDir := filepath.Join(dataDir, db)
		if err := os.MkdirAll(dbDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dbDir, err)
		}
		runDoltForMaintenanceTest(t, doltPath, dbDir, "init", "--name", "Gas City", "--email", "test@example.com")
		runDoltSQLForMaintenanceTest(t, doltPath, dbDir, maintenanceReaperSchemaSQL())
		runDoltSQLForMaintenanceTest(t, doltPath, dbDir, dbSeed)
		runDoltForMaintenanceTest(t, doltPath, dbDir, "add", ".")
		runDoltForMaintenanceTest(t, doltPath, dbDir, "commit", "-m", "seed stale extmsg records")
	}

	port := startDoltServerForMaintenanceTest(t, doltPath, dataDir)
	waitForDoltServerForMaintenanceTest(t, doltPath, port, "citydb")
	waitForDoltServerForMaintenanceTest(t, doltPath, port, "my-rig")
	writeCityBeadsMetadata(t, cityDir, "citydb")
	rigDir := filepath.Join(cityDir, "rigs", "my-rig")
	writeCityBeadsMetadata(t, rigDir, "my-rig")
	writeSiteRigBinding(t, cityDir, "my-rig", rigDir)

	binDir := t.TempDir()
	logDir := t.TempDir()
	bdLog := filepath.Join(logDir, "bd.log")
	escalateLog := filepath.Join(logDir, "escalate.log")
	if err := os.Symlink(doltPath, filepath.Join(binDir, "dolt")); err != nil {
		t.Fatalf("Symlink(dolt): %v", err)
	}
	writeRealDoltBdDouble(t, filepath.Join(binDir, "bd"))
	writeMaintenanceGCStub(t, filepath.Join(binDir, "gc"), `#!/bin/sh
case "$1 $2" in
  "session prune")
    printf '{"count":0}\n'
    ;;
esac
exit 0
`)
	escalateScript := filepath.Join(binDir, "escalate.sh")
	writeExecutable(t, escalateScript, `#!/bin/sh
printf '%s\n' "$*" >> "$ESCALATE_CALL_LOG"
exit 0
`)

	env := map[string]string{
		"BD_CALL_LOG":        bdLog,
		"ESCALATE_CALL_LOG":  escalateLog,
		"GC_ESCALATE_SCRIPT": escalateScript,
		"GC_CITY":            cityDir,
		"GC_CITY_PATH":       cityDir,
		"GC_DOLT_HOST":       "127.0.0.1",
		"GC_DOLT_PORT":       fmt.Sprintf("%d", port),
		"GC_DOLT_USER":       "root",
		"GC_DOLT_PASSWORD":   "",
		"FAKE_RIG_LIST_JSON": `{"rigs":[{"name":"my-rig","hq":false}]}`,
		"FAKE_SCOPE_DBS":     "city=citydb rig:my-rig=my-rig",
		"PATH":               binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
	reaperOut, err := runScriptResult(t, coreScriptPath("reaper.sh"), env)
	if err != nil {
		t.Fatalf("reaper.sh failed: %v\n%s", err, reaperOut)
	}

	escalateData, err := os.ReadFile(escalateLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadFile(escalate log): %v", err)
	}
	if strings.Contains(string(escalateData), "stale issue query failed") {
		t.Fatalf("reaper stale issue query failed on real Dolt:\n%s", escalateData)
	}
	// Rig rows are never closed by the city reaper; they are counted as skipped
	// candidates. Exactly one (rig-ord) must be a candidate: rig-ext/rig-ext2
	// are durable via my-rig's labels, and the citydb decoy must not hide
	// rig-ord. Probing citydb's labels instead would count 2; no filter, 3.
	if !strings.Contains(string(reaperOut), "skipped_non_city_issues:1,") {
		t.Fatalf("reaper did not report exactly one my-rig stale candidate:\n%s\nescalations:\n%s", reaperOut, escalateData)
	}

	bdData, err := os.ReadFile(bdLog)
	if err != nil {
		t.Fatalf("ReadFile(bd log): %v\nescalations:\n%s", err, escalateData)
	}
	if !strings.Contains(string(bdData), "close ord-stale --reason stale:auto-closed by reaper") {
		t.Fatalf("reaper did not close the ordinary stale issue:\nbd calls:\n%s\nescalations:\n%s", bdData, escalateData)
	}
	for id := range durableLabels {
		if strings.Contains(string(bdData), "close "+id+" ") {
			t.Fatalf("reaper closed durable extmsg record %s:\n%s", id, bdData)
		}
	}

	want := map[string]string{"ord-stale": "closed"}
	for id := range durableLabels {
		want[id] = "open"
	}
	requireMaintenanceStatuses(t, queryMaintenanceStatusByID(t, doltPath, port, "citydb", "issues"), want)
	requireMaintenanceStatuses(t, queryMaintenanceStatusByID(t, doltPath, port, "my-rig", "issues"), map[string]string{
		"rig-ord":  "open",
		"rig-ext":  "open",
		"rig-ext2": "open",
	})
}
