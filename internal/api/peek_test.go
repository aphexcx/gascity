package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

const attributedPaneFixture = "\x1b[2mstatus\x1b[0m\n────────────────\n❯ \x1b[2mTry the next step\x1b[0m\n────────────────\n"

func TestAgentOutputPeekTagsSuggestion(t *testing.T) {
	state := newFakeState(t)
	if err := state.sp.Start(context.Background(), "myrig--worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	state.sp.SetPeekOutput("myrig--worker", attributedPaneFixture)
	srv := newServerWithSearchPaths(state, t.TempDir())
	h := newTestCityHandlerWith(t, state, srv)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", cityURL(state, "/agent/myrig/worker/output"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var response agentOutputResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Format != "text" || len(response.Turns) != 1 {
		t.Fatalf("response = %+v", response)
	}
	text := response.Turns[0].Text
	if !strings.Contains(text, "[suggestion] Try the next step") || !strings.Contains(text, "status") || strings.Contains(text, "\x1b") {
		t.Fatalf("pane text = %q", text)
	}
}

func TestStructuredPeekFallbackTagsSuggestion(t *testing.T) {
	messages := structuredFallbackMessages("s", "claude", attributedPaneFixture)
	if len(messages) != 1 || len(messages[0].Blocks) != 1 {
		t.Fatalf("messages = %+v", messages)
	}
	if text := messages[0].Blocks[0].Text; !strings.Contains(text, "[suggestion] Try the next step") {
		t.Fatalf("pane text = %q", text)
	}
}
