package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	bdpack "github.com/gastownhall/gascity/examples/bd"
)

// Exercise the real shell startup branch with only its discovery/readiness
// collaborators stubbed. The child reports OS facts, so nohup alone cannot
// satisfy the detachment contract and no real database is started.
func TestDoltShellStartDetachesFromSession(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 required for the process observation fixture")
	}
	source, err := bdpack.PackFS.ReadFile("assets/scripts/gc-beads-bd.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "dolt"), "#!"+python+`
import json, os, sys
try:
    os.fstat(9)
    lock_open = True
except OSError:
    lock_open = False
print(json.dumps(dict(pid=os.getpid(), pgid=os.getpgrp(), sid=os.getsid(0),
    lock_open=lock_open, stdin_null=os.fstat(0).st_rdev == os.stat(os.devnull).st_rdev,
    argv=sys.argv[1:], env=dict(os.environ))))
`)
	writeExecutable(t, filepath.Join(dir, "flock"), "#!/bin/sh\nexit 0\n")
	harness := extractShellFunction(t, string(source), "op_start") + `
is_remote() { return 1; }
resolve_gc_helper_bin() { :; }
ensure_dolt_identity() { :; }
load_existing_managed_from_gc() { GC_EXISTING_MANAGED_PID=; GC_EXISTING_REUSABLE=false; }
load_probe_managed_from_gc() { GC_PROBE_PORT_HOLDER_PID=; GC_PROBE_RUNNING=false; }
load_start_managed_from_gc() { GC_START_MANAGED_USED=false; return 1; }
run_preflight_cleanup() { :; }
wait_dolt_data_lock_free() { return 0; }
write_config_yaml() { :; }
save_state() { :; }
load_wait_ready_from_gc() { GC_WAIT_READY=true; }
die() { echo "$*" >&2; exit 1; }
op_start
server_pid=$(cat "$PID_FILE")
wait "$server_pid" || exit $?
printf '%s\n' "$server_pid"
cat "$LOG_FILE"
`
	env := []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"DATA_DIR=" + dir, "LOCK_FILE=" + filepath.Join(dir, "start.lock"),
		"CONFIG_FILE=" + filepath.Join(dir, "config with spaces.yaml"),
		"LOG_FILE=" + filepath.Join(dir, "dolt.log"), "PID_FILE=" + filepath.Join(dir, "dolt.pid"),
		"GC_CITY_PATH=" + dir, "GC_DOLT_PORT=47823", "DOLT_PORT=47823", "DOLT_LOGLEVEL=debug",
	}
	for _, key := range managedDoltSessionScopedEnvKeys {
		env = append(env, key+"=caller")
	}
	out, stderr, err := runGCBeadsBdCommand(t, env, "sh", "-c", harness)
	if err != nil {
		t.Fatalf("shell startup: %v\n%s\n%s", err, out, stderr)
	}
	pidLine, report, ok := strings.Cut(out, "\n")
	if !ok {
		t.Fatalf("missing child report: %s", out)
	}
	var child struct {
		PID, PGID, SID int
		LockOpen       bool `json:"lock_open"`
		StdinNull      bool `json:"stdin_null"`
		Argv           []string
		Env            map[string]string
	}
	if err := json.Unmarshal([]byte(report), &child); err != nil {
		t.Fatalf("decode child report: %v\n%s", err, out)
	}
	if pidLine != strconv.Itoa(child.PID) || child.PID != child.PGID || child.PID != child.SID {
		t.Errorf("server must retain published PID and own session/group: published=%s pid=%d pgid=%d sid=%d", pidLine, child.PID, child.PGID, child.SID)
	}
	if child.LockOpen || !child.StdinNull {
		t.Errorf("server inherited caller descriptors: lock=%v stdin_null=%v", child.LockOpen, child.StdinNull)
	}
	for _, key := range managedDoltSessionScopedEnvKeys {
		if _, present := child.Env[key]; present {
			t.Errorf("server inherited session key %s", key)
		}
	}
	if child.Env["GC_CITY_PATH"] != dir || child.Env["GC_DOLT_PORT"] != "47823" || child.Env["DOLT_LOGLEVEL"] != "debug" {
		t.Errorf("lost city/server settings: %v", child.Env)
	}
	if want := "sql-server|--config|" + filepath.Join(dir, "config with spaces.yaml"); strings.Join(child.Argv, "|") != want {
		t.Errorf("server argv = %q, want %q", child.Argv, want)
	}
}
