package main

import (
	"bytes"
	"context"
	"fmt"
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

// startupMaterializingRuntime retains the fake's runtime observations while
// executing the real staging and skill materialization boundaries on Start.
type startupMaterializingRuntime struct {
	*runtime.Fake
	prepare func(runtime.Config) error
}

func (sp *startupMaterializingRuntime) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := sp.prepare(cfg); err != nil {
		return err
	}
	return sp.Fake.Start(ctx, name, cfg)
}

type startupReuseRuntime struct {
	*runtime.Fake
	hidden int
}

func (sp *startupReuseRuntime) IsRunning(name string) bool {
	if sp.hidden > 0 {
		sp.hidden--
		return false
	}
	return sp.Fake.IsRunning(name)
}

func TestStartupContentRuntimeReuse(t *testing.T) {
	for _, tc := range []struct {
		name       string
		hidden     int
		lateRecord bool
	}{
		{name: "warm"},
		{name: "worker_shortcut", hidden: 1},
		{name: "provider_collision", hidden: 2},
		{name: "warm_late_record", lateRecord: true},
		{name: "worker_shortcut_late_record", hidden: 1, lateRecord: true},
		{name: "provider_collision_late_record", hidden: 2, lateRecord: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCachedImportFixture(t)
			sp := &startupReuseRuntime{Fake: f.sp, hidden: tc.hidden}
			front := sessionFrontDoor(f.store)
			info, err := front.Get(f.beadID)
			if err != nil {
				t.Fatal(err)
			}
			tp := f.cr.buildFn(f.cfg, f.sp, f.store).State["worker"]
			prepared, _, err := buildPreparedStart(startCandidate{info: info, tp: tp}, f.cfg, f.store)
			if err != nil {
				t.Fatal(err)
			}
			expected := info.StartupContent
			if tc.lateRecord {
				patch := sessionpkg.StartupContentPatch(sessionpkg.StartupContent{
					Skipped: true, Reason: "earlier startup", Baseline: startupContentBaseline(prepared.fingerprintConfig),
				})
				if err := front.ApplyPatch(info.ID, patch); err != nil {
					t.Fatal(err)
				}
				expected = patch[sessionpkg.StartupContentMetadataKey]
			}
			path := filepath.Join(f.cacheDirs[1], "skills", "plan", "stray")
			if err := os.WriteFile(path, []byte("dirty"), 0o644); err != nil {
				t.Fatal(err)
			}
			result := runPreparedStartCandidate(context.Background(), *prepared, f.cityPath, sp, f.store, f.cfg, time.Minute,
				immediateStartStabilityWaiter, immediateSessionStaleKeyDetectionWaiter, nil)
			if result.err != nil {
				t.Fatal(result.err)
			}
			if !commitStartResultTraced(result, front, clock.Real{}, events.Discard, 1, f.stdout, f.stderr, nil) {
				t.Fatal("reused runtime did not commit")
			}
			got, err := front.Get(f.beadID)
			if err != nil {
				t.Fatal(err)
			}
			if got.StartupContent != expected {
				t.Fatalf("runtime reuse changed startup delivery state: before=%q after=%q", expected, got.StartupContent)
			}
			if strings.Contains(f.stderr.String(), "startup-content: session") {
				t.Fatal("runtime reuse logged a dirty startup")
			}
		})
	}
}

