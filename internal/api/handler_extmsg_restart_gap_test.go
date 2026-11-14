package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/session"
)

type restartControllerState struct{ *fakeState }

func (*restartControllerState) ControllerRunning() bool { return true }

func TestAPIMessagesWaitForNamedSessionRestart(t *testing.T) {
	for _, surface := range []string{"inbound-fanout", "interrupt_now", "follow_up", "messages"} {
		t.Run(surface, func(t *testing.T) {
			fs, srv, services, ref := newExtMsgAgentBindingFixture(t)
			srv.state = &restartControllerState{fs}
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
			mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
			info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
				BeadOnly: true, Alias: "runner", ExplicitName: "runner", Template: "runner", Provider: "test-launch", Command: "/bin/echo", WorkDir: fs.cityPath,
				ExtraMeta: map[string]string{
					session.NamedSessionMetadataKey: "true", session.NamedSessionIdentityMetadata: "runner", session.NamedSessionModeMetadata: "always",
					"session_origin": "named", "provider_kind": "codex", "state": "active",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			patch := session.RestartRequestPatch("", time.Now())
			patch["state"] = "active"
			if err := fs.cityBeadStore.SetMetadataBatch(info.ID, patch); err != nil {
				t.Fatal(err)
			}
			beforePokes := fs.pokeCount
			if surface == "inbound-fanout" {
				if _, err := services.Bindings.Bind(context.Background(), extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}, extmsg.BindInput{Conversation: ref, SessionID: info.ID}); err != nil {
					t.Fatal(err)
				}
				body := map[string]any{"message": map[string]any{
					"provider_message_id": "restart-gap-message", "conversation": conversationBody(ref),
					"actor": map[string]any{"id": "user-1", "display_name": "User", "is_bot": false},
					"text":  "deliver after restart", "received_at": time.Now().UTC().Format(time.RFC3339),
				}}
				for attempt := 0; attempt < 2; attempt++ {
					rec := postExtMsg(t, fs, srv, "/extmsg/inbound", body)
					if rec.Code != http.StatusOK {
						t.Fatalf("inbound status %d: %s", rec.Code, rec.Body.String())
					}
					var result extmsg.InboundResult
					if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
						t.Fatal(err)
					}
					if result.Delivery == nil || result.Delivery.Status != extmsg.InboundDeliveryPending {
						t.Errorf("queued inbound must report pending so an adapter holds: %+v", result.Delivery)
					}
				}
			} else {
				path := "/session/" + info.ID + "/submit"
				body := map[string]any{"message": "deliver after restart", "intent": surface}
				if surface == "messages" {
					path = "/session/" + info.ID + "/messages"
					delete(body, "intent")
				}
				rec := postExtMsg(t, fs, srv, path, body)
				if rec.Code != http.StatusAccepted {
					t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
				}
				var accepted asyncAcceptedBody
				if err := json.NewDecoder(rec.Body).Decode(&accepted); err != nil {
					t.Fatal(err)
				}
				if surface == "messages" {
					if success, failure := waitForSessionMessageResult(t, fs.eventProv, accepted.RequestID); success == nil {
						t.Fatalf("message failed: %+v", failure)
					}
				} else if success, failure := waitForSessionSubmitResult(t, fs.eventProv, accepted.RequestID); success == nil || !success.Queued {
					t.Fatalf("submit not queued: success=%+v failure=%+v", success, failure)
				}
			}
			srv.waitForBackground()
			for _, call := range fs.sp.Calls {
				if call.Method == "Start" {
					t.Error("message started the named session inside the restart gap")
				}
			}
			b, err := fs.cityBeadStore.Get(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			if b.Metadata["continuation_reset_pending"] != "true" {
				t.Error("message consumed the controller restart marker")
			}
			queued, err := nudgequeue.LoadState(fs.cityPath)
			if err != nil {
				t.Fatal(err)
			}
			if len(queued.Pending) != 1 {
				t.Errorf("queued deliveries = %d, want exactly one", len(queued.Pending))
			}
			if fs.pokeCount <= beforePokes {
				t.Error("queued message did not poke controller")
			}
		})
	}
}

func TestAPIWakeWithControllerOnlyRecordsWake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newSessionFakeState(t)
		srv := New(&restartControllerState{fs})
		info := createTestSession(t, fs.cityBeadStore, fs.sp, "Sleeping session")
		mgr := session.NewManagerWithOptions(fs.cityBeadStore, fs.sp)
		if err := mgr.Suspend(info.ID); err != nil {
			t.Fatal(err)
		}
		starts := fs.sp.CountCalls("Start", info.SessionName)
		if _, err := srv.humaHandleSessionWake(t.Context(), &SessionIDInput{ID: info.ID}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		b, err := fs.cityBeadStore.Get(info.ID)
		if err != nil {
			t.Fatal(err)
		}
		if fs.sp.CountCalls("Start", info.SessionName) != starts || fs.pokeCount != 1 || b.Metadata["wake_request"] != "explicit" {
			t.Fatalf("starts=%d pokes=%d wake=%q", fs.sp.CountCalls("Start", info.SessionName)-starts, fs.pokeCount, b.Metadata["wake_request"])
		}
	})
}

func TestAPIInboundMaterializesWithoutStartingWithController(t *testing.T) {
	fs, srv, services, ref := newExtMsgAgentBindingFixture(t)
	srv.state = &restartControllerState{fs}
	if _, err := services.Bindings.Bind(t.Context(), extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}, extmsg.BindInput{Conversation: ref, AgentName: "myrig/worker"}); err != nil {
		t.Fatal(err)
	}
	rec := postExtMsg(t, fs, srv, "/extmsg/inbound", map[string]any{"message": map[string]any{
		"provider_message_id": "new-session-message", "conversation": conversationBody(ref),
		"actor": map[string]any{"id": "user-1", "display_name": "User", "is_bot": false},
		"text":  "hello", "received_at": time.Now().UTC().Format(time.RFC3339),
	}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", rec.Code, rec.Body.String())
	}
	srv.waitForBackground()
	id, err := session.ResolveSessionID(fs.cityBeadStore, "myrig/worker")
	if err != nil {
		t.Fatal(err)
	}
	b, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := nudgequeue.LoadState(fs.cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if fs.sp.CountCalls("Start", b.Metadata["session_name"]) != 0 || len(queued.Pending) != 1 || fs.pokeCount == 0 {
		t.Fatalf("starts=%d queue=%d pokes=%d", fs.sp.CountCalls("Start", b.Metadata["session_name"]), len(queued.Pending), fs.pokeCount)
	}
}
