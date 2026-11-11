package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type startupRebaselineWrites struct {
	beads.Store
	reads, writes int
	readErr       error
	beforeWrite   func()
}

func (s *startupRebaselineWrites) Get(id string) (beads.Bead, error) {
	s.reads++
	if s.readErr != nil {
		return beads.Bead{}, s.readErr
	}
	return s.Store.Get(id)
}

func (s *startupRebaselineWrites) SetMetadataBatch(id string, patch map[string]string) error {
	s.writes++
	if s.beforeWrite != nil {
		s.beforeWrite()
	}
	return s.Store.SetMetadataBatch(id, patch)
}

type startupRebaselineWindow struct {
	t       *testing.T
	f       *cachedImportFixture
	sp      *startupMaterializingRuntime
	clk     *clock.Fake
	drains  *drainTracker
	desired map[string]TemplateParams
	names   map[string]bool
}

type startupRebaselineRelaunch struct {
	*runtime.Fake
	commit func()
}

// Relaunch places the startup commit inside the runtime action's window.
func (s *startupRebaselineRelaunch) Relaunch(ctx context.Context, name string, cfg runtime.Config) error {
	s.commit()
	return s.Fake.Relaunch(ctx, name, cfg)
}

func (h *startupRebaselineWindow) tick() {
	h.t.Helper()
	rows, err := h.f.store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		h.t.Fatal(err)
	}
	reconcileSessionBeadsAtPath(context.Background(), h.f.cityPath, rows, h.desired,
		h.names, h.f.cfg, h.sp, h.f.store, newDrainOps(h.sp), nil, nil, nil,
		h.drains, nil, false, nil, "test-city", nil, h.clk, events.Discard, time.Minute, time.Second,
		h.f.stdout, h.f.stderr, withStartStabilityWaiter(immediateStartStabilityWaiter),
		withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter))
	h.clk.Advance(time.Minute)
}

func (h *startupRebaselineWindow) read() sessionpkg.Info {
	h.t.Helper()
	info, err := sessionFrontDoor(h.f.store).Get(h.f.beadID)
	if err != nil {
		h.t.Fatal(err)
	}
	return info
}

func startupRebaselineWindowRecord(raw string) string {
	if raw == "" {
		return "(none)"
	}
	c, err := sessionpkg.DecodeStartupContent(raw)
	if err != nil {
		return "undecodable"
	}
	return fmt.Sprintf("{pending=%v sources=%d skipped=%v receipt=%v}", c.Pending, len(c.Sources), c.Skipped, c.Receipt != "")
}

func startupRebaselineWindowShort(s string) string {
	if len(s) > 15 {
		return s[:15]
	}
	return s
}