// A session must receive accepted fingerprint content or be restarted once
// that content is restored; sessions already running must not restart.
func TestCityRuntimeStartupContentRecovery(t *testing.T) {
	for _, shape := range []string{"scope_root", "new_workdir", "existing_workdir", "copied_directory"} {
		for _, change := range []string{"stray_file", "edited_skill", "unrelated_file"} {
			if shape == "copied_directory" && change != "edited_skill" {
				continue // the copied-directory story is an edited staged file
			}
			t.Run(shape+"/"+change, func(t *testing.T) {
				f := newCachedImportFixture(t, func(f *cachedImportFixture) {
					if shape == "copied_directory" {
						if err := os.Symlink(filepath.Join(f.cacheDirs[1], "scripts"), filepath.Join(f.cityPath, "scripts")); err != nil {
							t.Fatal(err)
						}
					}
				})
				t.Setenv("GC_CITY", f.cityPath)
				t.Chdir(f.cityPath)
				workdir := f.cityPath
				scopeRoot := shape == "scope_root" || shape == "copied_directory"
				if !scopeRoot {
					workdir = t.TempDir()
					f.cfg.Agents[0].WorkDir = workdir
				}
				materialize := func() error {
					config.ResetRemoteCacheValidationCache() // a fresh PreStart process
					cmd := newInternalMaterializeSkillsCmd(f.stdout, f.stderr)
					cmd.SetArgs([]string{"--best-effort", "--agent", "worker", "--workdir", workdir})
					return cmd.Execute()
				}
				if scopeRoot {
					if err := runStage1SkillMaterialization(f.cityPath, f.cfg, f.stderr); err != nil {
						t.Fatal(err)
					}
				} else if shape == "existing_workdir" {
					if err := materialize(); err != nil {
						t.Fatal(err)
					}
				}
				source := filepath.Join(f.cacheDirs[1], "skills", "plan")
				skill := filepath.Join(source, "SKILL.md")
				if shape == "copied_directory" {
					source = filepath.Join(f.cacheDirs[1], "scripts")
					skill = filepath.Join(source, "helper.sh")
				}
				accepted, err := os.ReadFile(skill)
				if err != nil {
					t.Fatal(err)
				}
				sink := filepath.Join(workdir, ".claude", "skills", "second.plan")
				if shape == "copied_directory" {
					sink = filepath.Join(workdir, ".gc", "scripts")
				}
				hasAccepted := func() bool {
					data, err := os.ReadFile(filepath.Join(sink, filepath.Base(skill)))
					entries, listErr := os.ReadDir(sink)
					return err == nil && listErr == nil && bytes.Equal(data, accepted) && len(entries) == 1
				}
				deliveredAccepted := false
				sp := &startupMaterializingRuntime{Fake: f.sp, prepare: func(cfg runtime.Config) error {
					if err := runtime.StageWorkDir(workdir, "", cfg.CopyFiles); err != nil {
						return err
					}
					if !scopeRoot {
						t.Setenv("GC_STARTUP_CONTENT_RECEIPT", cfg.Env["GC_STARTUP_CONTENT_RECEIPT"])
						if err := materialize(); err != nil {
							return err
						}
					}
					deliveredAccepted = hasAccepted()
					return nil
				}}
				f.cfg.NamedSessions = []config.NamedSession{{Name: "worker", Template: "worker", Mode: "always"}}
				if err := f.store.SetMetadataBatch(f.beadID, map[string]string{"configured_named_session": "true", "configured_named_identity": "worker", "pin_awake": "", "started_config_hash": "legacy"}); err != nil {
					t.Fatal(err)
				}
				clk := &clock.Fake{Time: time.Now()}
				drains := newDrainTracker()
				settle := func(n int) {
					t.Helper()
					for range n {
						rows, err := f.store.List(beads.ListQuery{AllowScan: true})
						if err != nil {
							t.Fatal(err)
						}
						desired := f.cr.buildFn(f.cfg, sp, f.store).State
						reconcileSessionBeadsAtPath(context.Background(), f.cityPath, rows, desired,
							map[string]bool{"worker": true}, f.cfg, sp, f.store, newDrainOps(sp), nil, nil, nil,
							drains, nil, false, nil, "test-city", nil, clk, events.Discard, time.Minute, time.Second,
							f.stdout, f.stderr, withStartStabilityWaiter(immediateStartStabilityWaiter),
							withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter))
						clk.Advance(time.Minute)
					}
				}
				// Settle the intentional workdir config change before the fault.
				settle(3)
				starts := f.sp.CountCalls("Start", "worker")
				changedPath, changedData := filepath.Join(source, "stray"), []byte("stray")
				switch change {
				case "edited_skill":
					changedPath, changedData = skill, []byte("---\nname: plan\ndescription: Changed\n---\nUnaccepted content.\n")
				case "unrelated_file":
					changedPath = filepath.Join(f.cacheDirs[1], "unrelated-file")
				}
				if err := os.WriteFile(changedPath, changedData, 0o644); err != nil {
					t.Fatal(err)
				}
				if shape == "scope_root" && change == "stray_file" {
					// Recovery of a runtime that was already running before the
					// dirty window cannot infer a delivery mismatch from today's disk.
					if err := f.store.SetMetadataBatch(f.beadID, map[string]string{"pending_create_claim": "true", "state": "creating"}); err != nil {
						t.Fatal(err)
					}
					if err := f.sp.SetMeta("worker", "GC_SESSION_ID", f.beadID); err != nil {
						t.Fatal(err)
					}
				}
				settle(2)
				if got := f.sp.CountCalls("Start", "worker"); got != starts {
					t.Fatalf("already-running session restarted during dirty window: %d -> %d", starts, got)
				}
				before, err := f.store.Get(f.beadID)
				if err != nil || before.Metadata["startup_content"] != "" {
					t.Fatalf("already-running session was marked by the dirty window: present=%t, %v", before.Metadata["startup_content"] != "", err)
				}
				if err := f.sp.Stop("worker"); err != nil {
					t.Fatal(err)
				}
				if (shape == "new_workdir" && change == "unrelated_file") || shape == "copied_directory" {
					// The supervisor can die after PreStart and spawn but before
					// commitStartResult. No pending-create claim is required for
					// waking a previously started session.
					info, err := sessionFrontDoor(f.store).Get(f.beadID)
					if err != nil {
						t.Fatal(err)
					}
					tp := f.cr.buildFn(f.cfg, sp, f.store).State["worker"]
					prepared, _, err := buildPreparedStart(startCandidate{info: info, tp: tp}, f.cfg, f.store)
					if err != nil {
						t.Fatal(err)
					}
					result := runPreparedStartCandidate(context.Background(), *prepared, f.cityPath, sp, f.store, f.cfg, time.Minute,
						immediateStartStabilityWaiter, immediateSessionStaleKeyDetectionWaiter, nil)
					if result.err != nil {
						t.Fatal(result.err)
					}
				} else {
					settle(3)
				}
				if got := f.sp.CountCalls("Start", "worker"); got != starts+1 {
					t.Fatalf("session did not start inside dirty window: got %d, want %d; %s", got, starts+1, f.stderr.String())
				}
				acceptedAtStart := deliveredAccepted
				wantRepair := !acceptedAtStart || !scopeRoot // a skipped PreStart is itself a repair obligation
				started, err := f.store.Get(f.beadID)
				if err != nil {
					t.Fatal(err)
				}
				if wantRepair && started.Metadata["startup_content"] == "" {
					t.Error("unaccepted delivery was not persisted on the session bead")
				}
				if change == "edited_skill" {
					err = os.WriteFile(skill, accepted, 0o644)
				} else {
					err = os.Remove(changedPath)
				}
				if err != nil {
					t.Fatal(err)
				}
				// Drop process-local snapshot and drain state, as a supervisor
				// restart does. Only the bead can carry the repair across it.
				fingerprintContents.Lock()
				delete(fingerprintContents.cities, f.cityPath)
				fingerprintContents.Unlock()
				drains = newDrainTracker()
				f.sp.Attached["worker"] = true
				settle(2)
				if got := f.sp.CountCalls("Start", "worker"); got != starts+1 {
					t.Fatalf("repair ignored attachment deferral: %d starts", got)
				}
				f.sp.Attached["worker"] = false
				clk.Advance(10 * time.Minute)
				settle(5)
				got := f.sp.CountCalls("Start", "worker")
				if wantRepair && got != starts+2 {
					t.Fatalf("session received unaccepted content but was not restarted exactly once after cleanup: starts=%d want=%d; %s", got, starts+2, f.stderr.String())
				}
				if !hasAccepted() {
					t.Fatal("recovered session lacks accepted pack skills")
				}
				wantMarks := 1
				if !wantRepair {
					wantMarks = 0
					if got != starts+1 {
						t.Errorf("accepted delivery restarted: got %d starts, want %d", got, starts+1)
					}
				}
				if got := strings.Count(f.stderr.String(), "startup-content: session"); got != wantMarks {
					t.Errorf("health lines = %d, want %d", got, wantMarks)
				}
				recovered, err := f.store.Get(f.beadID)
				if err != nil || recovered.Metadata["startup_content"] != "" {
					t.Fatalf("repair marker remained after restart: %v / %q", err, recovered.Metadata["startup_content"])
				}
				t.Logf("%s: accepted at dirty start=%v, recovery restarts=%d", fmt.Sprint(shape, "/", change), acceptedAtStart, got-starts-1)
			})
		}
	}
}
