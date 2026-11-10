package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
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

// A rejected import must not supply fresh filesystem content to session hashes.
func TestCityRuntimeDirtyImportPreservesSessionFingerprint(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
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
	var cacheDirs []string
	for _, name := range []string{"first", "second"} {
		repo := initImportWorktreePackRepo(t, name, "", fmt.Sprintf("[pack]\nname = %q\nschema = 2\nversion = \"1.0.0\"\n", name))
		write(filepath.Join(repo, "skills", "plan", "SKILL.md"), "---\nname: plan\ndescription: Plan work\n---\nPlan carefully.\n")
		mustGitImport(t, repo, "add", "skills")
		mustGitImport(t, repo, "commit", "-m", "add skill")
		commit := strings.TrimSpace(gitOutputImport(t, repo, "rev-parse", "HEAD"))
		source := "https://example.com/" + name + ".git"
		cacheDir, err := packman.RepoCachePath(source, commit)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(cacheDir), 0o755); err != nil {
			t.Fatal(err)
		}
		mustGitImport(t, repo, "clone", repo, cacheDir)
		cacheDirs = append(cacheDirs, cacheDir)
		lock.Packs[source] = packman.LockedPack{Commit: commit}
		fmt.Fprintf(&imports, "\n[imports.%s]\nsource = %q\nversion = %q\n", name, source, "sha:"+commit)
	}
	if err := packman.WriteLockfile(fsys.OSFS{}, cityPath, lock); err != nil {
		t.Fatal(err)
	}
	tomlPath := filepath.Join(cityPath, "city.toml")
	write(tomlPath, "[workspace]\nname = \"test-city\"\nprovider = \"claude\"\n[beads]\nprovider = \"file\"\n[session]\nprovider = \"tmux\"\n[providers.claude]\ncommand = \"echo\"\nprompt_mode = \"none\"\n[[agent]]\nname = \"worker\"\nscope = \"city\"\n"+imports.String())
	cfg, revision := loadCityRuntimeControllerConfig(t, cityPath)
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	var stdout, stderr bytes.Buffer
	builds := 0
	build := func(c *config.City, sp runtime.Provider, _ beads.Store) DesiredStateResult {
		builds++
		bp := newAgentBuildParams("test-city", cityPath, c, sp, time.Time{}, nil, &stderr)
		tp, err := resolveTemplate(bp, &c.Agents[0], "worker", nil)
		if err != nil {
			t.Fatal(err)
		}
		tp.SessionName = "worker"
		return DesiredStateResult{State: map[string]TemplateParams{"worker": tp}}
	}
	initial := build(cfg, sp, store).State["worker"]
	if initial.FPExtra["skills:first.plan"] == "" || initial.FPExtra["skills:second.plan"] == "" {
		t.Fatalf("missing import fingerprints: %v", initial.FPExtra)
	}
	sb, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
		"session_name": "worker", "template": "worker", "generation": "1", "state": "active", "pin_awake": "true", "started_config_hash": "legacy",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: initial.Command}); err != nil {
		t.Fatal(err)
	}
	dirty := &atomic.Bool{}
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath, CityName: "test-city", TomlPath: tomlPath, ConfigRev: revision,
		ConfigDirty: dirty, Cfg: cfg, SP: sp, BuildFn: build, Dops: newDrainOps(sp), Rec: events.Discard, Stdout: &stdout, Stderr: &stderr,
	})
	cr.od = nil
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = store
	cr.setControllerState(cs)
	cr.sessionDrains = newDrainTracker()
	lastProviderName := "tmux"
	var prevPoolRunning map[string]bool
	cr.tick(context.Background(), dirty, &lastProviderName, cityPath, &prevPoolRunning, "watch")
	sb, err = store.Get(sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldHash := sb.Metadata["started_config_hash"]
	if oldHash == "" || cr.sessionDrains.get(sb.ID) != nil {
		t.Fatalf("clean baseline missing fingerprint or draining: %s / %s", stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	cleanBuilds := builds
	od := &recordingOrderDispatcher{}
	cr.od = od
	tick := func() {
		t.Helper()
		cr.tick(context.Background(), dirty, &lastProviderName, cityPath, &prevPoolRunning, "watch")
		updated, err := store.Get(sb.ID)
		if err != nil {
			t.Fatal(err)
		}
		if updated.Metadata["started_config_hash"] != oldHash {
			t.Errorf("stored fingerprint changed: %q", updated.Metadata["started_config_hash"])
		}
		if cr.sessionDrains.get(sb.ID) != nil || strings.Contains(stdout.String(), "config-drift") {
			t.Errorf("session drained after import load failure: %s", stdout.String())
		}
	}
	stray := filepath.Join(cacheDirs[1], "skills", "plan", "stray.txt")
	write(stray, "untracked content must never enter a fingerprint\n")
	// Invalidate the cache validator's root/index stat memo, as a cache update
	// does, so this test exercises the rejected-load boundary deterministically.
	stamp := time.Now().Add(time.Second)
	if err := os.Chtimes(cacheDirs[1], stamp, stamp); err != nil {
		t.Fatal(err)
	}
	dirty.Store(true)
	tick()
	failure := cr.configLoadHealth.snapshot()
	if failure == nil || failure.Since.IsZero() || !strings.Contains(failure.Error, `city import "second"`) {
		t.Fatalf("missing actionable failure: %+v", failure)
	}
	replies := make(chan reloadControlReply, 1)
	cr.activeReload = &reloadRequest{soft: true, doneCh: replies}
	tick()
	select {
	case reply := <-replies:
		if reply.Outcome != reloadOutcomeFailed || cr.activeReload != nil {
			t.Fatalf("blocked soft reload was not completed as a failure: %+v", reply)
		}
	default:
		t.Fatal("blocked soft reload did not reply")
	}
	if builds != cleanBuilds || od.calls.Load() != 0 {
		t.Fatalf("rejected config reached build/order dispatch: builds=%d, orders=%d", builds-cleanBuilds, od.calls.Load())
	}
	if cr.cfg != cfg || cr.configRev != revision {
		t.Error("rejected import replaced the resolved config")
	}
	if got := strings.Count(stderr.String(), "local worktree changes"); got != 1 {
		t.Errorf("logged import failure %d times, want once: %s", got, stderr.String())
	}
	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}
	tick()
	if cr.configLoadHealth.snapshot() != nil {
		t.Fatal("repaired import did not clear failure without another watch event")
	}

	// A clean, relocked skill change must still reach ordinary drift handling.
	const source = "https://example.com/second.git"
	previousCommit := lock.Packs[source].Commit
	write(filepath.Join(cacheDirs[1], "skills", "plan", "SKILL.md"), "---\nname: plan\ndescription: Plan work\n---\nRevised plan.\n")
	mustGitImport(t, cacheDirs[1], "add", "skills")
	mustGitImport(t, cacheDirs[1], "commit", "-m", "revise skill")
	commit := strings.TrimSpace(gitOutputImport(t, cacheDirs[1], "rev-parse", "HEAD"))
	newCache, err := packman.RepoCachePath(source, commit)
	if err != nil {
		t.Fatal(err)
	}
	mustGitImport(t, cacheDirs[1], "clone", cacheDirs[1], newCache)
	lock.Packs[source] = packman.LockedPack{Commit: commit}
	if err := packman.WriteLockfile(fsys.OSFS{}, cityPath, lock); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	write(tomlPath, strings.ReplaceAll(string(data), previousCommit, commit))
	oldLifecycle := cityRuntimeStartBeadsLifecycle
	t.Cleanup(func() { cityRuntimeStartBeadsLifecycle = oldLifecycle })
	cityRuntimeStartBeadsLifecycle = func(string, string, *config.City, io.Writer) error {
		return fmt.Errorf("fixture lifecycle refused")
	}
	dirty.Store(true)
	tick()
	if failure := cr.configLoadHealth.snapshot(); failure == nil || !strings.Contains(failure.Error, "fixture lifecycle refused") {
		t.Fatalf("failed application cleared or hid the hold: %+v", failure)
	}
	cityRuntimeStartBeadsLifecycle = oldLifecycle
	cr.tick(context.Background(), dirty, &lastProviderName, cityPath, &prevPoolRunning, "watch")
	if !strings.Contains(stdout.String(), "config-drift") || cr.configLoadHealth.snapshot() != nil {
		t.Fatalf("clean skill change failed to drift: stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
}
