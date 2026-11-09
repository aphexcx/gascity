//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/extmsg"
)

func TestExtMsgConditionalFlagsRoundTrip(t *testing.T) {
	var got struct {
		BindingID         string                 `json:"binding_id"`
		ExpectedAgentName string                 `json:"expected_agent_name"`
		ExpectedCallerID  string                 `json:"expected_caller_id"`
		Conversation      extmsg.ConversationRef `json:"conversation"`
	}
	var listQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			listQuery = r.URL.Query().Get("conversation_id")
			if r.URL.Path != "/v0/city/alpha/extmsg/bindings" || r.URL.Query().Get("parent_conversation_id") != "room" {
				t.Errorf("list request: %s", r.URL)
			}
			if _, err := w.Write([]byte(`{"items":[],"total":0}`)); err != nil {
				t.Error(err)
			}
			return
		}
		if r.URL.Path != "/v0/city/alpha/extmsg/unbind" || r.Header.Get("X-GC-Request") != "true" {
			t.Errorf("unbind request: %s", r.URL)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		if _, err := w.Write([]byte(`{"unbound":[]}`)); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	origAlive, origSup, origCity := apiRouteControllerAliveHook, apiRouteSupervisorClientHook, cityFlag
	t.Cleanup(func() {
		apiRouteControllerAliveHook, apiRouteSupervisorClientHook, cityFlag = origAlive, origSup, origCity
	})
	apiRouteControllerAliveHook = func(string) int { return 0 }
	apiRouteSupervisorClientHook = func(string) *api.Client { return api.NewCityScopedClient(server.URL, "alpha") }
	cityFlag = writeCityTOMLForRoute(t, t.TempDir(), "name = \"alpha\"\n")
	t.Setenv("GC_NO_API", "")
	run := func(args ...string) {
		t.Helper()
		var out, stderr bytes.Buffer
		cmd := newExtMsgCmd(&out, &stderr)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v %s", args, err, stderr.String())
		}
	}
	run("unbind", "--binding-id", "binding-1", "--json")
	if got.BindingID != "binding-1" {
		t.Fatalf("binding id lost: %+v", got)
	}
	got.BindingID = ""
	run("unbind", "--provider", "slack", "--conversation-id", "room", "--scope-id", "alpha", "--kind", "room", "--expect-agent", "helper", "--expect-caller", "extmsg-default-route", "--json")
	if got.ExpectedAgentName != "helper" || got.ExpectedCallerID != "extmsg-default-route" || got.Conversation.Kind != extmsg.ConversationRoom {
		t.Fatalf("conditions lost: %+v", got)
	}
	run("bindings", "--provider", "slack", "--conversation-id", "thread", "--parent-conversation-id", "room", "--scope-id", "alpha", "--kind", "thread", "--json")
	if listQuery != "thread" {
		t.Fatalf("conversation filter lost: %q", listQuery)
	}
	var out, stderr bytes.Buffer
	cmd := newExtMsgCmd(&out, &stderr)
	cmd.SetArgs([]string{"unbind", "--provider", "slack", "--conversation-id", "room", "--scope-id", "alpha", "--expect-agent", "", "--expect-caller", ""})
	if err := cmd.Execute(); err == nil {
		t.Fatal("empty guard flags must not become unconditional")
	}
}
