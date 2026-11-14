package worker

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/testutil"
)

func installEntryTestBuilder(cfg *FactoryConfig, builder SessionRuntimeResolver) {
	cfg.BuildSessionLaunch = builder
}

func TestEntryLaunchBuildCostAndLockBoundary(t *testing.T) {
	store, sp := beads.NewMemStore(), runtime.NewFake()
	mgr := sessionpkg.NewManagerWithOptions(store, sp)
	info, err := mgr.CreateSession(t.Context(), sessionpkg.CreateOptions{Template: "runner", Provider: "codex", Command: "launch", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	builds := 0
	buildErr := errors.New("injected launch input failure")
	builder := SessionRuntimeResolver(func(sessionpkg.Info, string, map[string]string) (*ResolvedRuntime, error) {
		builds++
		locked := make(chan struct{})
		go func() { _ = sessionpkg.WithSessionMutationLock(info.ID, func() error { close(locked); return nil }) }()
		select {
		case <-locked:
		case <-time.After(testutil.GoroutineRaceTimeout):
			t.Error("entry builder holds the session lock")
		}
		return nil, buildErr
	})
	cfg := FactoryConfig{Store: store, Provider: sp}
	installEntryTestBuilder(&cfg, builder)
	factory, err := NewFactory(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h, err := factory.SessionByID(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Peek(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	if _, err := h.History(t.Context(), HistoryRequest{}); err != nil && !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatal(err)
	}
	if _, err := h.LiveObservation(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if builds != 0 {
		t.Fatalf("read/control operations made %d full builds", builds)
	}
	starts := sp.CountCalls("Start", info.SessionName)
	if err := h.Start(t.Context()); !errors.Is(err, buildErr) {
		t.Errorf("Start error=%v, want launch failure", err)
	}
	if builds != 1 || sp.CountCalls("Start", info.SessionName) != starts {
		t.Fatalf("builds=%d starts=%d", builds, sp.CountCalls("Start", info.SessionName))
	}
}

func TestEntryLaunchSelection(t *testing.T) {
	for _, tc := range []struct {
		name          string
		live, managed bool
		operation     string
		builds        int
	}{
		{"live-default", true, true, "message", 0},
		{"live-follow-up", true, true, "follow-up", 0},
		{"live-interrupt", true, true, "interrupt", 1},
		{"down-managed-message", false, true, "message", 0},
		{"down-managed-nudge", false, true, "nudge", 0},
		{"down-live-only", false, false, "live-only", 0},
		{"down-unmanaged-message", false, false, "message", 1},
		{"down-unmanaged-nudge", false, false, "nudge", 1},
		{"down-start", false, true, "start", 1},
		{"down-attach", false, true, "attach", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, sp := beads.NewMemStore(), runtime.NewFake()
			mgr := sessionpkg.NewManagerWithOptions(store, sp)
			info, err := mgr.CreateSession(t.Context(), sessionpkg.CreateOptions{BeadOnly: !tc.live, Template: "runner", Provider: "codex", Command: "cheap", WorkDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			if !tc.live {
				if err := store.SetMetadata(info.ID, "state", "active"); err != nil {
					t.Fatal(err)
				}
			}
			builds := 0
			cfg := FactoryConfig{Store: store, Provider: sp, CityPath: t.TempDir(), ControllerRunning: func() bool { return tc.managed }}
			installEntryTestBuilder(&cfg, func(sessionpkg.Info, string, map[string]string) (*ResolvedRuntime, error) {
				builds++
				return &ResolvedRuntime{Command: "full", Provider: "codex", Hints: runtime.Config{Command: "full", Env: map[string]string{"ENTRY_LAUNCH": "yes"}}}, nil
			})
			factory, err := NewFactory(cfg)
			if err != nil {
				t.Fatal(err)
			}
			h, err := factory.SessionByID(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch tc.operation {
			case "start":
				err = h.Start(t.Context())
			case "attach":
				err = h.Attach(t.Context())
			case "message", "follow-up", "interrupt":
				intent := DeliveryIntentDefault
				if tc.operation == "follow-up" {
					intent = DeliveryIntentFollowUp
				}
				if tc.operation == "interrupt" {
					intent = DeliveryIntentInterruptNow
					sp.InterruptBoundaryErrors[info.SessionName] = errors.New("injected boundary failure")
				}
				_, err = h.Message(t.Context(), MessageRequest{Text: "hello", Delivery: intent})
			case "nudge", "live-only":
				req := NudgeRequest{Text: "hello"}
				if tc.operation == "live-only" {
					req.Wake = NudgeWakeLiveOnly
				}
				_, err = h.Nudge(t.Context(), req)
			}
			if err != nil {
				t.Fatal(err)
			}
			if builds != tc.builds {
				t.Fatalf("builds=%d want=%d", builds, tc.builds)
			}
			if tc.builds > 0 {
				launch := sp.LastStartConfig(info.SessionName)
				if launch == nil || launch.Env["ENTRY_LAUNCH"] != "yes" {
					t.Fatal("full launch was not used")
				}
			}
		})
	}
}

func TestEntryLaunchSkippedWhileControllerOwnsRestart(t *testing.T) {
	for _, operation := range []string{"start", "attach"} {
		t.Run(operation, func(t *testing.T) {
			store, sp := beads.NewMemStore(), runtime.NewFake()
			mgr := sessionpkg.NewManagerWithOptions(store, sp)
			info, err := mgr.CreateSession(t.Context(), sessionpkg.CreateOptions{BeadOnly: true, Template: "runner", Command: "cheap", ExtraMeta: map[string]string{sessionpkg.NamedSessionMetadataKey: "true", "restart_requested": "true"}})
			if err != nil {
				t.Fatal(err)
			}
			builds := 0
			factory, err := NewFactory(FactoryConfig{Store: store, Provider: sp, ControllerRunning: func() bool { return true }, BuildSessionLaunch: func(sessionpkg.Info, string, map[string]string) (*ResolvedRuntime, error) {
				builds++
				return nil, errors.New("launch inputs must not be read while waiting")
			}})
			if err != nil {
				t.Fatal(err)
			}
			h, err := factory.SessionByID(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if operation == "start" {
				err = h.Start(ctx)
			} else {
				err = h.Attach(ctx)
			}
			if !errors.Is(err, context.Canceled) || builds != 0 || sp.CountCalls("Start", info.SessionName) != 0 {
				t.Fatalf("error=%v builds=%d starts=%d", err, builds, sp.CountCalls("Start", info.SessionName))
			}
		})
	}
}

func TestControllerOwnedEntryWaitsThroughPreparation(t *testing.T) {
	for _, operation := range []string{"start", "attach"} {
		for _, row := range []string{"fresh-drained", "drift-asleep", "healed-runtime-missing"} {
			for _, preparation := range []string{"after-wake", "before-entry"} {
				t.Run(operation+"/"+row+"/"+preparation, func(t *testing.T) {
					cityPath := t.TempDir()
					synctest.Test(t, func(t *testing.T) {
						store, sp := beads.NewMemStore(), runtime.NewFake()
						mgr := sessionpkg.NewManagerWithOptions(store, sp)
						info, err := mgr.CreateSession(t.Context(), sessionpkg.CreateOptions{
							BeadOnly: true, Template: "runner", Command: "caller-launch",
							ExtraMeta: map[string]string{sessionpkg.NamedSessionMetadataKey: "true"},
						})
						if err != nil {
							t.Fatal(err)
						}
						patch := sessionpkg.AcknowledgeDrainPatch(time.Now(), true)
						switch row {
						case "drift-asleep":
							patch = sessionpkg.ConfigDriftResetPatch(sessionpkg.StateAsleep, "", time.Now())
						case "healed-runtime-missing":
							patch = sessionpkg.MetadataPatch{"state": "asleep", "sleep_reason": "runtime-missing", "continuation_reset_pending": "true"}
						}
						if err := store.SetMetadataBatch(info.ID, patch); err != nil {
							t.Fatal(err)
						}
						prepare := func() {
							t.Helper()
							// Match the controller's lock boundary: PreWakePatch clears
							// reset markers before launch preparation outside the lock.
							if err := sessionpkg.WithSessionMutationLock(info.ID, func() error {
								return store.SetMetadataBatch(info.ID, sessionpkg.PreWakePatch(sessionpkg.PreWakePatchInput{
									Generation: 2, InstanceToken: "controller-incarnation", ContinuationEpoch: 2,
									Now: time.Now(), FreshWake: true,
								}))
							}); err != nil {
								t.Fatal(err)
							}
						}
						if preparation == "before-entry" {
							prepare()
						}
						builds, pokes := 0, 0
						poked := make(chan struct{}, 1)
						factory, err := NewFactory(FactoryConfig{
							Store: store, Provider: sp, CityPath: cityPath,
							ControllerRunning: func() bool { return true },
							PokeController: func() error {
								pokes++
								select {
								case poked <- struct{}{}:
								default:
								}
								return nil
							},
							BuildSessionLaunch: func(sessionpkg.Info, string, map[string]string) (*ResolvedRuntime, error) {
								builds++
								return nil, errors.New("controller-owned entry must not build a caller launch")
							},
						})
						if err != nil {
							t.Fatal(err)
						}
						h, err := factory.SessionByID(info.ID)
						if err != nil {
							t.Fatal(err)
						}
						done := make(chan error, 1)
						go func() {
							if operation == "start" {
								done <- h.Start(t.Context())
							} else {
								done <- h.Attach(t.Context())
							}
						}()
						synctest.Wait()
						if preparation == "after-wake" {
							prepare()
						}
						assertWaiting := func() {
							t.Helper()
							// Observe a later poll's poke without a fixed sleep.
							select {
							case <-poked:
							default:
							}
							select {
							case <-poked:
							case err := <-done:
								t.Fatalf("entry returned before controller startup: %v (caller builds=%d starts=%d)", err, builds, sp.CountCalls("Start", info.SessionName))
							}
							synctest.Wait()
							select {
							case err := <-done:
								t.Fatalf("entry returned before controller startup: %v (caller builds=%d starts=%d)", err, builds, sp.CountCalls("Start", info.SessionName))
							default:
							}
							if builds != 0 || sp.CountCalls("Start", info.SessionName) != 0 || pokes == 0 {
								t.Fatalf("caller builds=%d starts=%d pokes=%d", builds, sp.CountCalls("Start", info.SessionName), pokes)
							}
						}
						assertWaiting()
						// Even a failed preparation/retry must not transfer ownership
						// back to an entry that deliberately retained cheap inputs.
						if err := store.SetMetadata(info.ID, "state", "asleep"); err != nil {
							t.Fatal(err)
						}
						assertWaiting()
						if err := sp.Start(t.Context(), info.SessionName, runtime.Config{Command: "controller-launch"}); err != nil {
							t.Fatal(err)
						}
						if err := store.SetMetadata(info.ID, "state", "active"); err != nil {
							t.Fatal(err)
						}
						if err := <-done; err != nil {
							t.Fatal(err)
						}
						if sp.CountCalls("Start", info.SessionName) != 1 || builds != 0 {
							t.Fatalf("expected only the controller start; starts=%d caller builds=%d", sp.CountCalls("Start", info.SessionName), builds)
						}
					})
				})
			}
		}
	}
}

func TestEntryLaunchCreateRetainsSuppliedInputs(t *testing.T) {
	for _, mode := range []CreateMode{CreateModeStarted, CreateModeDeferred} {
		t.Run(string(mode), func(t *testing.T) {
			store, sp := beads.NewMemStore(), runtime.NewFake()
			builds := 0
			factory, err := NewFactory(FactoryConfig{Store: store, Provider: sp, BuildSessionLaunch: func(sessionpkg.Info, string, map[string]string) (*ResolvedRuntime, error) {
				builds++
				return nil, errors.New("unexpected full launch build on create")
			}})
			if err != nil {
				t.Fatal(err)
			}
			h, err := factory.Session(SessionSpec{Template: "test-provider", Provider: "test-provider", Command: "launch", WorkDir: t.TempDir(), Env: map[string]string{"CREATE_INPUT": "preserved"}, Hints: runtime.Config{SessionSetup: []string{"supplied-setup"}}})
			if err != nil {
				t.Fatal(err)
			}
			info, err := h.Create(t.Context(), mode)
			if err != nil {
				t.Fatal(err)
			}
			if builds != 0 {
				t.Fatalf("create made %d full builds", builds)
			}
			if mode == CreateModeStarted {
				launch := sp.LastStartConfig(info.SessionName)
				if launch == nil || launch.Env["CREATE_INPUT"] != "preserved" || len(launch.SessionSetup) != 1 || launch.SessionSetup[0] != "supplied-setup" {
					t.Fatal("create inputs were replaced")
				}
			}
		})
	}
}
