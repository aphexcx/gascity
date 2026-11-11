package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// These tests start at the persisted delivery boundary. The real dirty-start
// and materialization journey is owned by TestCityRuntimeStartupContentRecovery.
type startupRepairFixture struct {
	f       *cachedImportFixture
	clk     *clock.Fake
	drains  *drainTracker
	desired map[string]TemplateParams
	names   map[string]bool
}

func newStartupRepairFixture(t *testing.T) *startupRepairFixture {
	t.Helper()
	f := newCachedImportFixture(t)
	f.cfg.NamedSessions = []config.NamedSession{{Name: "worker", Template: "worker", Mode: "always"}}
	if err := f.store.SetMetadataBatch(f.beadID, map[string]string{
		"configured_named_session": "true", "configured_named_identity": "worker", "pin_awake": "",
	}); err != nil {
		t.Fatal(err)
	}
	return &startupRepairFixture{
		f: f, clk: &clock.Fake{Time: time.Now()}, drains: newDrainTracker(),
		desired: f.cr.buildFn(f.cfg, f.sp, f.store).State, names: map[string]bool{"worker": true},
	}
}

func (h *startupRepairFixture) tick(t *testing.T) {
	t.Helper()
	rows, err := h.f.store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	reconcileSessionBeadsAtPath(context.Background(), h.f.cityPath, rows, h.desired,
		h.names, h.f.cfg, h.f.sp, h.f.store, newDrainOps(h.f.sp), nil, nil, nil,
		h.drains, nil, false, nil, "test-city", nil, h.clk, events.Discard, time.Minute, time.Second,
		h.f.stdout, h.f.stderr, withStartStabilityWaiter(immediateStartStabilityWaiter),
		withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter))
	h.clk.Advance(time.Minute)
}

func markStartupRepair(t *testing.T, h *startupRepairFixture, id, name string, content sessionpkg.StartupContent) string {
	t.Helper()
	content.Baseline = startupContentBaseline(templateParamsToConfig(h.desired[name]))
	patch := sessionpkg.StartupContentPatch(content)
	maps.Copy(patch, sessionpkg.StartupContentBaselinePatch(content))
	if err := sessionFrontDoor(h.f.store).ApplyPatch(id, patch); err != nil {
		t.Fatal(err)
	}
	return patch[sessionpkg.StartupContentMetadataKey]
}

func holdStartupRepairCache(t *testing.T) func() {
	t.Helper()
	root, err := config.GlobalRepoCacheRoot()
	if err != nil {
		t.Fatal(err)
	}
	if scratch := os.Getenv("GC_HOME"); scratch == "" || !pathWithin(scratch, root) {
		t.Fatalf("refusing cache lock outside scratch GC_HOME: %s", root)
	}
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := config.WithRepoCacheWriteLock(root, func() (string, error) {
			close(held)
			<-release
			return "", nil
		})
		done <- err
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("taking scratch cache lock: %v", err)
	}
	var once sync.Once
	unlock := func() {
		once.Do(func() {
			close(release)
			if err := <-done; err != nil {
				t.Errorf("releasing scratch cache lock: %v", err)
			}
		})
	}
	t.Cleanup(unlock)
	return unlock
}

