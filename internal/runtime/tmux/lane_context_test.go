package tmux

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestPreStartMaterializesLaneContextAndDiagnostic(t *testing.T) {
	for _, failFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("worktree_preparation_after_failure_%t", failFirst), func(t *testing.T) {
			lane, repo := t.TempDir(), t.TempDir()
			logPath := filepath.Join(lane, ".gc", "pre-start.log")
			cfg := runtime.Config{Env: map[string]string{
				"GC_DIR": lane, "GC_RIG_ROOT": repo, "GC_PRESTART_LOG": logPath,
			}}
			ops := &tmuxStartOps{tm: &Tmux{}}
			if failFirst {
				cfg.PreStart = []string{"printf prerequisite", "echo failed-prepare >&2; exit 3"}
				if err := runPreStart(context.Background(), ops, "session-1", cfg, 10*time.Second); err == nil || !strings.Contains(err.Error(), "failed-prepare") {
					t.Fatalf("preparation failure lost its output: %v", err)
				}
				if _, err := os.Stat(filepath.Join(lane, ".gc")); !os.IsNotExist(err) {
					t.Fatalf("failed preparation polluted the empty worktree destination: %v", err)
				}
			}
			cfg.PreStart = []string{
				`git init -q "$GC_RIG_ROOT" && git -C "$GC_RIG_ROOT" -c user.name=Test -c user.email=test@example.invalid commit --allow-empty -qm initial && printf prerequisite`,
				`git -C "$GC_RIG_ROOT" worktree add --detach "$GC_DIR" && printf prepared-worktree`,
			}
			if err := runPreStart(context.Background(), ops, "session-1", cfg, 10*time.Second); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(logPath)
			if err != nil || !strings.Contains(string(got), "prerequisite") || !strings.Contains(string(got), "prepared-worktree") {
				t.Fatalf("completed worktree preparation lost diagnostic output: %q, %v", got, err)
			}
		})
	}
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
	env["GC_RIG"], env["BEADS_DIR"] = "next-repo", "/next-repo/.beads"
	if err := runPreStart(context.Background(), ops, "session-1", cfg, time.Second); err != nil {
		t.Fatal(err)
	}
	contextFile, err = os.ReadFile(filepath.Join(lane, ".gc", "worker-env.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"export GC_RIG='next-repo'", "export BEADS_DIR='/next-repo/.beads'"} {
		if !strings.Contains(string(contextFile), want) {
			t.Errorf("reused lane context lacks %q: %s", want, contextFile)
		}
	}
	for _, stale := range []string{"export GC_RIG='repo'", "export BEADS_DIR='/repo/.beads'"} {
		if strings.Contains(string(contextFile), stale) {
			t.Errorf("reused lane context retains %q: %s", stale, contextFile)
		}
	}
	// The worker can restore the binding even when its tool drops launch env.
	cfg.PreStart = []string{`env -i PATH=/usr/bin:/bin /bin/sh -c '. "$1"; printf "%s\n" "$GC_CITY_PATH" "$GC_RIG_ROOT" "$GC_RIG" "$BEADS_DIR" "$GC_PRESTART_LOG" > "$2"' sh "$GC_DIR/.gc/worker-env.sh" "$GC_DIR/recovered"`}
	if err := runPreStart(context.Background(), ops, "session-1", cfg, time.Second); err != nil {
		t.Fatal(err)
	}
	recovered, err := os.ReadFile(filepath.Join(lane, "recovered"))
	if err != nil || string(recovered) != "/city\n/repo\nnext-repo\n/next-repo/.beads\n"+logPath+"\n" {
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
