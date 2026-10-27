package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestPreStartMaterializesLaneContextAndDiagnostic(t *testing.T) {
	lane := t.TempDir()
	logPath := filepath.Join(lane, ".gc", "pre-start.log")
	env := map[string]string{
		"GC_DIR": lane, "GC_CITY_PATH": "/city", "GC_RIG_ROOT": "/repo", "GC_RIG": "repo",
		"BEADS_DIR": "/repo/.beads", "GC_PRESTART_LOG": logPath,
		"GC_SESSION_ID": "session-1", "EXAMPLE_TOKEN": "must-not-persist",
	}
	ops := &tmuxStartOps{tm: &Tmux{}}
	cfg := runtime.Config{Env: env, PreStart: []string{`printf 'prepared %s\n' "$EXAMPLE_TOKEN"`}}
	if err := runPreStart(context.Background(), ops, "session-1", cfg, time.Second); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(before), "prepared") || strings.Contains(string(before), "must-not-persist") {
		t.Fatalf("diagnostic must retain successful output with secrets redacted: %s", before)
	}
	contextFile, err := os.ReadFile(filepath.Join(lane, ".gc", "worker-env.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"GC_CITY_PATH", "GC_RIG_ROOT", "GC_RIG", "BEADS_DIR", "GC_PRESTART_LOG"} {
		if !strings.Contains(string(contextFile), "export "+key+"=") {
			t.Errorf("missing %s from sourceable context: %s", key, contextFile)
		}
	}
	if strings.Contains(string(contextFile), "EXAMPLE_TOKEN") || strings.Contains(string(contextFile), "must-not-persist") {
		t.Fatal("context file copied a non-path variable")
	}
	// The worker can restore the binding even when its tool drops launch env.
	cmd := exec.Command("/bin/sh", "-c", `. "$1"; printf '%s\n' "$GC_CITY_PATH" "$GC_RIG_ROOT" "$GC_RIG" "$BEADS_DIR" "$GC_PRESTART_LOG"`, "sh", filepath.Join(lane, ".gc", "worker-env.sh"))
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	recovered, err := cmd.CombinedOutput()
	if err != nil || string(recovered) != "/city\n/repo\nrepo\n/repo/.beads\n"+logPath+"\n" {
		t.Fatalf("filtered shell failed to restore context: %q, %v", recovered, err)
	}
	cfg.PreStart = []string{"echo second >&2; exit 3"}
	if err := runPreStart(context.Background(), ops, "session-1", cfg, time.Second); err == nil {
		t.Fatal("failure must still abort startup")
	}
	after, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(after), string(before)) || !strings.Contains(string(after), "second") {
		t.Fatalf("diagnostics must append failure output without replacing previous records: %s", after)
	}
}