func TestStartupContentRepairCheckPerPass(t *testing.T) {
	for _, count := range []int{0, 1, 20} {
		t.Run(fmt.Sprintf("marked_%d", count), func(t *testing.T) {
			h := newStartupRepairFixture(t)
			marks := map[string]string{}
			for i := range count {
				id, name := h.f.beadID, "worker"
				if i > 0 {
					name = fmt.Sprintf("worker-%d", i)
					tp := h.desired["worker"]
					tp.SessionName = name
					h.desired[name], h.names[name] = tp, true
					h.f.cfg.NamedSessions = append(h.f.cfg.NamedSessions, config.NamedSession{Name: name, Template: "worker", Mode: "always"})
					b, err := h.f.store.Create(beads.Bead{Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
						"session_name": name, "template": "worker", "generation": "1", "state": "active",
						"configured_named_session": "true", "configured_named_identity": name,
					}})
					if err != nil {
						t.Fatal(err)
					}
					id = b.ID
					if err := h.f.sp.Start(context.Background(), name, templateParamsToConfig(tp)); err != nil {
						t.Fatal(err)
					}
				}
				marks[id] = markStartupRepair(t, h, id, name, sessionpkg.StartupContent{Skipped: true, Reason: "dirty import at start"})
			}
			stray := filepath.Join(h.f.cacheDirs[1], "unrelated-file")
			config.ResetRemoteCacheValidationCache()
			check := func(label string, unlock func()) {
				t.Helper()
				before, begin := loadCityConfigCalls.Load(), time.Now()
				if unlock == nil {
					h.tick(t)
				} else {
					done := make(chan struct{})
					go func() { defer close(done); h.tick(t) }()
					select {
					case <-done:
					case <-time.After(time.Second):
						t.Errorf("%s: reconcile waited on the cache write lock", label)
					}
					unlock()
					<-done
					for name := range h.names {
						if h.f.sp.CountCalls("Start", name) != 1 {
							t.Errorf("busy cache restarted %s before confirming clean content", name)
						}
					}
					for id := range marks {
						if h.drains.get(id) != nil {
							t.Errorf("busy cache started a repair drain for %s", id)
						}
					}
				}
				loads := loadCityConfigCalls.Load() - before
				t.Logf("%s: marked=%d elapsed=%s config_loads=%d", label, count, time.Since(begin), loads)
				wantLoads := int64(0)
				if count > 0 {
					wantLoads = 1
				}
				if loads != wantLoads {
					t.Errorf("%s: config loads=%d for %d marked sessions", label, loads, count)
				}
				for id, mark := range marks {
					if got := mustGetBead(t, h.f.store, id).Metadata[sessionpkg.StartupContentMetadataKey]; got != mark {
						t.Errorf("%s: pending repair lost for %s", label, id)
					}
				}
			}
			check("lock held", holdStartupRepairCache(t))
			for name := range h.names {
				h.f.sp.Attached[name] = true
			}
			if err := os.WriteFile(stray, []byte("dirty"), 0o600); err != nil {
				t.Fatal(err)
			}
			check("dirty import", nil)
			if err := os.Remove(stray); err != nil {
				t.Fatal(err)
			}
			check("clean but attached", nil)
		})
	}
}

func TestStartupContentRepairSurvivesRebaseline(t *testing.T) {
	for _, path := range []string{"soft_reload", "alias", "soft_reload_dirty_config", "alias_dirty_config", "alive_legacy", "asleep_legacy", "alive_version", "asleep_version", "live_hash", "generated_hooks", "warm_relaunch"} {
		t.Run(path, func(t *testing.T) {
			h := newStartupRepairFixture(t)
			source := sessionpkg.StartupContentSource{Path: filepath.Join(h.f.cacheDirs[1], "skills", "plan")}
			source.Hash = startupSourceHash(source)
			stray := filepath.Join(source.Path, "stray")
			if err := os.WriteFile(stray, []byte("dirty"), 0o600); err != nil {
				t.Fatal(err)
			}
			markStartupRepair(t, h, h.f.beadID, "worker", sessionpkg.StartupContent{Sources: []sessionpkg.StartupContentSource{source}})
			dirtyConfig := path == "soft_reload_dirty_config" || path == "alias_dirty_config"
			if !dirtyConfig {
				if err := os.Remove(stray); err != nil {
					t.Fatal(err)
				}
			}
			front := sessionFrontDoor(h.f.store)
			h.f.sp.Attached["worker"] = true
			starts := h.f.sp.CountCalls("Start", "worker")
			tp := h.desired["worker"]
			if dirtyConfig {
				tp.Command += " updated-config"
				h.desired["worker"] = tp
			}
			info, err := front.Get(h.f.beadID)
			if err != nil {
				t.Fatal(err)
			}
			var rebaseErr error
			switch path {
			case "soft_reload", "soft_reload_dirty_config":
				preview := reloadSessionChanges([]sessionpkg.Info{info}, h.desired, h.f.cfg, true)
				if dirtyConfig && (len(preview) != 1 || preview[0].Action != "accept") {
					t.Error("soft reload preview hid an unrelated config acceptance")
				}
				result := acceptConfigDriftAcrossSessions(front, h.desired, nil, h.f.sp, h.drains, h.f.stderr)
				if result.Failed != 0 {
					t.Fatalf("soft reload failed: %+v", result)
				}
			case "alias", "alias_dirty_config":
				patch := sessionpkg.MetadataPatch{}
				queueAliasChangeDriftRebaseline(front, mustGetBead(t, h.f.store, info.ID), tp, func(k, v string) { patch[k] = v }, h.f.stderr)
				rebaseErr = front.ApplyPatch(info.ID, patch)
			case "alive_legacy", "asleep_legacy", "alive_version", "asleep_version":
				hash := "legacy"
				if path == "alive_version" || path == "asleep_version" {
					hash = "v0:old"
				}
				if err := front.ApplyPatch(info.ID, sessionpkg.MetadataPatch{"started_config_hash": hash}); err != nil {
					t.Fatal(err)
				}
				if path == "asleep_legacy" || path == "asleep_version" {
					if err := h.f.sp.Stop("worker"); err != nil {
						t.Fatal(err)
					}
					if err := front.ApplyPatch(info.ID, sessionpkg.MetadataPatch{"state": "asleep"}); err != nil {
						t.Fatal(err)
					}
				}
				h.tick(t)
			case "live_hash", "generated_hooks":
				// Both writers share the silent-rebaseline boundary; the latter
				// additionally preserves the old live/overlay evidence.
				_, rebaseErr = silentRebaselineSessionHashes(info, front, sessionCoreConfigForHashInfo(tp, info), info.CoreHashBreakdown, path == "generated_hooks")
			case "warm_relaunch":
				cfg := sessionCoreConfigForHashInfo(tp, info)
				relaunched, _ := relaunchAgentForLaunchDrift(context.Background(), h.f.sp, front, info, "worker", tp, h.f.cityPath, h.f.cfg, h.f.store,
					info.StartedConfigHash, runtime.CoreFingerprint(cfg), runtime.ProvisionFingerprint(cfg), "old-launch", nil, events.Discard, nil, h.f.stdout, h.f.stderr)
				t.Logf("warm relaunch performed=%t", relaunched)
			}
			if rebaseErr != nil {
				t.Fatal(rebaseErr)
			}
			after := mustGetBead(t, h.f.store, info.ID)
			if after.Metadata[sessionpkg.StartupContentMetadataKey] != "" && after.Metadata["started_config_hash"] == runtime.CoreFingerprint(templateParamsToConfig(tp)) {
				t.Error("rebaseline accepted clean hash while the repair was still pending")
			}
			if got := h.f.sp.CountCalls("Start", "worker"); got != starts {
				t.Fatal("rebaseline restarted a deferred session")
			}
			if dirtyConfig {
				if after.Metadata["started_config_hash"] != runtime.CoreFingerprint(sessionCoreConfigForHashInfo(tp, info)) {
					t.Error("unrelated config drift remains while startup content is still dirty")
				}
				if err := os.Remove(stray); err != nil {
					t.Fatal(err)
				}
			}
			h.f.sp.Attached["worker"] = false
			for range 6 {
				h.tick(t)
			}
			restarts := h.f.sp.CountCalls("Start", "worker") - starts
			t.Logf("%s: repair starts=%d", path, restarts)
			if restarts != 1 {
				t.Errorf("repair starts=%d, want exactly 1; %s", restarts, h.f.stderr.String())
			}
			if got := mustGetBead(t, h.f.store, info.ID).Metadata[sessionpkg.StartupContentMetadataKey]; got != "" {
				t.Error("repair record survived clean restart")
			}
		})
	}
}

