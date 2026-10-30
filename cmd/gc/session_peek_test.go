package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

// The frame and prompt colors come from the 2026-10-30 capture; the faint
// suggestion reconstructs the SGR 2 observation recorded on 2026-10-29.
const suggestionPeekFixture = "\x1b[2mstatus stays visible\x1b[0m\n" +
	"\x1b[38;5;244m────────────────\n" +
	"\x1b[38;5;246m❯\u00a0\x1b[39m\x1b[2mTry the next step\x1b[0m\n" +
	"\x1b[38;5;244m────────────────\x1b[39m\n"

func TestSessionPeekSchemaIncludesSuggestion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"session", "peek", "--json-schema=result"}, &stdout, &stderr); code != 0 {
		t.Fatalf("schema = %d: %s", code, &stderr)
	}
	var schema struct {
		Properties map[string]struct{ Type string } `json:"properties"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Properties["suggestion"].Type != "string" {
		t.Fatalf("schema has no suggestion string: %s", &stdout)
	}
}

func TestSessionPeekAPIRendersSuggestion(t *testing.T) {
	cr := api.CachedRead[api.SessionView]{Body: api.SessionView{ID: "session-1", LastOutput: suggestionPeekFixture}}
	for _, jsonOutput := range []bool{false, true} {
		var stdout, stderr bytes.Buffer
		if code := renderSessionPeekFromAPI(cr, "worker", 50, jsonOutput, &stdout, &stderr); code != 0 {
			t.Fatalf("render = %d: %s", code, &stderr)
		}
		if !jsonOutput {
			if !strings.Contains(stdout.String(), "[suggestion] Try the next step") || strings.Contains(stdout.String(), "\x1b") {
				t.Fatalf("text peek = %q", stdout.String())
			}
			continue
		}
		var got struct {
			Output     string `json:"output"`
			Suggestion string `json:"suggestion"`
			LineCount  int    `json:"line_count"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Suggestion != "Try the next step" || strings.Contains(got.Output, "Try the next step") || strings.Contains(got.Output, "\x1b") {
			t.Fatalf("JSON peek = %+v", got)
		}
		if got.LineCount != 4 || !strings.Contains(got.Output, "status stays visible") {
			t.Fatalf("lost pane lines: %+v", got)
		}
	}
}
