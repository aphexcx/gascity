package herdr

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

// newTestProvider builds a Provider suitable for pre_start tests. runPreStart
// never touches the herdr client, so no herdr binary or server is required.
func newTestProvider(t *testing.T, setupTimeout time.Duration) *Provider {
	t.Helper()
	return New("gctest-prestart", t.TempDir(), t.TempDir(), setupTimeout, 0)
}

func TestRunPreStartNoCommandsIsNoOp(t *testing.T) {
	p := newTestProvider(t, time.Second)
	if err := p.runPreStart(context.Background(), runtime.Config{}); err != nil {
		t.Fatalf("runPreStart with no commands = %v, want nil", err)
	}
}

func TestRunPreStartMaterializesLaneDiagnostics(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("worktree_destination_missing_%t", missing), func(t *testing.T) {
			lane, repo := t.TempDir(), t.TempDir()
			if missing {
				lane = filepath.Join(lane, "new-lane")
			}
			logPath := filepath.Join(lane, ".gc", "pre-start.log")
			p := newTestProvider(t, 10*time.Second)
			cfg := runtime.Config{Env: map[string]string{
				"GC_DIR": lane, "GC_RIG_ROOT": repo, "GC_PRESTART_LOG": logPath,
			}, PreStart: []string{"printf prerequisite", "echo failed-prepare >&2; exit 3"}}
			if err := p.runPreStart(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "failed-prepare") {
				t.Fatalf("preparation failure lost its output: %v", err)
			}
			if _, err := os.Stat(filepath.Join(lane, ".gc")); !os.IsNotExist(err) {
				t.Fatalf("failed preparation polluted the worktree destination: %v", err)
			}
			cfg.PreStart = []string{
				`git init -q "$GC_RIG_ROOT" && git -C "$GC_RIG_ROOT" -c user.name=Test -c user.email=test@example.invalid commit --allow-empty -qm initial && printf prerequisite`,
				`git -C "$GC_RIG_ROOT" worktree add --detach "$GC_DIR" && printf prepared-worktree`,
			}
			if err := p.runPreStart(context.Background(), cfg); err != nil {
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
	p := newTestProvider(t, time.Second)
	cfg := runtime.Config{
		Env:      map[string]string{"GC_DIR": lane, "GC_PRESTART_LOG": logPath},
		PreStart: []string{"printf prepared"},
	}
	if err := p.runPreStart(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(got), "prepared") {
		t.Fatalf("pre-start diagnostic = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(lane, ".gc", "worker-env.sh")); err != nil {
		t.Fatalf("lane environment was not materialized: %v", err)
	}
}

// pre_start carries stage-2 skill/MCP materialization, so the commands must
// actually run — and in order.
func TestRunPreStartRunsCommandsInOrder(t *testing.T) {
	dir := t.TempDir()
	p := newTestProvider(t, 10*time.Second)
	cfg := runtime.Config{
		PreStart: []string{
			"printf one >> " + filepath.Join(dir, "order.txt"),
			"printf two >> " + filepath.Join(dir, "order.txt"),
		},
	}
	if err := p.runPreStart(context.Background(), cfg); err != nil {
		t.Fatalf("runPreStart: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "order.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "onetwo" {
		t.Errorf("order = %q, want %q", got, "onetwo")
	}
}

// cwd comes from GC_DIR (not cfg.WorkDir), mirroring tmux's runSetupCommand.
func TestRunPreStartUsesGCDirAsCwd(t *testing.T) {
	dir := t.TempDir()
	p := newTestProvider(t, 10*time.Second)
	cfg := runtime.Config{
		Env:      map[string]string{"GC_DIR": dir},
		PreStart: []string{"pwd > pwd.txt"},
	}
	if err := p.runPreStart(context.Background(), cfg); err != nil {
		t.Fatalf("runPreStart: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "pwd.txt"))
	if err != nil {
		t.Fatalf("pre_start did not run in GC_DIR: %v", err)
	}
	// macOS resolves TempDir through /private; compare suffix.
	if !strings.HasSuffix(strings.TrimSpace(string(got)), strings.TrimPrefix(dir, "/private")) {
		t.Errorf("cwd = %q, want it to be GC_DIR %q", strings.TrimSpace(string(got)), dir)
	}
}

// cfg.Env must reach the command (materialization commands read GC_* vars).
func TestRunPreStartPassesEnv(t *testing.T) {
	dir := t.TempDir()
	p := newTestProvider(t, 10*time.Second)
	cfg := runtime.Config{
		Env:      map[string]string{"GC_DIR": dir, "GC_TEST_TOKEN": "sentinel"},
		PreStart: []string{"printf %s \"$GC_TEST_TOKEN\" > token.txt"},
	}
	if err := p.runPreStart(context.Background(), cfg); err != nil {
		t.Fatalf("runPreStart: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "token.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "sentinel" {
		t.Errorf("GC_TEST_TOKEN = %q, want %q", got, "sentinel")
	}
}

// Failures are fatal — an agent must never launch into an unprepared workDir.
// The error identifies which command failed and carries its output tail.
func TestRunPreStartFailureIsFatal(t *testing.T) {
	p := newTestProvider(t, 10*time.Second)
	cfg := runtime.Config{
		PreStart: []string{"true", "echo boom >&2; exit 3", "true"},
	}
	err := p.runPreStart(context.Background(), cfg)
	if err == nil {
		t.Fatal("runPreStart = nil, want error (failure must abort startup)")
	}
	if !strings.Contains(err.Error(), "pre_start[1]") {
		t.Errorf("error %q, want it to name the failing index pre_start[1]", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q, want it to include the command output tail", err)
	}
}

// A hung pre_start must not hang the start forever; setupTimeout bounds it.
func TestRunPreStartRespectsSetupTimeout(t *testing.T) {
	p := newTestProvider(t, 150*time.Millisecond)
	cfg := runtime.Config{PreStart: []string{"sleep 5"}}
	start := time.Now()
	err := p.runPreStart(context.Background(), cfg)
	if err == nil {
		t.Fatal("runPreStart = nil, want timeout error")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("took %v, want it bounded by setupTimeout", elapsed)
	}
}

// A non-positive setupTimeout falls back to the default rather than making
// every pre_start fail instantly with an already-expired context.
func TestNewDefaultsSetupTimeout(t *testing.T) {
	p := New("gctest-default", t.TempDir(), t.TempDir(), 0, 0)
	if p.setupTimeout != defaultSetupTimeout {
		t.Errorf("setupTimeout = %v, want default %v", p.setupTimeout, defaultSetupTimeout)
	}
	if err := p.runPreStart(context.Background(), runtime.Config{PreStart: []string{"true"}}); err != nil {
		t.Errorf("runPreStart with defaulted timeout = %v, want nil", err)
	}
}

// A pre_start whose GC_DIR doesn't exist yet must not fail on chdir — the
// worktree is often created concurrently with (or by) pre_start itself. The
// command runs with cwd falling back to the city root instead.
func TestRunPreStartToleratesMissingGCDir(t *testing.T) {
	cityRoot := t.TempDir()
	p := New("gctest-prestart-missing", t.TempDir(), cityRoot, 10*time.Second, 0)
	cfg := runtime.Config{
		Env:      map[string]string{"GC_DIR": filepath.Join(cityRoot, "does", "not", "exist")},
		PreStart: []string{"pwd > cwd.txt"},
	}
	if err := p.runPreStart(context.Background(), cfg); err != nil {
		t.Fatalf("runPreStart with missing GC_DIR = %v, want nil (cwd fallback)", err)
	}
	got, err := os.ReadFile(filepath.Join(cityRoot, "cwd.txt"))
	if err != nil {
		t.Fatalf("expected fallback cwd = cityRoot: %v", err)
	}
	if !strings.HasSuffix(strings.TrimSpace(string(got)), strings.TrimPrefix(cityRoot, "/private")) {
		t.Errorf("cwd = %q, want cityRoot %q", strings.TrimSpace(string(got)), cityRoot)
	}
}
