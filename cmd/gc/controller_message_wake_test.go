package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

type controllerMessageWakeFixture struct {
	cityPath string
	cfg      *config.City
	store    *beads.MemStore
	sp       *runtime.Fake
	events   *providerMessageResultEvents
	server   *api.Server
	info     session.Info
	starts   int
}

func newControllerMessageWakeFixture(t *testing.T, kind string) *controllerMessageWakeFixture {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	f := &controllerMessageWakeFixture{cityPath: t.TempDir(), store: beads.NewMemStore(), sp: runtime.NewFake()}
	text := "[workspace]\nname = \"wake-city\"\n[session]\nprovider = \"fake\"\n[providers.catalog-provider]\ncommand = \"echo\"\nprompt_mode = \"none\"\n"
	if kind != "manual" {
		text += "[[agent]]\nname = \"runner\"\nprovider = \"catalog-provider\"\n[[named_session]]\ntemplate = \"runner\"\nmode = \"" + kind + "\"\n"
	}
	path := filepath.Join(f.cityPath, "city.toml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	var err error
	f.cfg, _, err = config.LoadWithIncludes(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	f.events = &providerMessageResultEvents{Fake: events.NewFake(), done: make(chan events.Event, 4)}
	cs := newControllerState(t.Context(), f.cfg, f.sp, f.events, "wake-city", f.cityPath)
	cs.cityBeadStore, cs.pokeCh = f.store, make(chan struct{}, 4)
	f.server = api.New(cs)
	var id string
	if kind == "manual" {
		ev := f.post(t, "/sessions", `{"kind":"provider","name":"catalog-provider","alias":"manual-chat"}`, events.RequestResultSessionCreate)
		var created api.SessionCreateSucceededPayload
		if err := json.Unmarshal(ev.Payload, &created); err != nil {
			t.Fatal(err)
		}
		id = created.Session.ID
	} else {
		f.post(t, "/session/runner/submit", `{"message":"setup"}`, events.RequestResultSessionSubmit)
		f.reconcile(t)
		all, err := f.store.List(beads.ListQuery{Label: session.LabelSession})
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range all {
			if b.Metadata["template"] == "runner" && b.Status != "closed" {
				id = b.ID
			}
		}
	}
	m := session.NewManagerWithOptions(f.store, f.sp, session.WithCityPath(f.cityPath))
	f.info, err = m.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if !f.sp.IsRunning(f.info.SessionName) {
		t.Fatal("setup did not start the session")
	}
	if err := f.sp.Stop(f.info.SessionName); err != nil {
		t.Fatal(err)
	}
	if err := nudgequeue.WithState(f.cityPath, func(s *nudgequeue.State) error {
		s.Pending, s.InFlight = nil, nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.starts = f.sp.CountCalls("Start", f.info.SessionName)
	return f
}

func (f *controllerMessageWakeFixture) post(t *testing.T, path, body, resultType string) events.Event {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/city/wake-city"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	f.server.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST %s: %d %s", path, rec.Code, rec.Body.String())
	}
	return f.events.next(t, resultType)
}

func (f *controllerMessageWakeFixture) reconcile(t *testing.T) {
	t.Helper()
	all, err := f.store.List(beads.ListQuery{Label: session.LabelSession})
	if err != nil {
		t.Fatal(err)
	}
	desired := buildDesiredState("wake-city", f.cityPath, time.Now().UTC(), f.cfg, f.sp, f.store, io.Discard)
	cr := &CityRuntime{
		cityPath: f.cityPath, cityName: "wake-city", cfg: f.cfg, sp: f.sp,
		standaloneCityStore: f.store, sessionDrains: newDrainTracker(),
		rec: f.events, stdout: io.Discard, stderr: io.Discard,
	}
	cr.beadReconcileTick(t.Context(), desired, newSessionBeadSnapshot(all), nil, false)
	if !cr.asyncStarts.wait(hangBudget) {
		t.Fatal("controller start did not settle")
	}
}

func (f *controllerMessageWakeFixture) setRow(t *testing.T, row string) {
	t.Helper()
	now := time.Now().UTC()
	var patch session.MetadataPatch
	switch row {
	case "idle-asleep":
		patch = session.SleepPatch(now, "idle")
	case "fresh-drained", "drained-restart":
		patch = session.AcknowledgeDrainPatch(now, true)
	case "abandoned-creating":
		f.setRow(t, "fresh-drained")
		patch = session.PreWakePatch(session.PreWakePatchInput{
			Generation: 2, InstanceToken: "abandoned-preparation", ContinuationEpoch: 2,
			Now: now.Add(-staleCreatingStateTimeout - time.Minute), FreshWake: true,
		})
	case "held-restart", "wait-held-restart", "quarantined-restart":
		patch = session.SleepPatch(now, "idle")
		switch row {
		case "held-restart":
			patch["held_until"] = now.Add(time.Hour).Format(time.RFC3339)
		case "wait-held-restart":
			patch["wait_hold"] = "true"
		case "quarantined-restart":
			patch["quarantined_until"] = now.Add(time.Hour).Format(time.RFC3339)
		}
	case "drift-asleep":
		patch = session.ConfigDriftResetPatch(session.StateAsleep, "", now)
	case "healed-runtime-missing":
		patch = session.MetadataPatch{"state": "asleep", "sleep_reason": "runtime-missing", "session_key": "", "started_config_hash": "", "continuation_reset_pending": "true"}
	case "crashed-active", "crashed-awake":
		patch = session.MetadataPatch{"state": strings.TrimPrefix(row, "crashed-"), "sleep_reason": "", "last_woke_at": now.Add(-10 * time.Minute).Format(time.RFC3339)}
	case "restart-requested":
		patch = session.MetadataPatch{"state": "active", "restart_requested": "true"}
	case "handoff-requested":
		patch = session.MetadataPatch{"state": "active", "restart_requested": "true", "continuation_reset_pending": "true"}
	case "committed-reset":
		patch = session.RestartRequestPatch("", now)
	default:
		t.Fatalf("unknown row %q", row)
	}
	if err := f.store.SetMetadataBatch(f.info.ID, patch); err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(row, "-restart") {
		m := session.NewManagerWithOptions(f.store, f.sp, session.WithCityPath(f.cityPath))
		if err := m.RequestFreshRestart(f.info.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeferredControllerMessageStartsAndDeliversOnce(t *testing.T) {
	for _, kind := range []string{"on_demand", "always", "manual"} {
		for _, row := range []string{"idle-asleep", "fresh-drained", "drift-asleep", "healed-runtime-missing", "crashed-active", "crashed-awake", "restart-requested", "handoff-requested", "committed-reset", "drained-restart", "held-restart", "wait-held-restart", "quarantined-restart", "abandoned-creating"} {
			t.Run(kind+"/"+row, func(t *testing.T) {
				f := newControllerMessageWakeFixture(t, kind)
				f.setRow(t, row)
				ev := f.post(t, "/session/"+f.info.ID+"/submit", `{"message":"wake-marker","intent":"follow_up"}`, events.RequestResultSessionSubmit)
				var submitted api.SessionSubmitSucceededPayload
				if err := json.Unmarshal(ev.Payload, &submitted); err != nil {
					t.Fatal(err)
				}
				if !submitted.Queued || f.sp.CountCalls("Start", f.info.SessionName) != f.starts {
					t.Fatal("message must queue without a caller start")
				}
				queued, err := nudgequeue.LoadState(f.cityPath)
				if err != nil || len(queued.Pending) != 1 || queued.Pending[0].Message != "wake-marker" {
					t.Fatalf("expected exactly one queued message: err=%v queue=%+v", err, queued)
				}
				before, err := f.store.Get(f.info.ID)
				if err != nil {
					t.Fatal(err)
				}
				if strings.HasSuffix(row, "-restart") && before.Metadata["restart_requested"] != "true" {
					t.Fatal("message canceled the controller's restart request")
				}
				for pass := 0; pass < 3; pass++ {
					f.reconcile(t)
				}
				after, err := f.store.Get(f.info.ID)
				if err != nil {
					t.Fatal(err)
				}
				if after.Metadata["restart_requested"] == "true" {
					t.Fatal("controller did not consume the restart request")
				}
				starts := f.sp.CountCalls("Start", f.info.SessionName) - f.starts
				t.Logf("wake=%q starts=%d running=%v epoch=%s->%s", before.Metadata["wake_request"], starts, f.sp.IsRunning(f.info.SessionName), before.Metadata["continuation_epoch"], after.Metadata["continuation_epoch"])
				if starts != 1 || !f.sp.IsRunning(f.info.SessionName) {
					t.Fatalf("controller starts=%d running=%v, want one running replacement", starts, f.sp.IsRunning(f.info.SessionName))
				}
				if before.Metadata["continuation_reset_pending"] != "" && (after.Metadata["continuation_reset_pending"] != "" || after.Metadata["continuation_epoch"] == before.Metadata["continuation_epoch"]) {
					t.Fatal("replacement did not consume the fresh conversation reset")
				}
				idleSince := time.Now().Add(-time.Minute)
				f.sp.Activity = map[string]time.Time{f.info.SessionName: idleSince}
				target := nudgeTarget{
					cityPath: f.cityPath, agent: config.Agent{Name: "runner"}, sessionID: f.info.ID, sessionName: f.info.SessionName, alias: f.info.Alias,
					continuationEpoch: after.Metadata["continuation_epoch"], resolved: &config.ResolvedProvider{Name: "codex"},
				}
				nudges := f.sp.CountCalls("Nudge", f.info.SessionName)
				for attempt := 0; attempt < 2; attempt++ {
					delivered, err := tryDeliverQueuedNudgesByPoller(target, f.store, f.store, f.sp, time.Second, worker.LiveObservation{Running: true, LastActivity: &idleSince})
					if err != nil || delivered != (attempt == 0) {
						t.Fatalf("delivery attempt %d: delivered=%v err=%v", attempt, delivered, err)
					}
				}
				q, err := nudgequeue.LoadState(f.cityPath)
				if err != nil {
					t.Fatal(err)
				}
				if len(q.Pending)+len(q.InFlight)+len(q.Dead) != 0 || f.sp.CountCalls("Nudge", f.info.SessionName)-nudges != 1 {
					t.Fatal("message was not delivered exactly once with an empty queue")
				}
				occurrences := 0
				for _, call := range f.sp.SnapshotCalls() {
					if call.Method == "Nudge" && call.Name == f.info.SessionName {
						occurrences += strings.Count(call.Message, "wake-marker")
					}
				}
				if occurrences != 1 {
					t.Fatalf("delivered marker %d times", occurrences)
				}
			})
		}
	}
}

func TestWorkerEntryRetainsControllerOwnershipAcrossHeal(t *testing.T) {
	for _, operation := range []string{"start", "attach"} {
		t.Run(operation, func(t *testing.T) {
			f := newControllerMessageWakeFixture(t, "on_demand")
			f.setRow(t, "fresh-drained")
			if err := f.store.SetMetadataBatch(f.info.ID, session.PreWakePatch(session.PreWakePatchInput{
				Generation: 2, InstanceToken: "abandoned-preparation", ContinuationEpoch: 2,
				Now: time.Now().Add(-staleCreatingStateTimeout - time.Minute), FreshWake: true,
			})); err != nil {
				t.Fatal(err)
			}
			armed, healed := false, false
			builds, pokes := 0, 0
			factory, err := worker.NewFactory(worker.FactoryConfig{
				Store: f.store, Provider: f.sp, CityPath: f.cityPath,
				ControllerRunning: func() bool {
					if armed && !healed {
						healed = true
						// The hint has read creating, but the manager has not
						// entered its first locked operation. Run the real heal.
						f.reconcile(t)
						b, err := f.store.Get(f.info.ID)
						if err != nil || b.Metadata["state"] != "asleep" || b.Metadata["continuation_reset_pending"] != "" || b.Metadata["pending_create_claim"] != "" || b.Metadata["wake_request"] != "" {
							t.Fatalf("stale preparation did not heal to undemanded asleep: err=%v metadata=%v", err, b.Metadata)
						}
						if f.sp.CountCalls("Start", f.info.SessionName) != f.starts {
							t.Fatal("fixture has independent startup demand")
						}
					}
					return true
				},
				PokeController: func() error {
					pokes++
					f.reconcile(t)
					return nil
				},
				BuildSessionLaunch: func(session.Info, string, map[string]string) (*worker.ResolvedRuntime, error) {
					builds++
					return nil, errors.New("controller-owned entry must not build a caller launch")
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			h, err := factory.SessionByID(f.info.ID)
			if err != nil {
				t.Fatal(err)
			}
			armed = true
			ctx, cancel := context.WithTimeout(t.Context(), hangBudget)
			defer cancel()
			if operation == "start" {
				err = h.Start(ctx)
			} else {
				err = h.Attach(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			f.reconcile(t)
			if !healed || builds != 0 || pokes == 0 || f.sp.CountCalls("Start", f.info.SessionName)-f.starts != 1 {
				t.Fatalf("healed=%v builds=%d pokes=%d starts=%d; want exactly one controller start", healed, builds, pokes, f.sp.CountCalls("Start", f.info.SessionName)-f.starts)
			}
		})
	}
}

func TestStartAndAttachWakeAfterKillFence(t *testing.T) {
	for _, operation := range []string{"start", "attach"} {
		for _, completion := range []string{"cleared", "expired"} {
			t.Run(operation+"/"+completion, func(t *testing.T) {
				f := newControllerMessageWakeFixture(t, "on_demand")
				if err := f.sp.Start(t.Context(), f.info.SessionName, runtime.Config{Command: "outgoing"}); err != nil {
					t.Fatal(err)
				}
				f.starts = f.sp.CountCalls("Start", f.info.SessionName)
				if err := f.store.SetMetadataBatch(f.info.ID, session.KillPendingPatch(time.Now())); err != nil {
					t.Fatal(err)
				}
				pokes := 0
				m := session.NewManagerWithOptions(f.store, f.sp, session.WithCityPath(f.cityPath), session.WithRestartCoordinator(func() bool { return true }, func() error {
					pokes++
					b, err := f.store.Get(f.info.ID)
					if err != nil {
						return err
					}
					if pokes == 1 {
						if b.Metadata["wake_request"] != "" || f.sp.CountCalls("Start", f.info.SessionName) != f.starts {
							t.Fatal("entry must respect the active kill fence")
						}
						if err := f.sp.Stop(f.info.SessionName); err != nil {
							return err
						}
						if completion == "cleared" {
							return f.store.SetMetadata(f.info.ID, "state_reason", "")
						}
						return f.store.SetMetadata(f.info.ID, "slept_at", time.Now().Add(-session.KillPendingGrace-time.Second).Format(time.RFC3339))
					}
					if b.Metadata["wake_request"] != "explicit" {
						t.Fatal("controller-owned wait has no wake after the kill fence ended")
					}
					f.reconcile(t)
					return nil
				}))
				ctx, cancel := context.WithTimeout(t.Context(), hangBudget)
				defer cancel()
				var err error
				if operation == "start" {
					err = m.Start(ctx, f.info.ID, "caller-launch", runtime.Config{})
				} else {
					err = m.Attach(ctx, f.info.ID, "caller-launch", runtime.Config{})
				}
				if err != nil {
					t.Fatal(err)
				}
				f.reconcile(t)
				if pokes < 2 || f.sp.CountCalls("Start", f.info.SessionName)-f.starts != 1 || f.sp.LastStartConfig(f.info.SessionName).Command == "caller-launch" {
					t.Fatal("expected exactly one controller start after the kill fence ended")
				}
			})
		}
	}
}

func TestStartAndAttachWakeFreshDrainedNamedSession(t *testing.T) {
	for _, operation := range []string{"start", "attach"} {
		t.Run(operation, func(t *testing.T) {
			f := newControllerMessageWakeFixture(t, "on_demand")
			f.setRow(t, "fresh-drained")
			m := session.NewManagerWithOptions(f.store, f.sp, session.WithCityPath(f.cityPath), session.WithRestartCoordinator(func() bool { return true }, func() error {
				b, err := f.store.Get(f.info.ID)
				if err != nil || b.Metadata["wake_request"] != "explicit" {
					t.Fatalf("fresh-drained start has no explicit wake: err=%v", err)
				}
				f.reconcile(t)
				return nil
			}))
			ctx, cancel := context.WithTimeout(t.Context(), hangBudget)
			defer cancel()
			var err error
			if operation == "start" {
				err = m.Start(ctx, f.info.ID, "caller-launch", runtime.Config{})
			} else {
				err = m.Attach(ctx, f.info.ID, "caller-launch", runtime.Config{})
			}
			if err != nil {
				t.Fatal(err)
			}
			f.reconcile(t)
			if f.sp.CountCalls("Start", f.info.SessionName)-f.starts != 1 || f.sp.LastStartConfig(f.info.SessionName).Command == "caller-launch" {
				t.Fatal("expected exactly one controller start")
			}
		})
	}
}

// A successful start keeps a wake that arrived during startup. One extra start
// after drain lets a later message reach a running session.
func TestStartCommitKeepsWakeMadeDuringStart(t *testing.T) {
	for _, operation := range []string{"none", "message", "start", "attach", "wake", "after_commit", "sync", "manager_confirms", "failed", "abandoned"} {
		t.Run(operation, func(t *testing.T) {
			f := newControllerMessageWakeFixture(t, "on_demand")
			f.setRow(t, "fresh-drained")
			m := session.NewManagerWithOptions(f.store, f.sp, session.WithCityPath(f.cityPath))
			if _, err := m.PersistedStore().WakeSession(f.info.ID, time.Now(), session.WakeOpts{}); err != nil {
				t.Fatal(err)
			}
			desired := buildDesiredState("wake-city", f.cityPath, time.Now().UTC(), f.cfg, f.sp, f.store, io.Discard)
			tp, ok := desired.State[f.info.SessionName]
			if !ok {
				t.Fatal("initial wake did not select the configured session")
			}
			info, err := m.Get(f.info.ID)
			if err != nil {
				t.Fatal(err)
			}
			var prepareClock clock.Clock = clock.Real{}
			if operation == "abandoned" {
				prepareClock = &clock.Fake{Time: time.Now().Add(-2 * time.Minute)}
			}
			prepared, err := prepareStartCandidateForCity(startCandidate{info: info, tp: tp, configured: true}, f.cityPath, "wake-city", f.cfg, f.sp, f.store, prepareClock, io.Discard, nil)
			if err != nil {
				t.Fatal(err)
			}
			b, err := f.store.Get(f.info.ID)
			if err != nil || b.Metadata["state"] != "creating" || b.Metadata["wake_request"] != "" {
				t.Fatalf("prepare did not reach the consumed-wake boundary: err=%v metadata=%v", err, b.Metadata)
			}
			finished := false
			finishStart := func() error {
				if finished {
					return nil
				}
				finished = true
				result := runPreparedStartCandidate(t.Context(), *prepared, f.cityPath, f.sp, f.store, f.cfg, time.Minute, func(context.Context, string) bool { return true }, nil, nil)
				if result.err != nil {
					return result.err
				}
				if operation == "manager_confirms" {
					if _, err := m.SendImmediate(t.Context(), f.info.ID, "live", "", runtime.Config{}); err != nil {
						return err
					}
					current, err := m.Get(f.info.ID)
					if err != nil {
						return err
					}
					if current.MetadataState != "active" || current.PendingCreateClaim {
						t.Fatal("manager did not confirm the running session")
					}
				}
				if operation == "sync" {
					if !commitStartResultTraced(result, sessionFrontDoor(f.store), clock.Real{}, f.events, 0, io.Discard, io.Discard, nil) {
						return errors.New("sync commit rejected")
					}
					return nil
				}
				if !commitAsyncStartResultWithContext(t.Context(), result, f.sp, f.store, clock.Real{}, f.events, 0, io.Discard, io.Discard, nil) {
					return errors.New("real controller startup commit was rejected")
				}
				return nil
			}
			switch operation {
			case "message":
				f.post(t, "/session/"+f.info.ID+"/submit", `{"message":"wake-marker","intent":"follow_up"}`, events.RequestResultSessionSubmit)
			case "wake", "manager_confirms", "sync", "failed":
				recordTestWake(t, f, m, clock.Real{})
				if operation == "failed" {
					f.sp.StartErrors[f.info.SessionName] = errors.New("injected start failure")
					result := runPreparedStartCandidate(t.Context(), *prepared, f.cityPath, f.sp, f.store, f.cfg, time.Minute, func(context.Context, string) bool { return true }, nil, nil)
					if result.err == nil || commitAsyncStartResultWithContext(t.Context(), result, f.sp, f.store, clock.Real{}, f.events, 0, io.Discard, io.Discard, nil) {
						t.Fatal("failed provider start was accepted")
					}
					assertTestWake(t, f, "explicit")
					return
				}
			case "abandoned":
				f.post(t, "/session/"+f.info.ID+"/submit", `{"message":"wake-marker","intent":"follow_up"}`, events.RequestResultSessionSubmit)
				current, err := m.Get(f.info.ID)
				if err != nil {
					t.Fatal(err)
				}
				_, err = healStateWithRollbackInfo(current, false, true, sessionFrontDoor(f.store), clock.Real{}, time.Minute, false)
				if err != nil {
					t.Fatal(err)
				}
				assertTestWake(t, f, "explicit")
				for range 3 {
					f.reconcile(t)
				}
				finished = true
			case "start", "attach":
				waiting := session.NewManagerWithOptions(f.store, f.sp, session.WithCityPath(f.cityPath), session.WithRestartCoordinator(func() bool { return true }, finishStart))
				if operation == "start" {
					err = waiting.Start(t.Context(), f.info.ID, "caller-launch", runtime.Config{})
				} else {
					err = waiting.Attach(t.Context(), f.info.ID, "caller-launch", runtime.Config{})
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := finishStart(); err != nil {
				t.Fatal(err)
			}
			if f.sp.CountCalls("Start", f.info.SessionName)-f.starts != 1 {
				t.Fatal("expected exactly one initial controller start")
			}
			b, err = f.store.Get(f.info.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("wake after real successful startup: %q", b.Metadata["wake_request"])
			if operation == "after_commit" {
				recordTestWake(t, f, m, clock.Real{})
				assertStartsAfterDrain(t, f, 2)
				return
			}
			keepWake := operation != "none" && operation != "abandoned"
			if keepWake {
				assertTestWake(t, f, "explicit")
			} else {
				assertTestWake(t, f, "")
			}
			if operation == "message" || operation == "abandoned" {
				idleSince := time.Now().Add(-time.Minute)
				f.sp.Activity = map[string]time.Time{f.info.SessionName: idleSince}
				target := nudgeTarget{cityPath: f.cityPath, agent: config.Agent{Name: "runner"}, sessionID: f.info.ID, sessionName: f.info.SessionName, alias: f.info.Alias, continuationEpoch: b.Metadata["continuation_epoch"], resolved: &config.ResolvedProvider{Name: "codex"}}
				nudges := f.sp.CountCalls("Nudge", f.info.SessionName)
				delivered, err := tryDeliverQueuedNudgesByPoller(target, f.store, f.store, f.sp, time.Second, worker.LiveObservation{Running: true, LastActivity: &idleSince})
				if err != nil || !delivered || f.sp.CountCalls("Nudge", f.info.SessionName) != nudges+1 {
					t.Fatalf("message delivery: delivered=%v err=%v", delivered, err)
				}
			}
			q, err := nudgequeue.LoadState(f.cityPath)
			if err != nil || len(q.Pending)+len(q.InFlight)+len(q.Dead) != 0 {
				t.Fatalf("queue not empty after startup/delivery: err=%v queue=%+v", err, q)
			}
			if keepWake {
				assertStartsAfterDrain(t, f, 2)
				return
			}
			if err := f.store.SetMetadataBatch(f.info.ID, session.AcknowledgeDrainPatch(time.Now(), true)); err != nil {
				t.Fatal(err)
			}
			if err := f.sp.Stop(f.info.SessionName); err != nil {
				t.Fatal(err)
			}
			for pass := 0; pass < 3; pass++ {
				f.reconcile(t)
			}
			starts := f.sp.CountCalls("Start", f.info.SessionName) - f.starts
			if starts != 1 || f.sp.IsRunning(f.info.SessionName) {
				t.Fatalf("successful preparation left reusable wake demand: starts=%d running=%v; want one start and drained", starts, f.sp.IsRunning(f.info.SessionName))
			}
		})
	}
}

func preparePendingWakeTest(t *testing.T) (*controllerMessageWakeFixture, *session.Manager, *clock.Fake, *preparedStart) {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	f := &controllerMessageWakeFixture{cityPath: t.TempDir(), store: beads.NewMemStore(), sp: runtime.NewFake()}
	text := "[workspace]\nname = \"wake-city\"\n[session]\nprovider = \"fake\"\n[providers.catalog-provider]\ncommand = \"echo\"\nprompt_mode = \"none\"\n[[agent]]\nname = \"runner\"\nprovider = \"catalog-provider\"\n[[named_session]]\ntemplate = \"runner\"\nmode = \"on_demand\"\n"
	path := filepath.Join(f.cityPath, "city.toml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	var err error
	f.cfg, _, err = config.LoadWithIncludes(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	f.events = &providerMessageResultEvents{Fake: events.NewFake(), done: make(chan events.Event, 4)}
	cs := newControllerState(t.Context(), f.cfg, f.sp, f.events, "wake-city", f.cityPath)
	cs.cityBeadStore, cs.pokeCh = f.store, make(chan struct{}, 4)
	f.server = api.New(cs)
	f.post(t, "/session/runner/submit", `{"message":"setup"}`, events.RequestResultSessionSubmit)
	all, err := f.store.List(beads.ListQuery{Label: session.LabelSession})
	if err != nil {
		t.Fatal(err)
	}
	m := session.NewManagerWithOptions(f.store, f.sp, session.WithCityPath(f.cityPath))
	for _, b := range all {
		if b.Metadata["template"] == "runner" && b.Status != "closed" {
			f.info, err = m.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if f.info.ID == "" || !f.info.PendingCreateClaim {
		t.Fatalf("API did not create a claimed pending session: %+v", f.info)
	}
	clk := &clock.Fake{Time: time.Now().UTC()}
	desired := buildDesiredState("wake-city", f.cityPath, clk.Now(), f.cfg, f.sp, f.store, io.Discard)
	tp, ok := desired.State[f.info.SessionName]
	if !ok {
		t.Fatal("pending create absent from desired state")
	}
	prepared, err := prepareStartCandidateForCity(startCandidate{info: f.info, tp: tp, configured: true}, f.cityPath, "wake-city", f.cfg, f.sp, f.store, clk, io.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}

	return f, m, clk, prepared
}

func launchPendingWakeTest(t *testing.T, f *controllerMessageWakeFixture, _ *clock.Fake, prepared *preparedStart) startResult {
	t.Helper()
	result := runPreparedStartCandidate(t.Context(), *prepared, f.cityPath, f.sp, f.store, f.cfg, time.Minute, func(context.Context, string) bool { return true }, nil, nil)
	if result.err != nil {
		t.Fatal(result.err)
	}
	// Fake.Start retains Env but does not project it through GetMeta.
	launch := f.sp.LastStartConfig(f.info.SessionName)
	if launch == nil {
		t.Fatal("runtime launch was not recorded")
	}
	for _, key := range []string{"GC_SESSION_ID", "GC_INSTANCE_TOKEN", "GC_RUNTIME_EPOCH"} {
		if launch.Env[key] == "" {
			t.Fatalf("launch omitted %s", key)
		}
		if err := f.sp.SetMeta(f.info.SessionName, key, launch.Env[key]); err != nil {
			t.Fatal(err)
		}
	}

	return result
}

func recordTestWake(t *testing.T, f *controllerMessageWakeFixture, m *session.Manager, clk clock.Clock) {
	t.Helper()
	if _, err := m.PersistedStore().WakeSession(f.info.ID, clk.Now(), session.WakeOpts{}); err != nil {
		t.Fatal(err)
	}
	assertTestWake(t, f, "explicit")
}

func assertTestWake(t *testing.T, f *controllerMessageWakeFixture, want string) {
	t.Helper()
	row, err := f.store.Get(f.info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Metadata["wake_request"] != want || (row.Metadata["wake_requested_at"] == "") != (want == "") {
		t.Fatalf("wake_request=%q, wake_requested_at=%q, want wake %q", row.Metadata["wake_request"], row.Metadata["wake_requested_at"], want)
	}
}

func assertStartsAfterDrain(t *testing.T, f *controllerMessageWakeFixture, want int) {
	t.Helper()
	if err := f.store.SetMetadataBatch(f.info.ID, session.AcknowledgeDrainPatch(time.Now(), true)); err != nil {
		t.Fatal(err)
	}
	if err := f.sp.Stop(f.info.SessionName); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		f.reconcile(t)
	}
	if starts := f.sp.CountCalls("Start", f.info.SessionName) - f.starts; starts != want || !f.sp.IsRunning(f.info.SessionName) {
		t.Fatalf("after drain: starts=%d running=%v, want %d starts and running", starts, f.sp.IsRunning(f.info.SessionName), want)
	}
}

func runWakeRecoveryPass(t *testing.T, f *controllerMessageWakeFixture, clk *clock.Fake) {
	t.Helper()
	rows, err := f.store.List(beads.ListQuery{Label: session.LabelSession})
	if err != nil {
		t.Fatal(err)
	}
	desired := buildDesiredState("wake-city", f.cityPath, clk.Now(), f.cfg, f.sp, f.store, io.Discard)
	reconcileSessionBeadsAtPath(t.Context(), f.cityPath, rows, desired.State, configuredSessionNames(f.cfg, "wake-city", f.store),
		f.cfg, f.sp, f.store, nil, nil, nil, nil, newDrainTracker(), nil, false, map[string]bool{f.info.SessionName: true}, "wake-city", nil, clk, f.events, time.Minute, 0, io.Discard, io.Discard)
}

// Recovery keeps a wake made during startup because its snapshot cannot show
// which request started the session. The wake causes one more start after drain.
func TestRecoveryKeepsWakeDuringStart(t *testing.T) {
	f, m, clk, prepared := preparePendingWakeTest(t)
	recordTestWake(t, f, m, clk)
	_ = launchPendingWakeTest(t, f, clk, prepared)
	clk.Advance(2 * time.Minute)
	runWakeRecoveryPass(t, f, clk)
	assertConfirmedWakeRecovery(t, f, m, prepared)
	assertTestWake(t, f, "explicit")
	assertStartsAfterDrain(t, f, 2)
}

func assertConfirmedWakeRecovery(t *testing.T, f *controllerMessageWakeFixture, m *session.Manager, prepared *preparedStart) {
	t.Helper()
	info, err := m.Get(f.info.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := prepared.candidate.info
	starts := f.sp.CountCalls("Start", f.info.SessionName)
	if !before.PendingCreateClaim || info.PendingCreateClaim || info.CreationCompleteAt <= before.CreationCompleteAt || info.InstanceToken != before.InstanceToken || starts != 1 {
		t.Fatalf("recovery: claim=%v stamp=%q previous=%q same token=%v starts=%d", info.PendingCreateClaim, info.CreationCompleteAt, before.CreationCompleteAt, info.InstanceToken == before.InstanceToken, starts)
	}
}

// Recovery and a late result can both confirm one start. Both keep the wake
// so the next drain cannot strand a message.
func TestRecoveryAndAsyncResultCommitSameStart(t *testing.T) {
	for _, order := range []string{"wake_after_recovery", "pass_then_wake", "wake_then_pass", "wake_before_recovery"} {
		t.Run(order, func(t *testing.T) {
			f, m, clk, prepared := preparePendingWakeTest(t)
			if order == "wake_before_recovery" {
				recordTestWake(t, f, m, clk)
			}
			result := launchPendingWakeTest(t, f, clk, prepared)
			clk.Advance(2 * time.Minute)
			runWakeRecoveryPass(t, f, clk)
			assertConfirmedWakeRecovery(t, f, m, prepared)
			if order == "wake_before_recovery" {
				assertTestWake(t, f, "explicit")
			} else {
				assertTestWake(t, f, "")
			}
			if order == "pass_then_wake" {
				runWakeRecoveryPass(t, f, clk)
			}
			if order != "wake_before_recovery" {
				recordTestWake(t, f, m, clk)
			}
			if order == "wake_then_pass" {
				runWakeRecoveryPass(t, f, clk)
			}
			if order == "pass_then_wake" || order == "wake_then_pass" {
				info, err := m.Get(f.info.ID)
				if err != nil {
					t.Fatal(err)
				}
				if info.MetadataState != "awake" || !confirmStartCommitState(info.MetadataState) {
					t.Fatalf("after extra pass: state=%q", info.MetadataState)
				}
			}
			clk.Advance(time.Second)
			if !commitAsyncStartResultWithContext(t.Context(), result, f.sp, f.store, clk, f.events, 0, io.Discard, io.Discard, nil) {
				t.Fatal("late async result rejected")
			}
			assertTestWake(t, f, "explicit")
			if starts := f.sp.CountCalls("Start", f.info.SessionName); starts != 1 {
				t.Fatalf("starts before drain=%d", starts)
			}
			assertStartsAfterDrain(t, f, 2)
		})
	}
}

// Recovery leaves both wake fields out of its write because the timestamps
// cannot tell whether a newer request still needs a start.
func TestRecoveryBatchOmitsWakeKeys(t *testing.T) {
	for _, tc := range []struct{ name, woke, committed string }{
		{"no_prewake", "", "2026-01-01T00:00:00Z"},
		{"first_start", "2026-01-01T00:00:01Z", ""},
		{"newer_prewake", "2026-01-01T00:00:01Z", "2026-01-01T00:00:00Z"},
		{"equal_stamps", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, clk, prepared := preparePendingWakeTest(t)
			info := prepared.candidate.info
			info.LastWokeAt, info.CreationCompleteAt = tc.woke, tc.committed
			ok, batch := recoverRunningPendingCreate(info, prepared.candidate.tp, f.cfg, f.store, clk, nil, nil, io.Discard)
			if !ok {
				t.Fatal("recovery rejected")
			}
			for _, key := range []string{"wake_request", "wake_requested_at"} {
				if val, exists := batch[key]; exists {
					t.Fatalf("recovery wrote %s=%q", key, val)
				}
			}
		})
	}
}

// If a restart cannot stop the old runtime, recovery can confirm that runtime
// again. It keeps the pending wake so the session starts after drain.
func TestRecoveryAfterAsyncCommitAndConfigDrift(t *testing.T) {
	for _, when := range []string{"before_drift", "after_drift"} {
		t.Run(when, func(t *testing.T) {
			f, m, clk, prepared := preparePendingWakeTest(t)
			result := launchPendingWakeTest(t, f, clk, prepared)

			if !commitAsyncStartResultWithContext(t.Context(), result, f.sp, f.store, clk, f.events, 0, io.Discard, io.Discard, nil) {
				t.Fatal("first async commit was rejected")
			}
			first, err := m.Get(f.info.ID)
			if err != nil || first.PendingCreateClaim || first.CreationCompleteAt == "" {
				t.Fatal("first commit did not confirm start", err)
			}
			f.sp.Activity = map[string]time.Time{f.info.SessionName: clk.Now().Add(-24 * time.Hour)}
			f.cfg.Agents[0].PreStart = append(f.cfg.Agents[0].PreStart, "true")
			f.sp.StopErrors[f.info.SessionName] = errors.New("injected stop failure")
			limiter := newAsyncStartLimiter(1)
			release, reserved, _ := limiter.reserve(t.Context())
			if !reserved {
				t.Fatal("could not occupy start slot")
			}
			defer release()
			pass := func() {
				t.Helper()
				clk.Advance(2 * time.Minute)
				rows, err := f.store.List(beads.ListQuery{Label: session.LabelSession})
				if err != nil {
					t.Fatal(err)
				}
				ds := buildDesiredState("wake-city", f.cityPath, clk.Now(), f.cfg, f.sp, f.store, io.Discard)
				reconcileSessionBeadsAtPath(t.Context(), f.cityPath, rows, ds.State,
					configuredSessionNames(f.cfg, "wake-city", f.store), f.cfg, f.sp, f.store,
					nil, nil, nil, nil, newDrainTracker(), nil, false,
					map[string]bool{f.info.SessionName: true}, "wake-city", nil, clk, f.events,
					time.Minute, 0, os.Stdout, os.Stderr, withAsyncStartExecution(), withAsyncStartLimiter(limiter))
			}
			if when == "before_drift" {
				recordTestWake(t, f, m, clk)
			}
			pass()
			reset, err := m.Get(f.info.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("after drift: stop calls=%d running=%v claim=%v same token=%v same stamp=%v state=%s starts=%d", f.sp.CountCalls("Stop", f.info.SessionName), f.sp.IsRunning(f.info.SessionName), reset.PendingCreateClaim, reset.InstanceToken == first.InstanceToken, reset.CreationCompleteAt == first.CreationCompleteAt, reset.MetadataState, f.sp.CountCalls("Start", f.info.SessionName))
			if !reset.PendingCreateClaim || reset.CreationCompleteAt != first.CreationCompleteAt || !f.sp.IsRunning(f.info.SessionName) || reset.InstanceToken != first.InstanceToken {
				t.Fatal("drift did not leave the existing runtime with a fresh claim")
			}
			if when == "after_drift" {
				recordTestWake(t, f, m, clk)
			}
			beforeRepeat, err := m.Get(f.info.ID)
			if err != nil || beforeRepeat.WakeRequest != "explicit" {
				t.Fatal("post-commit wake was not recorded", err)
			}
			pass()
			again, err := m.Get(f.info.ID)
			if err != nil {
				t.Fatal(err)
			}
			starts := f.sp.CountCalls("Start", f.info.SessionName)
			t.Logf("after recovery: starts=%d running=%v same token=%v stamp advanced=%v claim=%v wake before=%q after=%q", starts, f.sp.IsRunning(f.info.SessionName), again.InstanceToken == first.InstanceToken, again.CreationCompleteAt != first.CreationCompleteAt, again.PendingCreateClaim, beforeRepeat.WakeRequest, again.WakeRequest)
			if starts != 1 || !f.sp.IsRunning(f.info.SessionName) || again.InstanceToken != first.InstanceToken || again.CreationCompleteAt == first.CreationCompleteAt || again.PendingCreateClaim {
				t.Fatal("recovery did not commit the same start a second time")
			}
			if again.WakeRequest != "explicit" {
				t.Fatal("repeat recovery erased the protected wake after the async commit")
			}

			delete(f.sp.StopErrors, f.info.SessionName)
			assertStartsAfterDrain(t, f, 2)
		})
	}
}

// After a failed commit write, recovery keeps the running replacement's wake.
// Clearing it could strand work that arrived before recovery ran.
func TestFirstRecoveryAfterCommitWriteFails(t *testing.T) {
	f, m, clk, prepared := preparePendingWakeTest(t)
	result := launchPendingWakeTest(t, f, clk, prepared)

	if !commitAsyncStartResultWithContext(t.Context(), result, f.sp, f.store, clk, f.events, 0, io.Discard, io.Discard, nil) {
		t.Fatal("first async commit was rejected")
	}
	first, err := m.Get(f.info.ID)
	if err != nil || first.PendingCreateClaim || first.CreationCompleteAt == "" {
		t.Fatal("first commit did not confirm start", err)
	}
	f.sp.Activity = map[string]time.Time{f.info.SessionName: clk.Now().Add(-24 * time.Hour)}
	f.cfg.Agents[0].PreStart = append(f.cfg.Agents[0].PreStart, "true")
	limiter := newAsyncStartLimiter(1)
	release, reserved, _ := limiter.reserve(t.Context())
	if !reserved {
		t.Fatal("could not occupy start slot")
	}
	defer release()
	pass := func() {
		t.Helper()
		clk.Advance(2 * time.Minute)
		rows, err := f.store.List(beads.ListQuery{Label: session.LabelSession})
		if err != nil {
			t.Fatal(err)
		}
		ds := buildDesiredState("wake-city", f.cityPath, clk.Now(), f.cfg, f.sp, f.store, io.Discard)
		reconcileSessionBeadsAtPath(t.Context(), f.cityPath, rows, ds.State,
			configuredSessionNames(f.cfg, "wake-city", f.store), f.cfg, f.sp, f.store,
			nil, nil, nil, nil, newDrainTracker(), nil, false,
			map[string]bool{f.info.SessionName: true}, "wake-city", nil, clk, f.events,
			time.Minute, 0, os.Stdout, os.Stderr, withAsyncStartExecution(), withAsyncStartLimiter(limiter))
	}
	pass()
	reset, err := m.Get(f.info.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after drift: stop calls=%d running=%v claim=%v same token=%v same stamp=%v state=%s starts=%d", f.sp.CountCalls("Stop", f.info.SessionName), f.sp.IsRunning(f.info.SessionName), reset.PendingCreateClaim, reset.InstanceToken == first.InstanceToken, reset.CreationCompleteAt == first.CreationCompleteAt, reset.MetadataState, f.sp.CountCalls("Start", f.info.SessionName))

	if !reset.PendingCreateClaim || reset.CreationCompleteAt != first.CreationCompleteAt || f.sp.IsRunning(f.info.SessionName) {
		t.Fatal("config drift must have stopped the old runtime, retained its last successful stamp, and requested a replacement")
	}
	clk.Advance(2 * time.Minute)
	ds := buildDesiredState("wake-city", f.cityPath, clk.Now(), f.cfg, f.sp, f.store, io.Discard)
	replacement, err := prepareStartCandidateForCity(startCandidate{info: reset, tp: ds.State[f.info.SessionName], configured: true}, f.cityPath, "wake-city", f.cfg, f.sp, f.store, clk, io.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	during, err := m.Get(f.info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if during.InstanceToken == first.InstanceToken || during.LastWokeAt <= first.CreationCompleteAt {
		t.Fatal("replacement needs a new token and a pre-wake later than the previous start")
	}
	if _, err := m.PersistedStore().WakeSession(f.info.ID, clk.Now(), session.WakeOpts{}); err != nil {
		t.Fatal(err)
	}
	result = runPreparedStartCandidate(t.Context(), *replacement, f.cityPath, f.sp, f.store, f.cfg, time.Minute, func(context.Context, string) bool { return true }, nil, nil)
	if result.err != nil {
		t.Fatal(result.err)
	}
	launch := f.sp.LastStartConfig(f.info.SessionName)
	for _, key := range []string{"GC_SESSION_ID", "GC_INSTANCE_TOKEN", "GC_RUNTIME_EPOCH"} {
		if launch == nil || launch.Env[key] == "" {
			t.Fatalf("missing %s", key)
		}
		if err := f.sp.SetMeta(f.info.SessionName, key, launch.Env[key]); err != nil {
			t.Fatal(err)
		}
	}
	// One unsuccessful atomic write. The following SetMarker and all recovery writes succeed.
	failing := &failingMetadataBatchStore{MemStore: f.store, failBatch: true}
	var diagnostic strings.Builder
	committed := commitAsyncStartResultWithContext(t.Context(), result, f.sp, failing, clk, f.events, 0, io.Discard, &diagnostic, nil)
	if committed || !strings.Contains(diagnostic.String(), "storing hashes") || !strings.Contains(diagnostic.String(), "batch failed") {
		t.Fatalf("unexpected failed write: %v %s", committed, diagnostic.String())
	}
	failed, err := m.Get(f.info.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pre-wake=%q previous successful commit=%q; after failed batch: last_woke_at=%q claim=%v wake=%q stamp=%q", during.LastWokeAt, first.CreationCompleteAt, failed.LastWokeAt, failed.PendingCreateClaim, failed.WakeRequest, failed.CreationCompleteAt)
	if failed.LastWokeAt != "" || !failed.PendingCreateClaim || failed.WakeRequest != "explicit" || failed.CreationCompleteAt != first.CreationCompleteAt {
		t.Fatal("missing failed-write preconditions")
	}
	pass()
	confirmed, err := m.Get(f.info.ID)
	if err != nil {
		t.Fatal(err)
	}
	starts := f.sp.CountCalls("Start", f.info.SessionName)
	t.Logf("first recovery commit: stamp=%q advanced=%v same replacement token=%v claim=%v wake=%q starts=%d running=%v", confirmed.CreationCompleteAt, confirmed.CreationCompleteAt > failed.CreationCompleteAt, confirmed.InstanceToken == during.InstanceToken, confirmed.PendingCreateClaim, confirmed.WakeRequest, starts, f.sp.IsRunning(f.info.SessionName))
	if confirmed.CreationCompleteAt <= failed.CreationCompleteAt || confirmed.InstanceToken != during.InstanceToken || confirmed.PendingCreateClaim || starts != 2 || !f.sp.IsRunning(f.info.SessionName) {
		t.Fatal("no recovery commit for the live replacement")
	}

	if confirmed.WakeRequest != "explicit" {
		t.Fatal("recovery cleared the replacement start's wake")
	}
	assertStartsAfterDrain(t, f, 3)
}

// Recovery can confirm a start from a snapshot taken before the async commit.
// Its write must preserve a wake that arrived after that snapshot.
func TestRecoveryWithSnapshotBeforeAsyncCommit(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	f := &controllerMessageWakeFixture{cityPath: t.TempDir(), store: beads.NewMemStore(), sp: runtime.NewFake()}
	text := "[workspace]\nname = \"wake-city\"\n[session]\nprovider = \"fake\"\n[providers.catalog-provider]\ncommand = \"echo\"\nprompt_mode = \"none\"\n[[agent]]\nname = \"runner\"\nprovider = \"catalog-provider\"\n[[named_session]]\ntemplate = \"runner\"\nmode = \"on_demand\"\n"
	path := filepath.Join(f.cityPath, "city.toml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	var err error
	f.cfg, _, err = config.LoadWithIncludes(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	f.events = &providerMessageResultEvents{Fake: events.NewFake(), done: make(chan events.Event, 4)}
	cs := newControllerState(t.Context(), f.cfg, f.sp, f.events, "wake-city", f.cityPath)
	cs.cityBeadStore, cs.pokeCh = f.store, make(chan struct{}, 4)
	f.server = api.New(cs)
	f.post(t, "/session/runner/submit", `{"message":"setup"}`, events.RequestResultSessionSubmit)
	all, err := f.store.List(beads.ListQuery{Label: session.LabelSession})
	if err != nil {
		t.Fatal(err)
	}
	m := session.NewManagerWithOptions(f.store, f.sp, session.WithCityPath(f.cityPath))
	for _, b := range all {
		if b.Metadata["template"] == "runner" && b.Status != "closed" {
			f.info, err = m.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if f.info.ID == "" || !f.info.PendingCreateClaim {
		t.Fatalf("API did not create a claimed pending session: %+v", f.info)
	}
	clk := &clock.Fake{Time: time.Now().UTC()}
	desired := buildDesiredState("wake-city", f.cityPath, clk.Now(), f.cfg, f.sp, f.store, io.Discard)
	tp, ok := desired.State[f.info.SessionName]
	if !ok {
		t.Fatal("pending create absent from desired state")
	}
	prepared, err := prepareStartCandidateForCity(startCandidate{info: f.info, tp: tp, configured: true}, f.cityPath, "wake-city", f.cfg, f.sp, f.store, clk, io.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := runPreparedStartCandidate(t.Context(), *prepared, f.cityPath, f.sp, f.store, f.cfg, time.Minute, func(context.Context, string) bool { return true }, nil, nil)
	if result.err != nil {
		t.Fatal(result.err)
	}
	// Fake.Start retains Env but does not project it through GetMeta.
	launch := f.sp.LastStartConfig(f.info.SessionName)
	if launch == nil {
		t.Fatal("runtime launch was not recorded")
	}
	for _, key := range []string{"GC_SESSION_ID", "GC_INSTANCE_TOKEN", "GC_RUNTIME_EPOCH"} {
		if launch.Env[key] == "" {
			t.Fatalf("launch omitted %s", key)
		}
		if err := f.sp.SetMeta(f.info.SessionName, key, launch.Env[key]); err != nil {
			t.Fatal(err)
		}
	}
	before, err := m.Get(f.info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !before.PendingCreateClaim || !f.sp.IsRunning(f.info.SessionName) {
		t.Fatal("expected live runtime and pending claim before commit")
	}
	// Model an async result whose commit is delayed beyond the existing lease.
	// The provider already started successfully; no second provider start occurs.
	clk.Advance(2 * time.Minute)
	if pendingCreateStartInFlightInfo(before, clk, time.Minute) {
		t.Fatal("lease has not expired")
	}
	all, err = f.store.List(beads.ListQuery{Label: session.LabelSession})
	if err != nil {
		t.Fatal(err)
	}
	// The pending row was listed above. While this pass holds that snapshot,
	// the async goroutine confirms the live start, then a caller requests a wake.
	if !commitAsyncStartResultWithContext(t.Context(), result, f.sp, f.store, clk, f.events, 0, io.Discard, io.Discard, nil) {
		t.Fatal("async did not confirm the start")
	}
	first, err := m.Get(f.info.ID)
	if err != nil || first.CreationCompleteAt == "" || first.PendingCreateClaim || first.InstanceToken != before.InstanceToken {
		t.Fatalf("missing first commit: stamp=%q claim=%v err=%v", first.CreationCompleteAt, first.PendingCreateClaim, err)
	}
	clk.Advance(time.Second)
	if _, err := m.PersistedStore().WakeSession(f.info.ID, clk.Now(), session.WakeOpts{}); err != nil {
		t.Fatal(err)
	}
	woken, err := f.store.Get(f.info.ID)
	if err != nil || woken.Metadata["wake_request"] != "explicit" || woken.Metadata["wake_requested_at"] == "" {
		t.Fatalf("wake missing after first commit: %q err=%v", woken.Metadata["wake_request"], err)
	}
	clk.Advance(time.Second)
	reconcileSessionBeadsAtPath(t.Context(), f.cityPath, all, desired.State,
		configuredSessionNames(f.cfg, "wake-city", f.store), f.cfg, f.sp, f.store,
		nil, nil, nil, nil, newDrainTracker(), nil, false,
		map[string]bool{f.info.SessionName: true}, "wake-city", nil, clk, f.events,
		time.Minute, 0, io.Discard, io.Discard)
	after, err := m.Get(f.info.ID)
	if err != nil {
		t.Fatal(err)
	}
	starts := f.sp.CountCalls("Start", f.info.SessionName)
	last, err := f.store.Get(f.info.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("listed claim=%v stamp=%q prewake=%q; async stamp=%q claim=%v; post-async wake=%q at=%q; recovery stamp=%q claim=%v wake=%q at=%q sameToken=%v starts=%d",
		before.PendingCreateClaim, before.CreationCompleteAt, before.LastWokeAt, first.CreationCompleteAt, first.PendingCreateClaim,
		woken.Metadata["wake_request"], woken.Metadata["wake_requested_at"], after.CreationCompleteAt, after.PendingCreateClaim, after.WakeRequest, last.Metadata["wake_requested_at"], first.InstanceToken == after.InstanceToken, starts)
	if after.CreationCompleteAt <= first.CreationCompleteAt || after.PendingCreateClaim || first.InstanceToken != after.InstanceToken || starts != 1 {
		t.Fatal("recovery did not re-confirm the same start")
	}
	if after.WakeRequest != woken.Metadata["wake_request"] || last.Metadata["wake_requested_at"] != woken.Metadata["wake_requested_at"] {
		t.Fatal("recovery erased the wake recorded after the async commit")
	}
}

// A late success keeps the wake for a message queued after the runtime exited.
// The controller can then start a replacement to deliver the message.
func TestMessageAfterRuntimeExitBeforeStartCommit(t *testing.T) {
	f := newControllerMessageWakeFixture(t, "on_demand")
	f.setRow(t, "fresh-drained")
	m := session.NewManagerWithOptions(f.store, f.sp, session.WithCityPath(f.cityPath))
	clk := &clock.Fake{Time: time.Now().UTC()}
	recordTestWake(t, f, m, clk)
	info, err := m.Get(f.info.ID)
	if err != nil {
		t.Fatal(err)
	}
	desired := buildDesiredState("wake-city", f.cityPath, clk.Now(), f.cfg, f.sp, f.store, io.Discard)
	tp, ok := desired.State[f.info.SessionName]
	if !ok {
		t.Fatal("initial wake missing from desired state")
	}
	prepared, err := prepareStartCandidateForCity(startCandidate{info: info, tp: tp, configured: true}, f.cityPath, "wake-city", f.cfg, f.sp, f.store, clk, io.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := launchPendingWakeTest(t, f, clk, prepared)
	if err := f.sp.Stop(f.info.SessionName); err != nil {
		t.Fatal(err)
	}
	if f.sp.IsRunning(f.info.SessionName) {
		t.Fatal("runtime is still running")
	}
	f.post(t, "/session/"+f.info.ID+"/submit", `{"message":"marker-after-exit","intent":"follow_up"}`, events.RequestResultSessionSubmit)
	assertTestWake(t, f, "explicit")
	queued, err := nudgequeue.LoadState(f.cityPath)
	if err != nil || len(queued.Pending) != 1 || queued.Pending[0].Message != "marker-after-exit" {
		t.Fatal("message was not queued", err)
	}
	if !commitAsyncStartResultWithContext(t.Context(), result, f.sp, f.store, clk, f.events, 0, io.Discard, io.Discard, nil) {
		t.Fatal("late result did not commit")
	}
	committed, err := m.Get(f.info.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		f.reconcile(t)
	}
	starts := f.sp.CountCalls("Start", f.info.SessionName) - f.starts
	t.Logf("after late success: wake=%q state=%q; after three passes: starts=%d running=%v", committed.WakeRequest, committed.MetadataState, starts, f.sp.IsRunning(f.info.SessionName))
	if starts != 2 || !f.sp.IsRunning(f.info.SessionName) {
		t.Fatal("no replacement runtime for queued message")
	}
	idleSince := time.Now().Add(-time.Minute)
	f.sp.Activity = map[string]time.Time{f.info.SessionName: idleSince}
	final, err := f.store.Get(f.info.ID)
	if err != nil {
		t.Fatal(err)
	}
	target := nudgeTarget{cityPath: f.cityPath, agent: config.Agent{Name: "runner"}, sessionID: f.info.ID, sessionName: f.info.SessionName, alias: f.info.Alias, continuationEpoch: final.Metadata["continuation_epoch"], resolved: &config.ResolvedProvider{Name: "codex"}}
	delivered, err := tryDeliverQueuedNudgesByPoller(target, f.store, f.store, f.sp, time.Second, worker.LiveObservation{Running: true, LastActivity: &idleSince})
	if err != nil || !delivered {
		t.Fatalf("queued message delivery=%v err=%v", delivered, err)
	}
}