// TestStartupContentRebaselineWriteObservation: case x precondition x writer.
//
//	case receipt_skip: an unrelated file at the locked-import root makes the
//	  PreStart's materialize-skills skip (reported only in the receipt FILE);
//	  the tracked startup sources stay clean, so the bead's Pending record has
//	  no Sources and Skipped=false. The cache is restored before the commit.
//	case commit_sources: the launch is clean (materialization succeeds, no
//	  Sources); a tracked source turns dirty after the snapshot and before the
//	  commit, so finishStartupContent first observes it at the commit. The
//	  source is restored after the commit.
//	precondition prior_repair (the gate's): the stored hash before the launch
//	  is a prior repair's marked hash. precondition config_edit: the stored
//	  hash is clean and the soft reload accepts a real operator-env edit (the
//	  alias writer queues its hash either way).
func TestStartupContentRebaselineWriteObservation(t *testing.T) {
	t.Run("clean_config_edit", testStartupContentRebaselineCleanConfigEdit)
	t.Run("write_boundary", testStartupContentRebaselineBoundary)
	t.Run("pending_asleep_migration", testStartupContentPendingAsleepMigration)
	for _, kase := range []string{"receipt_skip", "commit_sources", "pre_receipt"} {
		for _, pre := range []string{"prior_repair", "config_edit"} {
			for _, writer := range []string{"control", "soft_reload", "alias", "legacy", "version", "live_hash", "generated_hooks", "warm_relaunch", "warm_relaunch_during_commit"} {
				name := kase + "/" + pre + "/" + writer
				t.Run(name, func(t *testing.T) {
					oldProbe := staleResumeKeyProbe
					staleResumeKeyProbe = func(string, string, string) (bool, bool) { return true, true }
					t.Cleanup(func() { staleResumeKeyProbe = oldProbe })
					workdir := t.TempDir()
					f := newCachedImportFixture(t, func(f *cachedImportFixture) {
						f.cfg.Agents[0].WorkDir = workdir // not the scope root: PreStart materializes skills, a receipt per start
					})
					t.Setenv("GC_CITY", f.cityPath)
					t.Chdir(f.cityPath)
					f.cfg.NamedSessions = []config.NamedSession{{Name: "worker", Template: "worker", Mode: "always"}}
					if err := f.store.SetMetadataBatch(f.beadID, map[string]string{
						"configured_named_session": "true", "configured_named_identity": "worker", "pin_awake": "",
					}); err != nil {
						t.Fatal(err)
					}
					materialize := func() error {
						config.ResetRemoteCacheValidationCache() // a fresh PreStart process
						cmd := newInternalMaterializeSkillsCmd(f.stdout, f.stderr)
						cmd.SetArgs([]string{"--best-effort", "--agent", "worker", "--workdir", workdir})
						return cmd.Execute()
					}
					var afterPreStart func()
					sp := &startupMaterializingRuntime{Fake: f.sp, prepare: func(cfg runtime.Config) error {
						if err := runtime.StageWorkDir(workdir, "", cfg.CopyFiles); err != nil {
							return err
						}
						t.Setenv("GC_STARTUP_CONTENT_RECEIPT", cfg.Env["GC_STARTUP_CONTENT_RECEIPT"])
						if err := materialize(); err != nil {
							return err
						}
						if afterPreStart != nil {
							afterPreStart()
						}
						return nil
					}}
					h := &startupRebaselineWindow{
						t: t, f: f, sp: sp, clk: &clock.Fake{Time: time.Now()}, drains: newDrainTracker(),
						desired: f.cr.buildFn(f.cfg, sp, f.store).State, names: map[string]bool{"worker": true},
					}
					for range 3 {
						h.tick()
					}
					clean := h.read().StartedConfigHash
					if clean == "" || h.read().StartupContent != "" || !f.sp.IsRunning("worker") {
						t.Fatalf("no clean committed baseline: hash=%q record=%s running=%v; %s", clean, startupRebaselineWindowRecord(h.read().StartupContent), f.sp.IsRunning("worker"), f.stderr.String())
					}
					writerStore := &startupRebaselineWrites{Store: f.store}
					front := sessionFrontDoor(writerStore)
					tp := h.desired["worker"]
					if pre == "prior_repair" {
						// The gate: retain a prior repair hash (a finalized repair whose
						// marked hash is the stored hash when this start launches).
						prior := sessionpkg.StartupContent{Skipped: true, Reason: "prior dirty start"}
						prior.Baseline = startupContentBaseline(templateParamsToConfig(tp))
						patch := sessionpkg.StartupContentPatch(prior)
						maps.Copy(patch, sessionpkg.StartupContentBaselinePatch(prior))
						if err := front.ApplyPatch(f.beadID, patch); err != nil {
							t.Fatal(err)
						}
					}
					priorHash := h.read().StartedConfigHash
					unrelated := filepath.Join(f.cacheDirs[1], "unrelated-file")
					trackedStray := filepath.Join(f.cacheDirs[1], "skills", "plan", "stray")
					if kase == "receipt_skip" {
						if err := os.WriteFile(unrelated, []byte("unrelated"), 0o644); err != nil {
							t.Fatal(err)
						}
					}
					if err := f.sp.Stop("worker"); err != nil {
						t.Fatal(err)
					}
					if _, _, _, err := preWakeCommit(h.read(), front, h.clk); err != nil {
						t.Fatal(err)
					}
					prepared, _, err := buildPreparedStart(startCandidate{info: h.read(), tp: tp}, f.cfg, f.store)
					if err != nil {
						t.Fatal(err)
					}
					var snapshot *sessionBeadSnapshot
					var infoAtSnapshot sessionpkg.Info
					aliasPatch := sessionpkg.MetadataPatch{}
					var aliasRebaseline func()
					takeSnapshot := func() {
						t.Helper()
						infoAtSnapshot = h.read()
						snapshot, err = loadSessionBeadSnapshot(f.store)
						if err != nil {
							t.Fatal(err)
						}
						if writer == "alias" {
							aliasRebaseline = queueAliasChangeDriftRebaseline(front, mustGetBead(t, f.store, f.beadID), tp, func(k, v string) { aliasPatch[k] = v }, f.stderr)
						}
					}
					if kase == "pre_receipt" {
						// The new launch has not installed its record yet. A prior
						// repair hash may remain, but this snapshot sees no receipt.
						if err := front.ApplyPatch(f.beadID, sessionpkg.MetadataPatch{sessionpkg.StartupContentMetadataKey: ""}); err != nil {
							t.Fatal(err)
						}
						takeSnapshot()
						if infoAtSnapshot.StartupContent != "" {
							t.Fatal("pre-receipt snapshot has a record")
						}
					}
					// Premise (i): the bead's record as the launch leaves it, and the
					// receipt FILE as the PreStart leaves it.
					var receiptAtPreStart string
					afterPreStart = func() {
						b, _ := os.ReadFile(os.Getenv("GC_STARTUP_CONTENT_RECEIPT"))
						receiptAtPreStart = strings.TrimSpace(string(b))
					}
					startsBeforeLaunch := f.sp.CountCalls("Start", "worker")
					result := runPreparedStartCandidate(context.Background(), *prepared, f.cityPath, sp, f.store, f.cfg, time.Minute,
						immediateStartStabilityWaiter, immediateSessionStaleKeyDetectionWaiter, nil)
					afterPreStart = nil
					if result.err != nil {
						t.Fatal(result.err)
					}
					atLaunch := h.read()
					launchContent, err := sessionpkg.DecodeStartupContent(atLaunch.StartupContent)
					if err != nil {
						t.Fatal(err)
					}
					var receiptFile sessionpkg.StartupContent
					if launchContent.Receipt != "" {
						b, err := os.ReadFile(launchContent.Receipt)
						if err == nil {
							_ = json.Unmarshal(b, &receiptFile)
						}
					}
					t.Logf("R3 %s: after launch, bead record=%s stored=%s (prior=%v clean=%v); receipt file at PreStart=%s; receipt file now skipped=%v",
						name, startupRebaselineWindowRecord(atLaunch.StartupContent), startupRebaselineWindowShort(atLaunch.StartedConfigHash), atLaunch.StartedConfigHash == priorHash,
						atLaunch.StartedConfigHash == clean, receiptAtPreStart, receiptFile.Skipped)
					if !launchContent.Pending || launchContent.Skipped || len(launchContent.Sources) != 0 || launchContent.Receipt == "" {
						t.Fatalf("premise: the bead's record is not Pending with no observations: %s", startupRebaselineWindowRecord(atLaunch.StartupContent))
					}
					if kase == "receipt_skip" && !receiptFile.Skipped {
						t.Fatalf("premise: the receipt file does not record the skip: %s", receiptAtPreStart)
					}
					// The writers' snapshots, taken now (after the launch, before the commit).
					desiredAtWrite := h.desired
					if pre == "config_edit" && writer != "alias" {
						edited := tp
						edited.OperatorEnv = maps.Clone(edited.OperatorEnv)
						if edited.OperatorEnv == nil {
							edited.OperatorEnv = map[string]string{}
						}
						edited.OperatorEnv["OPUS_R3_ACCEPTED"] = "updated"
						edited.Env = maps.Clone(edited.Env)
						edited.Env["OPUS_R3_ACCEPTED"] = "updated"
						desiredAtWrite = map[string]TemplateParams{"worker": edited}
						h.desired = desiredAtWrite
					}
					if kase != "pre_receipt" {
						takeSnapshot()
					}
					// Between the snapshot and the commit.
					switch kase {
					case "receipt_skip":
						if err := os.Remove(unrelated); err != nil { // the gate: restore the cache, then commit
							t.Fatal(err)
						}
					case "commit_sources", "pre_receipt":
						if err := os.WriteFile(trackedStray, []byte("dirty at commit"), 0o644); err != nil {
							t.Fatal(err)
						}
					}
					finalize := func() {
						t.Helper()
						if !commitAsyncStartResultWithContext(context.Background(), result, f.sp, f.store, h.clk, events.Discard, 0, f.stdout, f.stderr, nil) {
							t.Fatalf("async commit rejected: %s", f.stderr.String())
						}
						committed := h.read()
						fin, err := sessionpkg.DecodeStartupContent(committed.StartupContent)
						if err != nil || fin.Pending || (len(fin.Sources) == 0 && !fin.Skipped) {
							t.Fatalf("the commit did not finalize a dirty delivery: %s, %v", startupRebaselineWindowRecord(committed.StartupContent), err)
						}
						if kase == "commit_sources" || kase == "pre_receipt" {
							if err := os.Remove(trackedStray); err != nil {
								t.Fatal(err)
							}
						}
					}
					if writer != "warm_relaunch_during_commit" {
						finalize()
					}
					before := h.read().StartedConfigHash
					writerStore.reads, writerStore.writes = 0, 0
					switch writer {
					case "warm_relaunch", "warm_relaunch_during_commit":
						cfg := sessionCoreConfigForHashInfo(desiredAtWrite["worker"], infoAtSnapshot)
						var warmProvider runtime.Provider = f.sp
						if writer == "warm_relaunch_during_commit" {
							warmProvider = &startupRebaselineRelaunch{Fake: f.sp, commit: finalize}
						}
						relaunched, _ := relaunchAgentForLaunchDrift(context.Background(), warmProvider, front, infoAtSnapshot, "worker", desiredAtWrite["worker"], f.cityPath, f.cfg, f.store,
							infoAtSnapshot.StartedConfigHash, runtime.CoreFingerprint(cfg), runtime.ProvisionFingerprint(cfg), "old-launch", nil, events.Discard, nil, f.stdout, f.stderr)
						// The snapshot permits a warm launch. Its write must retain
						// the newly committed repair for the next full start.
						if !relaunched || f.sp.CountCalls("Relaunch", "worker") != 1 {
							t.Error("fixture did not exercise the warm-relaunch write")
						}
					case "soft_reload":
						accepted := acceptConfigDriftAcrossSessions(front, desiredAtWrite, snapshot, f.sp, h.drains, f.stderr)
						if accepted.Failed != 0 {
							t.Fatalf("soft acceptance failed: %+v", accepted)
						}
						t.Logf("R3 %s: stale soft acceptance updated=%d failed=%d; stored %s -> %s", name, accepted.Updated, accepted.Failed, startupRebaselineWindowShort(before), startupRebaselineWindowShort(h.read().StartedConfigHash))
					case "alias":
						aliasRebaseline()
						if err := front.ApplyPatch(f.beadID, aliasPatch); err != nil {
							t.Fatal(err)
						}
						t.Logf("R3 %s: stale alias patch keys=%d; stored %s -> %s", name, len(aliasPatch), startupRebaselineWindowShort(before), startupRebaselineWindowShort(h.read().StartedConfigHash))
					case "legacy", "version", "live_hash", "generated_hooks":
						if writer == "legacy" {
							infoAtSnapshot.StartedConfigHash = "legacy"
						}
						if writer == "version" {
							infoAtSnapshot.StartedConfigHash = "v0:old"
						}
						if _, err := silentRebaselineSessionHashes(infoAtSnapshot, front, sessionCoreConfigForHashInfo(desiredAtWrite["worker"], infoAtSnapshot), infoAtSnapshot.CoreHashBreakdown, writer == "generated_hooks"); err != nil {
							t.Fatal(err)
						}
					}
					wantReads := 1
					if writer == "control" {
						wantReads = 0
					}
					t.Logf("write boundary: reads=%d writes=%d", writerStore.reads, writerStore.writes)
					if writerStore.reads != wantReads {
						t.Errorf("write-boundary reads=%d, want %d", writerStore.reads, wantReads)
					}
					starts := f.sp.CountCalls("Start", "worker")
					for range 8 {
						h.tick()
					}
					repairs := f.sp.CountCalls("Start", "worker") - starts
					end := h.read()
					t.Logf("R3 %s: launches before=%d; repair restarts after the commit=%d; record now=%s; marked-for-repair lines=%d",
						name, starts-startsBeforeLaunch, repairs, startupRebaselineWindowRecord(end.StartupContent), strings.Count(f.stderr.String(), "startup-content: session"))
					adv0, full0 := loadPrematerializedCityConfigCalls.Load(), loadCityConfigCalls.Load()
					for range 3 {
						h.tick()
					}
					t.Logf("R3 %s: three more passes: advisory config loads=%d full config loads=%d starts=%d record=%s",
						name, loadPrematerializedCityConfigCalls.Load()-adv0, loadCityConfigCalls.Load()-full0,
						f.sp.CountCalls("Start", "worker")-starts-repairs, startupRebaselineWindowRecord(h.read().StartupContent))
					if repairs != 1 {
						t.Errorf("R3 REAL: %s: a dirty delivery finalized at the commit was repaired %d times, want 1", name, repairs)
					}
				})
			}
		}
	}
}

