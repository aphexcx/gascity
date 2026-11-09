package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gastownhall/gascity/internal/extmsg"
)

func TestExtMsgConditionalUnbindHTTP(t *testing.T) {
	fs, srv, services, ref := newExtMsgAgentBindingFixture(t)
	first, err := services.Bindings.Bind(context.Background(), extmsg.Caller{Kind: extmsg.CallerController, ID: "extmsg-default-route"}, extmsg.BindInput{Conversation: ref, AgentName: "myrig/worker"})
	if err != nil {
		t.Fatal(err)
	}
	query := url.Values{"scope_id": {ref.ScopeID}, "provider": {ref.Provider}, "account_id": {ref.AccountID}, "kind": {string(ref.Kind)}, "conversation_id": {ref.ConversationID}}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(fs, "/extmsg/bindings")+"?"+query.Encode(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var listed ListBody[extmsg.SessionBindingRecord]
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Items) != 1 || listed.Items[0].Caller.ID != "extmsg-default-route" || listed.Items[0].CreatedAt.IsZero() {
		t.Fatalf("list omitted provenance: %+v", listed)
	}
	invalid := postExtMsg(t, fs, srv, "/extmsg/unbind", map[string]any{"conversation": conversationBody(ref), "expected_agent_name": "", "expected_caller_id": ""})
	if invalid.Code != http.StatusUnprocessableEntity {
		t.Fatalf("empty conditions must not become unconditional: %d %s", invalid.Code, invalid.Body.String())
	}

	// Explicitly binding the same agent between inspection and unbind wins.
	bound := postExtMsg(t, fs, srv, "/extmsg/bind", map[string]any{"conversation": conversationBody(ref), "agent_name": "myrig/worker"})
	if bound.Code != http.StatusOK {
		t.Fatalf("bind: %d %s", bound.Code, bound.Body.String())
	}
	conflict := postExtMsg(t, fs, srv, "/extmsg/unbind", map[string]any{"conversation": conversationBody(ref), "expected_agent_name": "myrig/worker", "expected_caller_id": "extmsg-default-route"})
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conditional status=%d body=%s", conflict.Code, conflict.Body.String())
	}
	var body struct {
		ActiveBinding *extmsg.SessionBindingRecord `json:"active_binding"`
	}
	if err := json.Unmarshal(conflict.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ActiveBinding == nil || body.ActiveBinding.Caller.ID != "api" {
		t.Fatalf("conflict omitted current binding: %s", conflict.Body.String())
	}
	stale := postExtMsg(t, fs, srv, "/extmsg/unbind", map[string]any{"binding_id": first.ID})
	if stale.Code != http.StatusNotFound {
		t.Fatalf("stale id: %d %s", stale.Code, stale.Body.String())
	}
	removed := postExtMsg(t, fs, srv, "/extmsg/unbind", map[string]any{"binding_id": body.ActiveBinding.ID})
	if removed.Code != http.StatusOK {
		t.Fatalf("remove id: %d %s", removed.Code, removed.Body.String())
	}
	var result ExtMsgUnbindBody
	if err := json.Unmarshal(removed.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Unbound) != 1 || result.Unbound[0].Caller.ID != "api" {
		t.Fatalf("removed provenance: %+v", result)
	}
}
