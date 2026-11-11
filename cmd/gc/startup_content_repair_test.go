package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
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
				before := loadCityConfigCalls.Load() + loadPrematerializedCityConfigCalls.Load()
				begin := time.Now()
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
				loads := loadCityConfigCalls.Load() + loadPrematerializedCityConfigCalls.Load() - before
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
				if dirtyConfig && !beginSessionDrainInfo(info, h.f.sp, h.drains, "config-drift", h.clk, time.Minute) {
					t.Fatal("could not queue the old config drift")
				}
				preview := reloadSessionChanges([]sessionpkg.Info{info}, h.desired, h.f.cfg, true)
				if dirtyConfig && (len(preview) != 1 || preview[0].Action != "accept") {
					t.Error("soft reload preview hid an unrelated config acceptance")
				}
				result := acceptConfigDriftAcrossSessions(front, h.desired, nil, h.f.sp, h.drains, h.f.stderr)
				if result.Failed != 0 {
					t.Fatalf("soft reload failed: %+v", result)
				}
				if dirtyConfig && (result.CanceledDrains != 1 || h.drains.get(info.ID) != nil) {
					t.Error("accepted config left its obsolete drain queued")
				}
			case "alias", "alias_dirty_config":
				patch := sessionpkg.MetadataPatch{}
				queueAliasChangeDriftRebaseline(front, mustGetBead(t, h.f.store, info.ID), tp, func(k, v string) { patch[k] = v }, h.f.stderr)()
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

