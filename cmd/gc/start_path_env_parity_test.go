package main

import (
	"bytes"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// Reused from the earlier launch-parity proof, restricted to paths that may
// start outside the reconciler. Compare actual runtime Start inputs.
func TestSessionStartEnvironmentParity(t *testing.T) {
	t.Setenv("GC_TEST_AMBIENT_SWEEP", "ambient")
	cityPath := t.TempDir()
	writePhase0InterfaceCity(t, cityPath, `[workspace]
name = "launch-city"
[beads]
provider = "file"
[agent_defaults]
upstream = "u"
[upstreams.u]
base_url = "https://model.example.invalid"
auth_token = "invented-test-token"
base_url_env = "TEST_MODEL_BASE_URL"
auth_token_env = "TEST_MODEL_AUTH_TOKEN"
[[agent]]
name = "runner"
provider = "test-launch"
[agent.env]
TEST_AGENT_ENV = "agent"
[providers.test-launch]
command = "/bin/echo"
args = ["--model", "example-model"]
prompt_mode = "none"
[providers.test-launch.env]
TEST_PROVIDER_ENV = "provider"
`)
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	a := &cfg.Agents[0]
	resolved, err := config.ResolveProvider(a, &cfg.Workspace, cfg.Providers, func(name string) (string, error) { return name, nil })
	if err != nil {
		t.Fatal(err)
	}
	previousManaged, previousPoke := nudgeCityUsesManagedReconciler, nudgePokeController
	nudgeCityUsesManagedReconciler = func(string) bool { return false }
	nudgePokeController = func(string) error { return nil }
	t.Cleanup(func() { nudgeCityUsesManagedReconciler, nudgePokeController = previousManaged, previousPoke })
	clean := func(env map[string]string) map[string]string {
		out := maps.Clone(env)
		for _, key := range []string{"GC_INSTANCE_TOKEN", "BEADS_HOLDER_TOKEN", "GC_SESSION_KEY", "GC_RUNTIME_EPOCH", "GC_CONTINUATION_EPOCH"} {
			delete(out, key)
		}
		return out
	}
	for _, surface := range []string{"start", "attach", "nudge", "message", "cli-interrupt", "api-interrupt", "create", "deferred-start"} {
		t.Run(surface, func(t *testing.T) {
			store, sp := beads.NewMemStore(), runtime.NewFake()
			mgr := session.NewManagerWithOptions(store, sp)
			info, err := mgr.CreateSession(t.Context(), session.CreateOptions{BeadOnly: true, Template: a.Name, Command: resolved.CommandString() + " --unwanted-flag", WorkDir: cityPath, Provider: resolved.Name, ExtraMeta: map[string]string{session.NamedSessionMetadataKey: "true", session.NamedSessionIdentityMetadata: a.Name, "provider_kind": "codex", "session_origin": "named"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetMetadata(info.ID, "state", "active"); err != nil {
				t.Fatal(err)
			}
			params := newAgentBuildParams(cfg.EffectiveCityName(), cityPath, cfg, sp, time.Unix(0, 0), store, io.Discard)
			tp, err := resolveTemplateForSessionBeadInfo(params, a, a.QualifiedName(), nil, info)
			if err != nil {
				t.Fatal(err)
			}
			tp.Env["GC_SESSION_ORIGIN"] = "named"
			wantLaunch, _, err := templateParamsToConfigWithDelivery(tp)
			if err != nil {
				t.Fatal(err)
			}
			baseline := runtime.NewFake()
			baselineManager := session.NewManagerWithOptions(store, baseline)
			if err := baselineManager.StartRuntimeOnly(t.Context(), info.ID, wantLaunch.Command, wantLaunch); err != nil {
				t.Fatal(err)
			}
			wantLaunch = *baseline.LastStartConfig(info.SessionName)
			wantKeys := slices.Sorted(maps.Keys(clean(wantLaunch.Env)))
			t.Logf("reconciler keys: %v", wantKeys)
			var launch *runtime.Config
			if surface == "create" || surface == "deferred-start" {
				createStore := beads.NewMemStore()
				metadata := map[string]string{"real_world_app_session_kind": "agent", "session_origin": "named", session.NamedSessionMetadataKey: "true", session.NamedSessionIdentityMetadata: "runner"}
				h, err := newWorkerSessionHandleForResolvedRuntimeWithConfig(cityPath, createStore, sp, cfg, "runner", "runner", a.Name, "test", resolved.CommandString(), resolved.Name, cityPath, "", resolved, metadata)
				if err != nil {
					t.Fatal(err)
				}
				if surface == "deferred-start" {
					if _, err := h.Create(t.Context(), worker.CreateModeDeferred); err != nil {
						t.Fatal(err)
					}
					if err := h.Start(t.Context()); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := h.Create(t.Context(), worker.CreateModeStarted); err != nil {
						t.Fatal(err)
					}
				}
				for _, call := range sp.Calls {
					if call.Method == "Start" {
						launch = sp.LastStartConfig(call.Name)
					}
				}
			} else {
				if strings.Contains(surface, "interrupt") {
					if err := sp.Start(t.Context(), info.SessionName, runtime.Config{Command: "outgoing"}); err != nil {
						t.Fatal(err)
					}
					sp.InterruptBoundaryErrors[info.SessionName] = errors.New("injected boundary failure")
				}
				if surface == "api-interrupt" {
					ev := &entryLaunchResultEvents{Fake: events.NewFake(), done: make(chan events.Event, 1)}
					state := &controllerState{cfg: cfg, sp: sp, cityBeadStore: store, cityPath: cityPath, cityName: cfg.EffectiveCityName(), eventProv: ev, pokeCh: make(chan struct{}, 1)}
					req := httptest.NewRequest(http.MethodPost, "/v0/city/"+state.CityName()+"/session/"+info.ID+"/submit", bytes.NewBufferString(`{"message":"replace","intent":"interrupt_now"}`))
					req.Header.Set("X-GC-Request", "true")
					rec := httptest.NewRecorder()
					api.New(state).ServeHTTP(rec, req)
					if rec.Code != http.StatusAccepted {
						t.Fatalf("status=%d", rec.Code)
					}
					select {
					case event := <-ev.done:
						if event.Type != events.RequestResultSessionSubmit {
							t.Fatalf("submit event=%s", event.Type)
						}
					case <-time.After(hangBudget):
						t.Fatal("submit did not finish")
					}
				} else {
					h, err := workerHandleForSessionWithConfig(cityPath, store, sp, cfg, info.ID)
					if err != nil {
						t.Fatal(err)
					}
					switch surface {
					case "start":
						err = h.Start(t.Context())
					case "attach":
						err = h.Attach(t.Context())
					case "nudge":
						_, err = h.Nudge(t.Context(), worker.NudgeRequest{Text: "hello"})
					case "message":
						_, err = h.Message(t.Context(), worker.MessageRequest{Text: "hello"})
					case "cli-interrupt":
						_, err = h.Message(t.Context(), worker.MessageRequest{Text: "hello", Delivery: worker.DeliveryIntentInterruptNow})
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				launch = sp.LastStartConfig(info.SessionName)
			}
			if launch == nil {
				t.Fatal("start did not happen")
			}
			if !strings.Contains(launch.Command, "--model example-model") || strings.Contains(launch.Command, "--unwanted-flag") {
				t.Error("launch did not use the configured command")
			}
			gotKeys := slices.Sorted(maps.Keys(clean(launch.Env)))
			t.Logf("%s keys: %v", surface, gotKeys)
			if surface == "create" {
				t.Logf("direct-create residual missing=%v extra=%v", missingLaunchKeys(wantKeys, gotKeys), missingLaunchKeys(gotKeys, wantKeys))
				for _, key := range []string{"TEST_MODEL_BASE_URL", "TEST_MODEL_AUTH_TOKEN"} {
					if launch.Env[key] != "" {
						t.Errorf("direct create unexpectedly changed base key %s", key)
					}
				}
				return
			}
			if !slices.Equal(gotKeys, wantKeys) {
				t.Errorf("environment key sets differ; missing=%v extra=%v", missingLaunchKeys(wantKeys, gotKeys), missingLaunchKeys(gotKeys, wantKeys))
			}
			for _, key := range []string{"TEST_MODEL_BASE_URL", "TEST_MODEL_AUTH_TOKEN", "TEST_PROVIDER_ENV", "TEST_AGENT_ENV", "GC_TEST_AMBIENT_SWEEP"} {
				if launch.Env[key] == "" || launch.Env[key] != wantLaunch.Env[key] {
					t.Errorf("missing or changed %s", key)
				}
			}
			if launch.Upstream != wantLaunch.Upstream || !maps.Equal(launch.OperatorEnv, wantLaunch.OperatorEnv) {
				t.Error("upstream/operator identity differs")
			}
		})
	}
}

func missingLaunchKeys(want, got []string) []string {
	var missing []string
	for _, key := range want {
		if !slices.Contains(got, key) {
			missing = append(missing, key)
		}
	}
	return missing
}