type startupReceiptFailStore struct{ beads.Store }

func (s startupReceiptFailStore) Update(string, beads.UpdateOpts) error {
	return errors.New("injected startup metadata failure")
}

func TestStartupContentReceiptFailureCleanup(t *testing.T) {
	for _, failure := range []string{"launch", "metadata", "recovery"} {
		t.Run(failure, func(t *testing.T) {
			f := newCachedImportFixture(t)
			f.cfg.Agents[0].WorkDir = t.TempDir()
			if err := f.sp.Stop("worker"); err != nil {
				t.Fatal(err)
			}
			var store beads.Store = f.store
			switch failure {
			case "launch":
				f.sp.StartErrors["worker"] = errors.New("injected launch failure")
			case "metadata":
				store = startupReceiptFailStore{Store: store}
			case "recovery":
				f.sp.StartErrors["worker"] = runtime.ErrRuntimeUnavailable
			}
			for range 3 {
				info, err := sessionFrontDoor(f.store).Get(f.beadID)
				if err != nil {
					t.Fatal(err)
				}
				tp := f.cr.buildFn(f.cfg, f.sp, f.store).State["worker"]
				prepared, _, err := buildPreparedStart(startCandidate{info: info, tp: tp}, f.cfg, f.store)
				if err != nil {
					t.Fatal(err)
				}
				result := runPreparedStartCandidate(context.Background(), *prepared, f.cityPath, f.sp, store, f.cfg, time.Minute,
					immediateStartStabilityWaiter, immediateSessionStaleKeyDetectionWaiter, nil)
				if failure != "recovery" && result.err == nil {
					t.Fatal("injected failure was not returned")
				}
				entries, err := os.ReadDir(filepath.Join(f.cityPath, ".gc", "runtime", "startup-content"))
				if err != nil {
					t.Fatal(err)
				}
				if failure == "recovery" {
					if len(entries) != 1 || !startOutcomeDefersCommit(result.outcome) {
						t.Fatal("uncertain runtime lost its recovery receipt")
					}
					break
				}
				if len(entries) != 0 {
					t.Errorf("%s failure left %d receipts", failure, len(entries))
				}
			}
		})
	}
}
