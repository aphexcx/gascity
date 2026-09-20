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
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/bin/sh
set -e
printf '%s\n' "$*" >> "$BD_CALL_LOG"
case "$1" in
  prune)
    printf '{"pruned_count":0}\n'
    ;;
  close)
    issue_id="$2"
    DOLT_CLI_PASSWORD="${GC_DOLT_PASSWORD:-}" dolt --host "$GC_DOLT_HOST" --port "$GC_DOLT_PORT" --user "$GC_DOLT_USER" --no-tls --use-db citydb sql \
      -q "UPDATE issues SET status='closed', closed_at=NOW() WHERE id='${issue_id}'; CALL DOLT_COMMIT('-Am', 'test bd close')"
    ;;
esac
exit 0
`)
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
	if !strings.Contains(string(bdData), "close issue-close --reason stale inactive workflow root auto-closed by reaper") {
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