// A launch receipt is not a repair: every hash writer must leave a clean
// in-flight start clean, including when recovery runs before its async commit.
func TestStartupContentCleanPendingRebaseline(t *testing.T) {
	for _, shape := range []string{"ordinary", "named"} {
		for _, path := range []string{"none", "soft_reload", "alias", "soft_preview", "alive_legacy", "asleep_legacy", "alive_version", "asleep_version", "live_hash", "generated_hooks", "warm_relaunch"} {
			for _, timing := range []string{"after_window", "in_window_then_commit"} {
				t.Run(shape+"/"+path+"/"+timing, func(t *testing.T) {
					origProbe := staleResumeKeyProbe
					staleResumeKeyProbe = func(string, string, string) (bool, bool) { return true, true }
					t.Cleanup(func() { staleResumeKeyProbe = origProbe })
					f := newCachedImportFixture(t, func(f *cachedImportFixture) {
						f.cfg.Agents[0].WorkDir = t.TempDir()
					})
					t.Setenv("GC_CITY", f.cityPath)
					t.Chdir(f.cityPath)
					if shape == "named" {
						f.cfg.NamedSessions = []config.NamedSession{{Name: "worker", Template: "worker", Mode: "always"}}
						if err := f.store.SetMetadataBatch(f.beadID, map[string]string{
							"configured_named_session": "true", "configured_named_identity": "worker", "pin_awake": "",
						}); err != nil {
							t.Fatal(err)
						}
					}
					h := &startupRepairFixture{
						f: f, clk: &clock.Fake{Time: time.Now()}, drains: newDrainTracker(),
						desired: f.cr.buildFn(f.cfg, f.sp, f.store).State, names: map[string]bool{"worker": true},
					}
					front := sessionFrontDoor(f.store)
					read := func() sessionpkg.Info {
						t.Helper()
						info, err := front.Get(f.beadID)
						if err != nil {
							t.Fatal(err)
						}
						return info
					}
					for range 3 {
						h.tick(t)
					}
					clean := read().StartedConfigHash
					if clean == "" || read().StartupContent != "" || !f.sp.IsRunning("worker") {
						t.Fatal("fixture has no clean committed baseline")
					}
					if err := f.sp.Stop("worker"); err != nil {
						t.Fatal(err)
					}
					if _, _, _, err := preWakeCommit(read(), front, h.clk); err != nil {
						t.Fatal(err)
					}
					tp := h.desired["worker"]
					prepared, _, err := buildPreparedStart(startCandidate{info: read(), tp: tp}, f.cfg, f.store)
					if err != nil {
						t.Fatal(err)
					}
					result := runPreparedStartCandidate(context.Background(), *prepared, f.cityPath, f.sp, f.store, f.cfg, time.Minute,
						immediateStartStabilityWaiter, immediateSessionStaleKeyDetectionWaiter, nil)
					if result.err != nil {
						t.Fatal(result.err)
					}
					info := read()
					content, err := sessionpkg.DecodeStartupContent(info.StartupContent)
					if err != nil || !content.Pending || content.Receipt == "" || content.Skipped || len(content.Sources) != 0 || info.StartedConfigHash != clean {
						t.Fatalf("launch did not leave a clean Pending receipt: content=%+v err=%v hash clean=%t", content, err, info.StartedConfigHash == clean)
					}
					starts := f.sp.CountCalls("Start", "worker")
					switch path {
					case "soft_reload":
						accepted := acceptConfigDriftAcrossSessions(front, h.desired, nil, f.sp, h.drains, f.stderr)
						if accepted.Failed != 0 || accepted.Updated != 0 {
							t.Errorf("soft reload changed a clean start: %+v", accepted)
						}
					case "alias":
						patch := sessionpkg.MetadataPatch{}
						queueAliasChangeDriftRebaseline(front, mustGetBead(t, f.store, info.ID), tp, func(k, v string) { patch[k] = v }, f.stderr)()
						err = front.ApplyPatch(info.ID, patch)
					case "soft_preview":
						if changes := reloadSessionChanges([]sessionpkg.Info{info}, h.desired, f.cfg, true); len(changes) != 0 {
							t.Errorf("soft preview listed a clean start: %+v", changes)
						}
					case "alive_legacy", "asleep_legacy", "alive_version", "asleep_version", "live_hash", "generated_hooks":
						// Exercise the shared write boundary with the snapshot each
						// caller supplies, before recovery can clear the Pending receipt.
						if strings.HasSuffix(path, "legacy") {
							info.StartedConfigHash = "legacy"
						} else if strings.HasSuffix(path, "version") {
							info.StartedConfigHash = "v0:old"
						}
						if strings.HasPrefix(path, "asleep_") {
							info.State = "asleep"
						}
						_, err = silentRebaselineSessionHashes(info, front, sessionCoreConfigForHashInfo(tp, info), info.CoreHashBreakdown, path == "generated_hooks")
					case "warm_relaunch":
						cfg := sessionCoreConfigForHashInfo(tp, info)
						relaunched, _ := relaunchAgentForLaunchDrift(context.Background(), f.sp, front, info, "worker", tp, f.cityPath, f.cfg, f.store,
							info.StartedConfigHash, runtime.CoreFingerprint(cfg), runtime.ProvisionFingerprint(cfg), "old-launch", nil, events.Discard, nil, f.stdout, f.stderr)
						if !relaunched || f.sp.CountCalls("Relaunch", "worker") != 1 {
							t.Errorf("clean Pending receipt blocked the warm relaunch: %s", f.stderr.String())
						}
					}
					if err != nil {
						t.Fatal(err)
					}
					if read().StartedConfigHash != clean {
						t.Error("rebaseline stamped a repair fingerprint on a clean Pending receipt")
					}
					if timing == "after_window" {
						h.clk.Advance(3 * time.Minute)
					}
					h.tick(t)
					drains := 0
					if d := h.drains.get(info.ID); d != nil && d.reason == "config-drift" {
						drains++
					}
					if timing == "in_window_then_commit" {
						if !commitAsyncStartResultWithContext(context.Background(), result, f.sp, f.store, h.clk, events.Discard, 0, f.stdout, f.stderr, nil) {
							t.Error("late start commit was rejected")
						}
					}
					for range 8 {
						h.tick(t)
						if d := h.drains.get(info.ID); d != nil && d.reason == "config-drift" {
							drains++
						}
					}
					restarts := f.sp.CountCalls("Start", "worker") - starts
					driftLines := strings.Count(f.stderr.String(), "config-drift worker")
					after := read()
					t.Logf("restarts=%d config-drift drains=%d drift lines=%d hash clean=%t record cleared=%t", restarts, drains, driftLines, after.StartedConfigHash == clean, after.StartupContent == "")
					if restarts != 0 || drains != 0 || driftLines != 0 || after.StartedConfigHash != clean || after.StartupContent != "" || !f.sp.IsRunning("worker") {
						t.Errorf("clean launch was not preserved: %s", f.stderr.String())
					}
				})
			}
		}
	}
}

