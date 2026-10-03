package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

func TestCmdSessionNew_IdentityHolderRejectsGenericFallback(t *testing.T) {
	for _, tc := range []struct {
		name       string
		state      string
		identifier string
		singleton  bool
		closed     bool
	}{
		{name: "drained alias holder", state: "drained", identifier: "alias"},
		{name: "archived runtime holder", state: "archived", identifier: "session_name"},
		{name: "singleton drained alias holder", state: "drained", identifier: "alias", singleton: true},
		{name: "singleton closed runtime holder", state: "closed", identifier: "session_name", singleton: true, closed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := writeSessionListTestCity(t)
			if tc.singleton {
				writeSimplePlainTemplateCityTOML(t, cityDir)
				writeCatalogFile(t, cityDir, "agents/kenneth/agent.toml", "provider = \"codex\"\nstart_command = \"echo\"\nmax_active_sessions = 1\n")
			} else {
				writeSimpleNamedSessionCityTOML(t, cityDir)
			}

			store, err := openCityStoreAt(cityDir)
			if err != nil {
				t.Fatal(err)
			}
			holder, err := store.Create(beads.Bead{
				Type:   session.BeadType,
				Labels: []string{session.LabelSession},
				Metadata: map[string]string{
					"template":    "kenneth",
					"state":       tc.state,
					tc.identifier: "kenneth",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.closed {
				if err := store.Close(holder.ID); err != nil {
					t.Fatal(err)
				}
			}

			var stdout, stderr bytes.Buffer
			if code := cmdSessionNew([]string{"kenneth"}, "", "", "", true, false, 0, &stdout, &stderr); code == 0 {
				t.Fatalf("cmdSessionNew succeeded despite holder %s: stdout=%q", holder.ID, stdout.String())
			}
			if !strings.Contains(stderr.String(), holder.ID) {
				t.Errorf("stderr = %q, want conflicting holder %s", stderr.String(), holder.ID)
			}
			currentStore, err := openCityStoreAt(cityDir)
			if err != nil {
				t.Fatal(err)
			}
			all, err := currentStore.ListByLabel(session.LabelSession, 0, beads.IncludeClosed)
			if err != nil {
				t.Fatal(err)
			}
			if len(all) != 1 || all[0].ID != holder.ID {
				t.Fatalf("session beads = %+v, want only original holder %s", all, holder.ID)
			}
		})
	}
}

func TestCmdSessionNew_SingletonUsesCanonicalIdentity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tmuxAlias string
		userAlias string
		wantName  string
		wantAlias string
		wantAgent string
	}{
		{name: "canonical", wantName: "kenneth", wantAlias: "kenneth", wantAgent: "kenneth"},
		{name: "tmux alias", tmuxAlias: "terminal", wantName: "terminal", wantAlias: "kenneth", wantAgent: "terminal"},
		{name: "explicit user alias", userAlias: "custom", wantAlias: "custom", wantAgent: "kenneth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := writeSessionListTestCity(t)
			writeSimplePlainTemplateCityTOML(t, cityDir)
			writeCatalogFile(t, cityDir, "agents/kenneth/agent.toml", "provider = \"codex\"\nstart_command = \"echo\"\nmax_active_sessions = 1\ntmux_alias = \""+tc.tmuxAlias+"\"\n")

			var stdout, stderr bytes.Buffer
			if code := cmdSessionNew([]string{"kenneth"}, tc.userAlias, "", "", true, false, 0, &stdout, &stderr); code != 0 {
				t.Fatalf("cmdSessionNew = %d: %s", code, stderr.String())
			}
			b := onlySessionBead(t, cityDir)
			if got := b.Metadata["session_name"]; tc.wantName != "" && got != tc.wantName {
				t.Errorf("session_name = %q, want %q", got, tc.wantName)
			} else if tc.wantName == "" && !strings.HasPrefix(got, "s-") {
				t.Errorf("session_name = %q, want generated runtime for explicit user alias", got)
			}
			for key, want := range map[string]string{"alias": tc.wantAlias, "agent_name": tc.wantAgent, "session_origin": "manual"} {
				if got := b.Metadata[key]; got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
		})
	}
}
