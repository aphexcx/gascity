package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

type entryLaunchAPIState struct {
	*fakeState
	builder worker.SessionRuntimeResolver
}

func (*entryLaunchAPIState) ControllerRunning() bool                               { return true }
func (s *entryLaunchAPIState) SessionLaunchBuilder() worker.SessionRuntimeResolver { return s.builder }

func TestAPIInterruptReplacementNeedsFullLaunch(t *testing.T) {
	for _, site := range []string{"interrupt-boundary", "idle-wait", "pi-restart", "pi-restore"} {
		t.Run(site, func(t *testing.T) {
			fs := newSessionFakeState(t)
			cfg, err := config.Parse([]byte(`[workspace]
name = "test-city"
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
[[named_session]]
template = "runner"
[providers.test-launch]
command = "/bin/echo"
prompt_mode = "none"
`))
			if err != nil {
				t.Fatal(err)
			}
			config.ApplyAgentDefaults(cfg)
			fs.cfg = cfg
			kind := "codex"
			if site == "idle-wait" {
				kind = "claude"
			}
			if strings.HasPrefix(site, "pi-") {
				kind = "pi"
			}
			mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
			info, err := mgr.CreateSession(t.Context(), session.CreateOptions{Template: "runner", Provider: "test-launch", Command: "/bin/echo", WorkDir: fs.cityPath, ExtraMeta: map[string]string{"provider_kind": kind, "session_origin": "named", session.NamedSessionMetadataKey: "true", session.NamedSessionIdentityMetadata: "runner"}})
			if err != nil {
				t.Fatal(err)
			}
			transcriptDir := t.TempDir()
			if site == "pi-restore" {
				content := fmt.Sprintf("{\"type\":\"session\",\"id\":\"abc123\",\"cwd\":%q}\n{\"type\":\"message\",\"id\":\"u1\",\"message\":{\"role\":\"user\",\"content\":\"pending\"}}\n", info.WorkDir)
				if err := os.WriteFile(filepath.Join(transcriptDir, "2026-01-01T00-00-00-000Z_abc123.jsonl"), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(transcriptDir, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(transcriptDir, 0o700); err != nil {
						t.Error(err)
					}
				})
				// This input is present on both the cheap base resolver and the
				// full launch, so the base reaches the same restore site.
				p := cfg.Providers["test-launch"]
				p.Env = map[string]string{"PI_CODING_AGENT_SESSION_DIR": transcriptDir}
				cfg.Providers["test-launch"] = p
			}
			if site == "interrupt-boundary" {
				fs.sp.InterruptBoundaryErrors[info.SessionName] = errors.New("injected interrupt boundary failure")
			}
			if site == "idle-wait" {
				fs.sp.WaitForIdleErrors[info.SessionName] = errors.New("injected idle failure")
			}
			var builds atomic.Int32
			state := &entryLaunchAPIState{fakeState: fs, builder: func(info session.Info, _ string, _ map[string]string) (*worker.ResolvedRuntime, error) {
				builds.Add(1)
				return &worker.ResolvedRuntime{Command: "/bin/echo --prepared", Provider: "test-launch", WorkDir: info.WorkDir, Hints: runtime.Config{Command: "/bin/echo --prepared", Env: map[string]string{"TEST_MODEL_BASE_URL": "https://model.example.invalid", "TEST_MODEL_AUTH_TOKEN": "invented-test-token", "PI_CODING_AGENT_SESSION_DIR": transcriptDir}}}, nil
			}}
			srv := New(state)
			starts := fs.sp.CountCalls("Start", info.SessionName)
			rec := postExtMsg(t, fs, srv, "/session/"+info.ID+"/submit", map[string]any{"message": "replace this turn", "intent": "interrupt_now"})
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status=%d", rec.Code)
			}
			var accepted asyncAcceptedBody
			if err := json.NewDecoder(rec.Body).Decode(&accepted); err != nil {
				t.Fatal(err)
			}
			success, failure := waitForSessionSubmitResult(t, fs.eventProv, accepted.RequestID)
			srv.waitForBackground()
			wantFailure := site == "idle-wait" || site == "pi-restore"
			if wantFailure && failure == nil {
				t.Fatal("injected failure was not reported")
			}
			if !wantFailure && success == nil {
				t.Fatalf("submit failed: %+v", failure)
			}
			if builds.Load() != 1 {
				t.Errorf("entry builds=%d, want one", builds.Load())
			}
			if fs.sp.CountCalls("Start", info.SessionName) != starts+1 {
				t.Fatal("expected exactly one same-call replacement start")
			}
			launch := fs.sp.LastStartConfig(info.SessionName)
			for _, key := range []string{"TEST_MODEL_BASE_URL", "TEST_MODEL_AUTH_TOKEN"} {
				if launch.Env[key] == "" {
					t.Errorf("replacement lost %s", key)
				}
			}
		})
	}
}

