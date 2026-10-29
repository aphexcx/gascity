package dolt_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartReachesProviderWithoutLiveRuntime(t *testing.T) {
	for _, tc := range []struct {
		state string
		exit  int
	}{
		{"missing", 0},
		{"stopped", 0},
		{"dead-pid", 0},
		{"provider-failure", 7},
		{"remote-noop", 2},
		{"proxied", 0},
	} {
		t.Run(tc.state, func(t *testing.T) {
			city := t.TempDir()
			logPath := writeFakeBeadsBDForRestart(t, city, map[string]int{"start": tc.exit})
			stateDir := filepath.Join(city, ".gc", "runtime", "packs", "dolt")
			if err := os.MkdirAll(stateDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.state == "stopped" || tc.state == "dead-pid" {
				// An impossible PID leaves the running:true publication stale.
				data := fmt.Sprintf(`{"running":%t,"pid":2147483647,"port":47823,"data_dir":%q}`, tc.state == "dead-pid", filepath.Join(stateDir, "dolt-data"))
				if err := os.WriteFile(filepath.Join(stateDir, "dolt-state.json"), []byte(data), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.state == "proxied" {
				writeProxiedScope(t, city)
			}
			out, err := runDoltLifecycleWithEnv(t, city, repoRoot(t), "commands/start/run.sh", nil)
			code := 0
			if err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("start: %v\n%s", err, out)
				}
				code = exitErr.ExitCode()
			}
			if code != tc.exit {
				t.Fatalf("start with %s runtime state: exit %d, want %d\n%s", tc.state, code, tc.exit, out)
			}
			ops, err := os.ReadFile(logPath)
			if tc.state == "proxied" {
				if !os.IsNotExist(err) || !strings.Contains(string(out), proxiedNoOpMessage) {
					t.Fatalf("bd-owned scope reached provider or lost no-op: ops=%q, err=%v, out=%s", ops, err, out)
				}
				return
			}
			if err != nil || strings.TrimSpace(string(ops)) != "start" {
				t.Fatalf("provider operations = %q, %v; want start", ops, err)
			}
		})
	}
}
