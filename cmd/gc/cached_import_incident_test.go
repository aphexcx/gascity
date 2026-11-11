package main

// Regression fixture for locked-import drift with a warm validation memo.
// Every path is under t.TempDir(); the fixture refuses a cache outside it.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
	"github.com/gastownhall/gascity/internal/runtime"
)

type cachedImportFixture struct {
	t                *testing.T
	cityPath         string
	tomlPath         string
	cacheDirs        []string
	cfg              *config.City
	revision         string
	store            *beads.MemStore
	sp               *runtime.Fake
	stdout           *bytes.Buffer
	stderr           *bytes.Buffer
	builds           int
	cr               *CityRuntime
	dirty            *atomic.Bool
	beadID           string
	oldHash          string
	lastProviderName string
	prevPoolRunning  map[string]bool
	od               *recordingOrderDispatcher
}

// newCachedImportFixture boots two locked imports, one running session, and
// one accepted fingerprint with a warm cache-validation memo.
func newCachedImportFixture(t *testing.T) *cachedImportFixture {
	t.Helper()
	clearGCEnv(t)
	f := &cachedImportFixture{t: t, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, dirty: &atomic.Bool{}, lastProviderName: "tmux"}
	f.cityPath = t.TempDir()
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lock := &packman.Lockfile{Schema: packman.LockfileSchema, Packs: map[string]packman.LockedPack{}}
	var imports strings.Builder
	for _, name := range []string{"first", "second"} {
		repo := initImportWorktreePackRepo(t, name, "", fmt.Sprintf("[pack]\nname = %q\nschema = 2\nversion = \"1.0.0\"\n", name))
		write(filepath.Join(repo, "skills", "plan", "SKILL.md"), "---\nname: plan\ndescription: Plan work\n---\nPlan carefully.\n")
		write(filepath.Join(repo, "prompt.template.md"), "Accepted prompt for {{.AgentName}}.\n")
		write(filepath.Join(repo, "settings.json"), "{\"accepted\":true}\n")
		write(filepath.Join(repo, "overlay", "per-provider", "claude", "instructions.md"), "Accepted overlay.\n")
		mustGitImport(t, repo, "add", ".")
		mustGitImport(t, repo, "commit", "-m", "add skill")
		commit := strings.TrimSpace(gitOutputImport(t, repo, "rev-parse", "HEAD"))
		source := "https://example.com/" + name + ".git"
		cacheDir, err := packman.RepoCachePath(source, commit)
		if err != nil {
			t.Fatal(err)
		}
		if home, _ := os.UserHomeDir(); home != "" && strings.HasPrefix(cacheDir, filepath.Join(home, ".gc")) {
			t.Fatalf("fixture cache %s is under the live gc home; refusing", cacheDir)
		}
		if err := os.MkdirAll(filepath.Dir(cacheDir), 0o755); err != nil {
			t.Fatal(err)
		}
		mustGitImport(t, repo, "clone", repo, cacheDir)
		f.cacheDirs = append(f.cacheDirs, cacheDir)
		lock.Packs[source] = packman.LockedPack{Commit: commit}
		fmt.Fprintf(&imports, "\n[imports.%s]\nsource = %q\nversion = %q\n", name, source, "sha:"+commit)
	}
	if err := packman.WriteLockfile(fsys.OSFS{}, f.cityPath, lock); err != nil {
		t.Fatal(err)
	}
	f.tomlPath = filepath.Join(f.cityPath, "city.toml")
	write(f.tomlPath, "[workspace]\nname = \"test-city\"\nprovider = \"claude\"\n[beads]\nprovider = \"file\"\n[session]\nprovider = \"tmux\"\n[providers.claude]\ncommand = \"echo\"\nprompt_mode = \"none\"\n[[agent]]\nname = \"worker\"\nscope = \"city\"\n"+imports.String()+fmt.Sprintf("\n[[agent]]\nname = %q\nscope = \"city\"\n", config.ControlDispatcherAgentName))
	f.cfg, f.revision = loadCityRuntimeControllerConfig(t, f.cityPath)
	f.store = beads.NewMemStore()
	f.sp = runtime.NewFake()
	build := func(c *config.City, sp runtime.Provider, _ beads.Store) DesiredStateResult {
		f.builds++
		bp := newAgentBuildParams("test-city", f.cityPath, c, sp, time.Time{}, nil, f.stderr)
		tp, err := resolveTemplate(bp, &c.Agents[0], "worker", nil)
		if err != nil {
			t.Fatal(err)
		}
		tp.SessionName = "worker"
		return DesiredStateResult{State: map[string]TemplateParams{"worker": tp}}
	}
	initial := build(f.cfg, f.sp, f.store).State["worker"]
	if initial.FPExtra["skills:first.plan"] == "" || initial.FPExtra["skills:second.plan"] == "" {
		t.Fatalf("missing import fingerprints: %v", initial.FPExtra)
	}
	sb, err := f.store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
		"session_name": "worker", "template": "worker", "generation": "1", "state": "active", "pin_awake": "true", "started_config_hash": "legacy",
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.beadID = sb.ID
	if err := f.sp.Start(context.Background(), "worker", runtime.Config{Command: initial.Command}); err != nil {
		t.Fatal(err)
	}
	f.cr = newTestCityRuntime(t, CityRuntimeParams{
		CityPath: f.cityPath, CityName: "test-city", TomlPath: f.tomlPath, ConfigRev: f.revision,
		ConfigDirty: f.dirty, Cfg: f.cfg, SP: f.sp, BuildFn: build, Dops: newDrainOps(f.sp), Rec: events.Discard, Stdout: f.stdout, Stderr: f.stderr,
	})
	f.cr.od = nil
	cs := newControllerState(context.Background(), f.cfg, f.sp, events.NewFake(), "test-city", f.cityPath)
	cs.cityBeadStore = f.store
	f.cr.setControllerState(cs)
	f.cr.sessionDrains = newDrainTracker()
	f.tick("watch")
	f.oldHash = f.storedHash()
	if f.oldHash == "" || f.draining() {
		t.Fatalf("clean baseline missing fingerprint or draining: %s / %s", f.stdout.String(), f.stderr.String())
	}
	f.stdout.Reset()
	f.stderr.Reset()
	f.od = &recordingOrderDispatcher{}
	f.cr.od = f.od
	return f
}

