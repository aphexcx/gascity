package main

import (
	"bytes"
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
)

type providerMessageResultEvents struct {
	*events.Fake
	done chan events.Event
}

func (e *providerMessageResultEvents) Record(event events.Event) {
	e.Fake.Record(event)
	switch event.Type {
	case events.RequestResultSessionCreate, events.RequestResultSessionSubmit, events.RequestFailed:
		e.done <- event
	}
}

func (e *providerMessageResultEvents) next(t *testing.T, want string) events.Event {
	t.Helper()
	select {
	case event := <-e.done:
		if event.Type != want {
			t.Fatalf("result event=%s, want %s: %s", event.Type, want, event.Payload)
		}
		return event
	case <-time.After(10 * time.Second):
		t.Fatalf("no %s result", want)
		return events.Event{}
	}
}

func TestComposedProviderMessageStartsThroughReconciler(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	cityPath := t.TempDir()
	const cityName = "composed-city"
	const providerName = "catalog-provider"
	const cityConfig = "[workspace]\nname = \"composed-city\"\n[session]\nprovider = \"fake\"\n[providers.catalog-provider]\ncommand = \"echo\"\nprompt_mode = \"none\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	implicit := findAgentByTemplate(cfg, providerName)
	if implicit == nil || !implicit.Implicit {
		t.Fatal("composition did not create the catalog provider's implicit agent")
	}
	store, sp := beads.NewMemStore(), runtime.NewFake()
	ep := &providerMessageResultEvents{Fake: events.NewFake(), done: make(chan events.Event, 4)}
	cs := newControllerState(t.Context(), cfg, sp, ep, cityName, cityPath)
	cs.cityBeadStore = store
	cs.pokeCh = make(chan struct{}, 4)
	srv := api.New(cs)
	post := func(path, body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v0/city/"+cityName+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-GC-Request", "true")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("POST %s status=%d: %s", path, rec.Code, rec.Body.String())
		}
	}
	post("/sessions", `{"kind":"provider","name":"catalog-provider","alias":"provider-chat"}`)
	createdEvent := ep.next(t, events.RequestResultSessionCreate)
	var created api.SessionCreateSucceededPayload
	if err := json.Unmarshal(createdEvent.Payload, &created); err != nil {
		t.Fatal(err)
	}
	mgr := session.NewManagerWithOptions(store, sp, session.WithCityPath(cityPath))
	if err := mgr.Suspend(created.Session.ID); err != nil {
		t.Fatal(err)
	}
	info, err := mgr.Get(created.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sp.IsRunning(info.SessionName) {
		t.Fatal("runtime remained up after suspend")
	}
	starts := sp.CountCalls("Start", info.SessionName)
	post("/session/"+info.ID+"/submit", `{"message":"composed-provider-wake-marker","intent":"follow_up"}`)
	resultEvent := ep.next(t, events.RequestResultSessionSubmit)
	var submitted api.SessionSubmitSucceededPayload
	if err := json.Unmarshal(resultEvent.Payload, &submitted); err != nil {
		t.Fatal(err)
	}
	if !submitted.Queued || sp.CountCalls("Start", info.SessionName) != starts {
		t.Fatal("managed provider message must queue without starting in the API call")
	}
	queued, err := nudgequeue.LoadState(cityPath)
	if err != nil || len(queued.Pending) != 1 {
		t.Fatalf("queue before reconcile=%+v, err=%v; want one pending message", queued, err)
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Metadata["wake_request"] != string(session.WakeCauseExplicit) {
		t.Fatalf("wake_request=%q, want explicit", b.Metadata["wake_request"])
	}
	var output bytes.Buffer
	desired := buildDesiredState(cityName, cityPath, time.Now().UTC(), cfg, sp, store, &output)
	if _, ok := desired.State[info.SessionName]; !ok {
		t.Fatalf("composed API provider session missing from desired set: %s", output.String())
	}
	cr := &CityRuntime{
		cityPath: cityPath, cityName: cityName, cfg: cfg, sp: sp,
		standaloneCityStore: store, sessionDrains: newDrainTracker(),
		rec: ep, stdout: io.Discard, stderr: &output,
	}
	cr.beadReconcileTick(t.Context(), desired, newSessionBeadSnapshot([]beads.Bead{b}), nil, false)
	if !cr.asyncStarts.wait(10 * time.Second) {
		t.Fatal("reconcile start did not finish")
	}
	if !sp.IsRunning(info.SessionName) || sp.CountCalls("Start", info.SessionName) != starts+1 {
		t.Fatalf("reconcile did not start the queued provider session exactly once: running=%v starts=%d before=%d; %s", sp.IsRunning(info.SessionName), sp.CountCalls("Start", info.SessionName), starts, output.String())
	}
	t.Log("composed provider: implicit agent, desired membership, one queued message, explicit wake, one reconciler start")
}

func TestReconcilerOrphanSessionDisposition(t *testing.T) {
	for _, live := range []bool{false, true} {
		name := "dead"
		if live {
			name = "live"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("GC_BEADS", "file")
			cityPath := t.TempDir()
			writeCityTOML(t, cityPath, "orphan-city", "configured-worker")
			cfg := &config.City{
				Workspace: config.Workspace{Name: "orphan-city"},
				Session:   config.SessionConfig{Provider: "fake"},
				Agents:    []config.Agent{{Name: "configured-worker"}},
			}
			store, sp := beads.NewMemStore(), runtime.NewFake()
			b, err := store.Create(beads.Bead{
				Title: "orphan-provider", Type: session.BeadType, Labels: []string{session.LabelSession},
				Metadata: map[string]string{
					"session_name": "orphan-provider-session", "template": "removed-provider", "provider": "removed-provider",
					"session_origin": "manual", "state": "suspended", "generation": "1", "continuation_epoch": "1",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if live {
				if err := sp.Start(t.Context(), "orphan-provider-session", runtime.Config{}); err != nil {
					t.Fatal(err)
				}
			}
			desired := buildDesiredState("orphan-city", cityPath, time.Now().UTC(), cfg, sp, store, io.Discard)
			if _, ok := desired.State["orphan-provider-session"]; ok {
				t.Fatal("orphan unexpectedly selected into desired state")
			}
			cr := &CityRuntime{
				cityPath: cityPath, cityName: "orphan-city", cfg: cfg, sp: sp,
				standaloneCityStore: store, sessionDrains: newDrainTracker(),
				rec: events.NewFake(), stdout: io.Discard, stderr: io.Discard,
			}
			cr.beadReconcileTick(t.Context(), desired, newSessionBeadSnapshot([]beads.Bead{b}), nil, false)
			after, err := store.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if live {
				if cr.sessionDrains.get(b.ID) == nil || after.Status == "closed" {
					t.Fatalf("live orphan was not retained for drain: status=%s", after.Status)
				}
			} else if after.Status != "closed" || after.Metadata["state"] != "orphaned" {
				t.Fatalf("dead orphan status=%s state=%s, want closed/orphaned", after.Status, after.Metadata["state"])
			}
		})
	}
}