func testStartupContentRebaselineBoundary(t *testing.T) {
	for _, writer := range []string{"soft_reload", "alias", "silent"} {
		for _, state := range []string{"pending", "pending_observed", "finalized_clean", "read_error", "malformed"} {
			t.Run(writer+"/"+state, func(t *testing.T) {
				base := beads.NewMemStore()
				old := sessionpkg.StartupContent{Skipped: true}
				metadata := map[string]string{"session_name": "worker", "template": "worker", "started_config_hash": "old"}
				maps.Copy(metadata, sessionpkg.StartupContentPatch(old))
				b, err := base.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: metadata})
				if err != nil {
					t.Fatal(err)
				}
				backing := &getCountingStore{Store: base}
				cache := beads.NewCachingStore(backing, nil)
				if err := cache.Prime(context.Background()); err != nil {
					t.Fatal(err)
				}
				measured := &startupRebaselineWrites{Store: cache}
				front := sessionFrontDoor(measured)
				info, err := front.Get(b.ID)
				if err != nil {
					t.Fatal(err)
				}
				snapshot, err := loadSessionBeadSnapshot(cache)
				if err != nil {
					t.Fatal(err)
				}
				tp := TemplateParams{Command: "worker", SessionName: "worker", TemplateName: "worker"}
				patch := sessionpkg.MetadataPatch{}
				var stderr bytes.Buffer
				alias := queueAliasChangeDriftRebaseline(front, b, tp, func(k, v string) { patch[k] = v }, &stderr)
				latest := sessionpkg.StartupContent{Pending: true, Receipt: "/unread-receipt"}
				if state == "pending_observed" {
					latest.Skipped = true
				}
				if state == "finalized_clean" {
					latest = sessionpkg.StartupContent{}
				}
				update := sessionpkg.StartupContentPatch(latest)
				if state == "malformed" {
					update[sessionpkg.StartupContentMetadataKey] = "{"
				}
				if err := front.ApplyPatch(b.ID, update); err != nil {
					t.Fatal(err)
				}
				if state == "read_error" {
					measured.readErr = errors.New("read unavailable")
				}
				backing.reset()
				measured.reads, measured.writes = 0, 0
				measured.beforeWrite = func() {
					if backing.count() != 0 {
						t.Errorf("clean cached re-read made %d backing reads", backing.count())
					}
				}
				var writeErr error
				switch writer {
				case "soft_reload":
					r := acceptConfigDriftAcrossSessions(front, map[string]TemplateParams{"worker": tp}, snapshot, nil, nil, &stderr)
					if (state == "read_error" || state == "malformed") && r.Failed != 1 {
						t.Errorf("failed read not reported: %+v", r)
					}
				case "alias":
					alias()
					writeErr = front.ApplyPatch(b.ID, patch)
				case "silent":
					_, writeErr = silentRebaselineSessionHashes(info, front, sessionCoreConfigForRebaselineInfo(tp, info), "", false)
				}
				failedRead := state == "read_error" || state == "malformed"
				if writer == "silent" && failedRead {
					if writeErr == nil {
						t.Error("read failure was swallowed")
					}
				} else if writeErr != nil {
					t.Fatal(writeErr)
				}
				if writer != "silent" && failedRead && stderr.Len() == 0 {
					t.Error("read failure was not logged")
				}
				wantWrites := 0
				if state == "finalized_clean" {
					wantWrites = 1
				}
				t.Logf("reads=%d writes=%d backing reads=%d", measured.reads, measured.writes, backing.count())
				// CachingStore refreshes after a write with its own backing Get;
				// the added pre-write Get is an in-memory read, checked above.
				if measured.reads != 1 || measured.writes != wantWrites || backing.count() != wantWrites {
					t.Errorf("write-boundary cost: reads=%d writes=%d backing reads=%d", measured.reads, measured.writes, backing.count())
				}
				after, err := base.Get(b.ID)
				if err != nil {
					t.Fatal(err)
				}
				wantHash := "old"
				if state == "finalized_clean" {
					wantHash = runtime.CoreFingerprint(sessionCoreConfigWithOverrides(tp, info))
				}
				if after.Metadata["started_config_hash"] != wantHash {
					t.Error("writer did not respect the latest delivery record")
				}
			})
		}
	}
}