func (f *cachedImportFixture) tick(trigger string) {
	f.t.Helper()
	f.cr.tick(context.Background(), f.dirty, &f.lastProviderName, f.cityPath, &f.prevPoolRunning, trigger)
}

func (f *cachedImportFixture) storedHash() string {
	f.t.Helper()
	b, err := f.store.Get(f.beadID)
	if err != nil {
		f.t.Fatal(err)
	}
	return b.Metadata["started_config_hash"]
}

func (f *cachedImportFixture) draining() bool {
	return f.cr.sessionDrains.get(f.beadID) != nil
}

func TestCityRuntimeIncidentReplayWarmValidationMemo(t *testing.T) {
	for _, tc := range []struct {
		name                string
		symlink, watchEvent bool
	}{
		{"untracked_file_no_watch_event", false, false},
		{"untracked_file_with_watch_event", false, true},
		{"dangling_symlink_no_watch_event", true, false},
		{"dangling_symlink_with_watch_event", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCachedImportFixture(t)
			stray := filepath.Join(f.cacheDirs[1], "skills", "plan", "stray")
			if tc.symlink {
				if err := os.Symlink(filepath.Join(f.cityPath, "does-not-exist"), stray); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(stray, []byte("untracked\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			// No git command or cache-root touch: both would invalidate the memo.
			check := func(stage string) {
				t.Helper()
				f.dirty.Store(tc.watchEvent)
				f.tick("patrol")
				if f.draining() || strings.Contains(f.stdout.String(), "config-drift") {
					t.Errorf("%s: session drained: %s", stage, f.stdout.String())
				}
				if got := f.storedHash(); got != f.oldHash {
					t.Errorf("%s: stored fingerprint = %s, want %s", stage, got, f.oldHash)
				}
			}
			check("stray present")
			check("stray still present")
			// A cold validator rejects the same dirty cache, after the tested ticks.
			config.ResetRemoteCacheValidationCache()
			_, err := tryReloadConfig(f.tomlPath, "test-city", f.cityPath)
			if err == nil || !strings.Contains(err.Error(), "local worktree changes") {
				t.Fatalf("cold validator accepted dirty cache: %v", err)
			}
			if err := os.Remove(stray); err != nil {
				t.Fatal(err)
			}
			check("stray removed")
		})
	}
}

func TestCityRuntimeRejectedReloadKeepsWorking(t *testing.T) {
	for _, cause := range []string{"toml_typo", "storage_change"} {
		t.Run(cause, func(t *testing.T) {
			f := newCachedImportFixture(t)
			data, err := os.ReadFile(f.tomlPath)
			if err != nil {
				t.Fatal(err)
			}
			if cause == "toml_typo" {
				data = []byte("[[[ bad toml")
			} else {
				data = append(data, []byte(fmt.Sprintf("\n[storage.classes]\nwork = \"work\"\ngraph = \"infra\"\nsessions = \"infra\"\nmessaging = \"infra\"\norders = \"infra\"\nnudges = \"infra\"\n[storage.bindings.infra]\nprovider = %q\npath = %q\n", config.StorageProviderSQLiteBeads, filepath.Join(t.TempDir(), "store")))...)
			}
			if err := os.WriteFile(f.tomlPath, data, 0o644); err != nil {
				t.Fatal(err)
			}
			f.dirty.Store(true)
			f.tick("watch")
			if f.cr.configLoadHealth.snapshot() == nil {
				t.Fatal("reload was not rejected")
			}
			if cause == "storage_change" && !strings.Contains(f.cr.configLoadHealth.snapshot().Error, "[storage] changed") {
				t.Fatalf("storage reload failed for the wrong reason: %+v", f.cr.configLoadHealth.snapshot())
			}
			if f.draining() || f.storedHash() != f.oldHash {
				t.Fatalf("rejected config changed the running session: %s", f.stdout.String())
			}
			if err := f.sp.Stop("worker"); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				f.tick("patrol")
			}
			if !f.sp.IsRunning("worker") {
				t.Errorf("stopped session was not restarted: %s", f.stderr.String())
			}
			if f.od.calls.Load() == 0 {
				t.Error("orders did not run on retained config")
			}
		})
	}
}

func TestCityRuntimeRejectedReloadRetainsSkillMembership(t *testing.T) {
	for _, change := range []string{"delete", "add"} {
		t.Run(change, func(t *testing.T) {
			f := newCachedImportFixture(t)
			if change == "delete" {
				if err := os.Remove(filepath.Join(f.cacheDirs[0], "skills", "plan", "SKILL.md")); err != nil {
					t.Fatal(err)
				}
			} else {
				dir := filepath.Join(f.cacheDirs[0], "skills", "extra")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("Unexpected skill"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			config.ResetRemoteCacheValidationCache()
			f.dirty.Store(true)
			f.tick("watch")
			if failure := f.cr.configLoadHealth.snapshot(); failure == nil || !strings.Contains(failure.Error, "local worktree changes") {
				t.Fatalf("expected dirty-import rejection: %+v", failure)
			}
			if f.draining() || f.storedHash() != f.oldHash {
				t.Fatalf("rejected skill membership changed the session: %s", f.stdout.String())
			}
		})
	}
}

func TestCityRuntimeRejectedReloadRetainsOrderDispatcher(t *testing.T) {
	f := newCachedImportFixture(t)
	f.cr.orderRescanEnabled = true
	f.cr.orderRescanLast = time.Time{}
	orderDir := filepath.Join(f.cacheDirs[0], "orders")
	if err := os.MkdirAll(orderDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orderDir, "unexpected.toml"), []byte("[order]\nexec = \"true\"\ntrigger = \"manual\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config.ResetRemoteCacheValidationCache()
	f.dirty.Store(true)
	f.tick("watch")
	if f.cr.configLoadHealth.snapshot() == nil {
		t.Fatal("expected dirty-import rejection")
	}
	if f.cr.od != f.od || f.od.calls.Load() == 0 {
		t.Fatalf("rejected sources replaced the accepted dispatcher or stopped dispatch: %s", f.stderr.String())
	}
	if !f.cr.orderRescanLast.IsZero() {
		t.Fatal("order rescan adopted rejected sources")
	}
	f.cr.configLoadHealth.clear()
	f.cr.rescanOrderDispatcherIfDue(context.Background(), f.cityPath, time.Now())
	if f.cr.orderRescanLast.IsZero() {
		t.Fatal("order rescan did not resume after reload recovery")
	}
}
