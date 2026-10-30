package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

func TestSessionListJSONShowsStartupMenu(t *testing.T) {
	var out, stderr bytes.Buffer
	infos := []session.Info{{ID: "s-1", SessionName: "worker", State: session.StateActive}}
	if code := writeSessionListJSONWithMenus(infos, "", "", map[string]string{"s-1": "Hooks need review"}, &out, &stderr); code != 0 {
		t.Fatal(stderr.String())
	}
	var got sessionListJSON
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Sessions[0].State != "parked" || got.Sessions[0].Reason != "Hooks need review" {
		t.Fatalf("row=%+v", got.Sessions[0])
	}
	if infos[0].State != session.StateActive || got.Summary.Active != 1 {
		t.Fatal("display observation changed lifecycle state")
	}
}

func TestInternalCodexHookTrustReportsFailure(t *testing.T) {
	var out, stderr bytes.Buffer
	cmd := newInternalCodexHookTrustCmd(&out, &stderr)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err == nil || stderr.Len() == 0 {
		t.Fatalf("missing error diagnostic: err=%v stderr=%q", err, stderr.String())
	}
}

func TestCodexHookTrustRetargetKeepsLauncherAndCity(t *testing.T) {
	commands := appendCodexHookTrustPreStart(nil, "/city/bin/codex -p city", "/city", "/city")
	got := retargetPreStartWorkDir(commands, "/city", "/lane")[0]
	if !strings.Contains(got, "--city-dir /city --workdir /lane") || !strings.Contains(got, "'/city/bin/codex -p city'") {
		t.Fatal(got)
	}
}