// testStartupContentRebaselineCleanConfigEdit: a CLEAN start in its window when a soft
// reload accepts a real config edit (fresh snapshot, while the record is
// Pending). Restarts after the launch, per commit timing. For QUESTION C: what
// a writer that leaves Pending sessions alone would cost on the next pass.
func testStartupContentRebaselineCleanConfigEdit(t *testing.T) {
	for _, shape := range []string{"ordinary", "named"} {
		for _, writer := range []string{"control_no_reload", "soft_reload"} {
			for _, timing := range []string{"in_window_then_commit", "after_window"} {
				name := shape + "/" + writer + "/" + timing
				t.Run(name, func(t *testing.T) {
					oldProbe := staleResumeKeyProbe
					staleResumeKeyProbe = func(string, string, string) (bool, bool) { return true, true }
					t.Cleanup(func() { staleResumeKeyProbe = oldProbe })
					workdir := t.TempDir()
					f := newCachedImportFixture(t, func(f *cachedImportFixture) { f.cfg.Agents[0].WorkDir = workdir })
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
					h := &startupRebaselineWindow{
						t: t, f: f, sp: &startupMaterializingRuntime{Fake: f.sp, prepare: func(runtime.Config) error { return nil }},
						clk: &clock.Fake{Time: time.Now()}, drains: newDrainTracker(), names: map[string]bool{"worker": true},
					}
					h.desired = f.cr.buildFn(f.cfg, h.sp, f.store).State
					for range 3 {
						h.tick()
					}
					clean := h.read().StartedConfigHash
					front := sessionFrontDoor(f.store)
					tp := h.desired["worker"]
					if err := f.sp.Stop("worker"); err != nil {
						t.Fatal(err)
					}
					if _, _, _, err := preWakeCommit(h.read(), front, h.clk); err != nil {
						t.Fatal(err)
					}
					prepared, _, err := buildPreparedStart(startCandidate{info: h.read(), tp: tp}, f.cfg, f.store)
					if err != nil {
						t.Fatal(err)
					}
					result := runPreparedStartCandidate(context.Background(), *prepared, f.cityPath, h.sp, f.store, f.cfg, time.Minute,
						immediateStartStabilityWaiter, immediateSessionStaleKeyDetectionWaiter, nil)
					if result.err != nil {
						t.Fatal(result.err)
					}
					launches := f.sp.CountCalls("Start", "worker")
					edited := tp
					edited.OperatorEnv = maps.Clone(edited.OperatorEnv)
					if edited.OperatorEnv == nil {
						edited.OperatorEnv = map[string]string{}
					}
					edited.OperatorEnv["OPUS_R3_ACCEPTED"] = "updated"
					edited.Env = maps.Clone(edited.Env)
					edited.Env["OPUS_R3_ACCEPTED"] = "updated"
					h.desired = map[string]TemplateParams{"worker": edited}
					t.Logf("R3C %s: launched; record=%s stored clean=%v", name, startupRebaselineWindowRecord(h.read().StartupContent), h.read().StartedConfigHash == clean)
					if writer == "soft_reload" {
						accepted := acceptConfigDriftAcrossSessions(front, h.desired, nil, f.sp, h.drains, f.stderr)
						t.Logf("R3C %s: soft acceptance updated=%d", name, accepted.Updated)
					}
					accepted := h.read().StartedConfigHash
					if timing == "in_window_then_commit" {
						if !commitAsyncStartResultWithContext(context.Background(), result, f.sp, f.store, h.clk, events.Discard, 0, f.stdout, f.stderr, nil) {
							t.Fatalf("commit rejected: %s", f.stderr.String())
						}
					} else {
						h.clk.Advance(3 * time.Minute)
					}
					t.Logf("R3C %s: after %s: stored equals the accepted hash=%v, equals the launch hash=%v", name, timing, h.read().StartedConfigHash == accepted, h.read().StartedConfigHash == clean)
					drainTicks := 0
					for range 8 {
						h.tick()
						if d := h.drains.get(f.beadID); d != nil && d.reason == "config-drift" {
							drainTicks++
						}
					}
					t.Logf("R3C %s: restarts after the launch=%d warm relaunches=%d ticks with a config-drift drain=%d config-drift lines=%d running=%v record=%s",
						name, f.sp.CountCalls("Start", "worker")-launches, f.sp.CountCalls("Relaunch", "worker"), drainTicks, strings.Count(f.stderr.String(), "config-drift worker"),
						f.sp.IsRunning("worker"), startupRebaselineWindowRecord(h.read().StartupContent))
					if f.sp.CountCalls("Start", "worker") != launches || f.sp.CountCalls("Relaunch", "worker") != 1 || drainTicks != 0 || !f.sp.IsRunning("worker") || h.read().StartupContent != "" {
						t.Errorf("clean env edit must take one warm relaunch and no full restart: %s", f.stderr.String())
					}
				})
			}
		}
	}
}