func TestStartupContentRepairSurvivesStalePendingRebaseline(t *testing.T) {
	for _, path := range []string{"control", "soft_reload", "alias"} {
		t.Run(path, func(t *testing.T) {
			oldProbe := staleResumeKeyProbe
			staleResumeKeyProbe = func(string, string, string) (bool, bool) { return true, true }
			t.Cleanup(func() { staleResumeKeyProbe = oldProbe })
			h := newStartupRepairFixture(t)
			f := h.f
			t.Setenv("GC_CITY", f.cityPath)
			t.Chdir(f.cityPath)
			front := sessionFrontDoor(f.store)
			read := func() sessionpkg.Info {
				t.Helper()
				info, err := front.Get(f.beadID)
				if err != nil {
					t.Fatal(err)
				}
				return info
			}
			source := sessionpkg.StartupContentSource{Path: filepath.Join(f.cacheDirs[1], "skills", "plan")}
			source.Hash = startupSourceHash(source)
			stray := filepath.Join(source.Path, "stray")
			if err := os.WriteFile(stray, []byte("dirty delivery"), 0o600); err != nil {
				t.Fatal(err)
			}
			markStartupRepair(t, h, f.beadID, "worker", sessionpkg.StartupContent{Sources: []sessionpkg.StartupContentSource{source}})
			priorRepair := read().StartedConfigHash
			if err := f.sp.Stop("worker"); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := preWakeCommit(read(), front, h.clk); err != nil {
				t.Fatal(err)
			}
			tp := h.desired["worker"]
			prepared, _, err := buildPreparedStart(startCandidate{info: read(), tp: tp}, f.cfg, f.store)
			if err != nil {
				t.Fatal(err)
			}
			result := runPreparedStartCandidate(context.Background(), *prepared, f.cityPath, f.sp, f.store, f.cfg, time.Minute,
				immediateStartStabilityWaiter, immediateSessionStaleKeyDetectionWaiter, nil)
			if result.err != nil {
				t.Fatal(result.err)
			}
			pending, err := sessionpkg.DecodeStartupContent(read().StartupContent)
			if err != nil || !pending.Pending || len(pending.Sources) == 0 || read().StartedConfigHash != priorRepair {
				t.Fatalf("fixture did not launch dirty with a prior repair hash: pending=%t sources=%d sameHash=%t err=%v", pending.Pending, len(pending.Sources), read().StartedConfigHash == priorRepair, err)
			}
			if err := os.Remove(stray); err != nil {
				t.Fatal(err)
			}
			snapshot, err := loadSessionBeadSnapshot(f.store)
			if err != nil {
				t.Fatal(err)
			}
			aliasPatch := sessionpkg.MetadataPatch{}
			var aliasRebaseline func()
			if path == "alias" {
				aliasRebaseline = queueAliasChangeDriftRebaseline(front, mustGetBead(t, f.store, f.beadID), tp, func(k, v string) { aliasPatch[k] = v }, f.stderr)
			}
			if !commitAsyncStartResultWithContext(context.Background(), result, f.sp, f.store, h.clk, events.Discard, 0, f.stdout, f.stderr, nil) {
				t.Fatal("dirty async start commit rejected")
			}
			finalized, err := sessionpkg.DecodeStartupContent(read().StartupContent)
			if err != nil || finalized.Pending || len(finalized.Sources) == 0 || read().StartedConfigHash != priorRepair {
				t.Fatalf("fixture did not finalize dirty: pending=%t sources=%d sameHash=%t err=%v", finalized.Pending, len(finalized.Sources), read().StartedConfigHash == priorRepair, err)
			}
			switch path {
			case "soft_reload":
				accepted := acceptConfigDriftAcrossSessions(front, h.desired, snapshot, f.sp, h.drains, f.stderr)
				t.Logf("stale acceptance updated=%d failed=%d", accepted.Updated, accepted.Failed)
				if accepted.Failed != 0 {
					t.Fatal("acceptance failed")
				}
			case "alias":
				aliasRebaseline()
				if err := front.ApplyPatch(f.beadID, aliasPatch); err != nil {
					t.Fatal(err)
				}
			}
			starts := f.sp.CountCalls("Start", "worker")
			for range 8 {
				h.tick(t)
			}
			repairs := f.sp.CountCalls("Start", "worker") - starts
			t.Logf("repair restarts=%d record retained=%t", repairs, read().StartupContent != "")
			if repairs != 1 {
				t.Errorf("restored source never repaired the dirty start: got %d restarts, want 1", repairs)
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