func TestAPIEntryLaunchCost(t *testing.T) {
	fs := newSessionFakeState(t)
	mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
	info, err := mgr.CreateSession(t.Context(), session.CreateOptions{Template: "runner", Provider: "codex", Command: "/bin/echo", WorkDir: fs.cityPath})
	if err != nil {
		t.Fatal(err)
	}
	builds := 0
	srv := New(&entryLaunchAPIState{fakeState: fs, builder: func(session.Info, string, map[string]string) (*worker.ResolvedRuntime, error) {
		builds++
		return nil, errors.New("launch-only failure")
	}})
	h, err := srv.workerHandleForSession(fs.cityBeadStore, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(fs, "/sessions"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d", rec.Code)
	}
	if _, err := h.Peek(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	if _, err := h.History(t.Context(), worker.HistoryRequest{}); err != nil && !errors.Is(err, worker.ErrHistoryUnavailable) {
		t.Fatal(err)
	}
	if _, err := h.LiveObservation(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, intent := range []worker.DeliveryIntent{worker.DeliveryIntentDefault, worker.DeliveryIntentFollowUp} {
		if _, err := h.Message(t.Context(), worker.MessageRequest{Text: "hello", Delivery: intent}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if builds != 0 {
		t.Fatalf("read, live-message, and stop operations built %d launches", builds)
	}
	if _, err := h.Message(t.Context(), worker.MessageRequest{Text: "interrupt", Delivery: worker.DeliveryIntentInterruptNow}); err == nil {
		t.Error("interrupt did not return launch error")
	}
	if builds != 1 {
		t.Fatalf("interrupt builds=%d", builds)
	}
}

func TestAPICreatesDoNotBuildEntryLaunch(t *testing.T) {
	for _, kind := range []string{"provider", "agent"} {
		t.Run(kind, func(t *testing.T) {
			fs := newSessionFakeState(t)
			var builds atomic.Int32
			srv := New(&entryLaunchAPIState{fakeState: fs, builder: func(session.Info, string, map[string]string) (*worker.ResolvedRuntime, error) {
				builds.Add(1)
				return nil, errors.New("API create must keep supplied launch")
			}})
			body := `{"kind":"provider","name":"test-agent","project_id":"alpha","alias":"create-test"}`
			if kind == "agent" {
				body = `{"kind":"agent","name":"myrig/worker"}`
			}
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(body)))
			if rec.Code != http.StatusAccepted {
				t.Fatalf("create status=%d", rec.Code)
			}
			var accepted asyncAcceptedBody
			if err := json.NewDecoder(rec.Body).Decode(&accepted); err != nil {
				t.Fatal(err)
			}
			success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
			if success == nil {
				t.Fatalf("create failed: %s", failure.ErrorMessage)
			}
			if builds.Load() != 0 {
				t.Fatalf("API create made %d full builds", builds.Load())
			}
		})
	}
}
