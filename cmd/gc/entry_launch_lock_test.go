package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

type entryLaunchResultEvents struct {
	*events.Fake
	done chan events.Event
}

func (e *entryLaunchResultEvents) Record(event events.Event) {
	e.Fake.Record(event)
	if event.Type == events.RequestResultSessionSubmit || event.Type == events.RequestFailed {
		e.done <- event
	}
}

type observedEntryLaunchState struct {
	*controllerState
	entered chan struct{}
}

func (s *observedEntryLaunchState) SessionLaunchBuilder() worker.SessionRuntimeResolver {
	capability, ok := any(s.controllerState).(interface {
		SessionLaunchBuilder() worker.SessionRuntimeResolver
	})
	if !ok {
		return nil
	}
	build := capability.SessionLaunchBuilder()
	return func(info sessionpkg.Info, kind string, metadata map[string]string) (*worker.ResolvedRuntime, error) {
		close(s.entered)
		return build(info, kind, metadata)
	}
}

func TestAPIEntryLaunchReleasesSessionLockWhileCacheBlocked(t *testing.T) {
	f := newCachedImportFixture(t, func(f *cachedImportFixture) {
		f.cfg.AgentDefaults.Upstream = "test-model"
		f.cfg.Upstreams = map[string]config.UpstreamSpec{"test-model": {
			BaseURL: "https://model.example.invalid", AuthToken: "invented-test-token",
			BaseURLEnv: "TEST_MODEL_BASE_URL", AuthTokenEnv: "TEST_MODEL_AUTH_TOKEN",
		}}
		config.ApplyAgentDefaults(f.cfg)
	})
	if err := f.store.SetMetadataBatch(f.beadID, map[string]string{
		"provider": "claude", "provider_kind": "codex", "command": "echo",
		"session_origin": "named", sessionpkg.NamedSessionMetadataKey: "true",
		sessionpkg.NamedSessionIdentityMetadata: "worker",
	}); err != nil {
		t.Fatal(err)
	}
	f.sp.InterruptBoundaryErrors["worker"] = errors.New("injected interrupt boundary failure")
	ep := &entryLaunchResultEvents{Fake: events.NewFake(), done: make(chan events.Event, 1)}
	cs := newControllerState(t.Context(), f.cfg, f.sp, ep, "test-city", f.cityPath)
	cs.cityBeadStore = f.store
	cs.pokeCh = make(chan struct{}, 1)
	state := &observedEntryLaunchState{controllerState: cs, entered: make(chan struct{})}
	srv := api.New(state)
	unlock := holdStartupRepairCache(t)
	starts := f.sp.CountCalls("Start", "worker")
	body, err := json.Marshal(struct {
		Message string `json:"message"`
		Intent  string `json:"intent"`
	}{Message: "replace this turn", Intent: "interrupt_now"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v0/city/test-city/session/"+f.beadID+"/submit", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit status %d", rec.Code)
	}
	var result *events.Event
	select {
	case <-state.entered:
	case event := <-ep.done:
		result = &event
		t.Log("base has no entry builder; lock half passes vacuously")
	case <-time.After(10 * time.Second):
		t.Fatal("neither entry builder nor submit completion observed")
	}
	acquired := make(chan struct{})
	go func() {
		_ = sessionpkg.WithSessionMutationLock(f.beadID, func() error { close(acquired); return nil })
	}()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		unlock()
		t.Fatal("entry launch build held the session mutation lock")
	}
	unlock()
	if result == nil {
		select {
		case event := <-ep.done:
			result = &event
		case <-time.After(10 * time.Second):
			t.Fatal("submit did not finish after cache release")
		}
	}
	if result.Type != events.RequestResultSessionSubmit {
		t.Fatalf("submit result event type %s", result.Type)
	}
	if f.sp.CountCalls("Start", "worker") != starts+1 {
		t.Fatal("expected exactly one replacement Start")
	}
	launch := f.sp.LastStartConfig("worker")
	for _, key := range []string{"TEST_MODEL_BASE_URL", "TEST_MODEL_AUTH_TOKEN"} {
		if launch.Env[key] == "" {
			t.Errorf("interrupt replacement launch lacks %s", key)
		}
	}
}