func testStartupContentPendingAsleepMigration(t *testing.T) {
	for _, hash := range []string{"legacy", "v0:old"} {
		t.Run(hash, func(t *testing.T) {
			h := newStartupRepairFixture(t)
			f := h.f
			f.cfg.Agents[0].WorkDir = t.TempDir()
			h.desired = f.cr.buildFn(f.cfg, f.sp, f.store).State
			t.Setenv("GC_CITY", f.cityPath)
			t.Chdir(f.cityPath)
			for range 3 {
				h.tick(t)
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
			if err := f.sp.Stop("worker"); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := preWakeCommit(read(), front, h.clk); err != nil {
				t.Fatal(err)
			}
			prepared, _, err := buildPreparedStart(startCandidate{info: read(), tp: h.desired["worker"]}, f.cfg, f.store)
			if err != nil {
				t.Fatal(err)
			}
			result := runPreparedStartCandidate(context.Background(), *prepared, f.cityPath, f.sp, f.store, f.cfg, time.Minute,
				immediateStartStabilityWaiter, immediateSessionStaleKeyDetectionWaiter, nil)
			if result.err != nil {
				t.Fatal(result.err)
			}
			content, err := sessionpkg.DecodeStartupContent(read().StartupContent)
			if err != nil || !content.Pending {
				t.Fatalf("no pending launch: %+v %v", content, err)
			}
			// The launch is never committed and its runtime is subsequently lost.
			if err := f.sp.Stop("worker"); err != nil {
				t.Fatal(err)
			}
			if err := front.ApplyPatch(f.beadID, sessionpkg.MetadataPatch{"state": "asleep", "started_config_hash": hash}); err != nil {
				t.Fatal(err)
			}
			h.clk.Advance(3 * time.Minute)
			starts := f.sp.CountCalls("Start", "worker")
			for range 8 {
				h.tick(t)
			}
			t.Logf("starts=%d running=%t hash=%s", f.sp.CountCalls("Start", "worker")-starts, f.sp.IsRunning("worker"), read().StartedConfigHash)
			if f.sp.CountCalls("Start", "worker") == starts || !f.sp.IsRunning("worker") {
				t.Errorf("Pending migration prevents ordinary wake: %s", f.stderr.String())
			}
		})
	}
}
