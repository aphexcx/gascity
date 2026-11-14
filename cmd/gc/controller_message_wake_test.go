package main

import (
	"context"
	"encoding/json"
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
	case "fresh-drained":
		patch = session.AcknowledgeDrainPatch(now, true)
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
}

func TestDeferredControllerMessageStartsAndDeliversOnce(t *testing.T) {
	for _, kind := range []string{"on_demand", "always", "manual"} {
		for _, row := range []string{"idle-asleep", "fresh-drained", "drift-asleep", "healed-runtime-missing", "crashed-active", "crashed-awake", "restart-requested", "handoff-requested", "committed-reset"} {
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
				for pass := 0; pass < 3; pass++ {
					f.reconcile(t)
				}
				after, err := f.store.Get(f.info.ID)
				if err != nil {
					t.Fatal(err)
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
