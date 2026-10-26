package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestCmdSlingRefusesImplicitCodeSlot(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "codex-astra"} {
		t.Run(provider, func(t *testing.T) {
			cityDir := setupCmdSlingBeadExistsFixture(t)
			configPath := filepath.Join(cityDir, "city.toml")
			content, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			content = append(content, []byte("\n[providers.claude]\nbase = \"builtin:claude\"\n[providers.codex]\nbase = \"builtin:codex\"\n[providers.codex-astra]\nbase = \"builtin:codex\"\n")...)
			if err := os.WriteFile(configPath, content, 0o644); err != nil {
				t.Fatal(err)
			}
			rigDir := filepath.Join(cityDir, "frontend")
			writeTestFileStoreBeads(t, rigDir, []beads.Bead{{ID: "FE-1", Title: "Code change", Status: "open", Type: "task"}})
			var stdout, stderr bytes.Buffer
			cmd := newSlingCmd(&stdout, &stderr)
			cmd.SetArgs([]string{"frontend/" + provider, "FE-1", "--force", "--no-convoy", "--no-formula"})
			if err := cmd.Execute(); err == nil || !strings.Contains(stderr.String(), "--allow-implicit") || !strings.Contains(stderr.String(), "frontend/worker") {
				t.Fatalf("implicit dispatch must refuse and name the configured lane: err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
			store, err := openStoreAtForCity(rigDir, cityDir)
			if err != nil {
				t.Fatal(err)
			}
			b, err := store.Get("FE-1")
			if err != nil || b.Metadata["gc.routed_to"] != "" {
				t.Fatalf("refusal changed routing: bead=%+v err=%v", b, err)
			}

			stdout.Reset()
			stderr.Reset()
			cmd = newSlingCmd(&stdout, &stderr)
			cmd.SetArgs([]string{"frontend/" + provider + "-1", "FE-1", "--force", "--no-convoy", "--no-formula"})
			if err := cmd.Execute(); err == nil || !strings.Contains(stderr.String(), "--allow-implicit") {
				t.Fatalf("numbered implicit slot must refuse: err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
			stdout.Reset()
			stderr.Reset()
			cmd = newSlingCmd(&stdout, &stderr)
			cmd.SetArgs([]string{"frontend/" + provider, "FE-1", "--allow-implicit", "--no-convoy", "--no-formula"})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("explicit opt-in must route: err=%v stderr=%q", err, stderr.String())
			}
			b, err = store.Get("FE-1")
			if err != nil || b.Metadata["gc.routed_to"] != "frontend/"+provider {
				t.Fatalf("opt-in did not route: bead=%+v err=%v", b, err)
			}

			for _, suffix := range []string{"", "-1"} {
				stdout.Reset()
				stderr.Reset()
				cmd = newSlingCmd(&stdout, &stderr)
				cmd.SetArgs([]string{"frontend/" + provider + suffix, "write a new feature", "--no-convoy", "--no-formula", "--json"})
				if err := cmd.Execute(); err == nil || !json.Valid(stdout.Bytes()) || !strings.Contains(stdout.String(), "implicit_target_refused") {
					t.Fatalf("inline refusal must be structured: err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
				}
			}
			all, err := store.List(beads.ListQuery{AllowScan: true})
			if err != nil || len(all) != 1 {
				t.Fatalf("inline refusal created a bead: beads=%+v err=%v", all, err)
			}
		})
	}
}

func TestImplicitSlingRefusalCodeRule(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Prefix: "ci"},
		Rigs:      []config.Rig{{Name: "frontend", Prefix: "ci-code"}},
		Agents: []config.Agent{
			{Name: "worker-b", Dir: "frontend"},
			{Name: "worker-a", Dir: "frontend"},
			{Name: "elsewhere", Dir: "backend"},
			{Name: "codex", Dir: "frontend", Implicit: true},
		},
	}
	implicit := config.Agent{Name: "codex", Dir: "frontend", Implicit: true}
	explicit := implicit
	explicit.Implicit = false
	citySlot := implicit
	citySlot.Dir = ""
	for _, tc := range []struct {
		name   string
		a      config.Agent
		b      beads.Bead
		refuse bool
	}{
		{"rig work", implicit, beads.Bead{ID: "FE-1"}, true},
		{"numbered slot", config.Agent{Name: "codex-1", Provider: "codex", Dir: "frontend", Implicit: true}, beads.Bead{ID: "FE-1"}, true},
		{"city code", implicit, beads.Bead{ID: "ci-1"}, true},
		{"city operation", implicit, beads.Bead{ID: "ci-1", Labels: []string{"mayor-tool"}}, false},
		{"rig label is not exempt", implicit, beads.Bead{ID: "FE-1", Labels: []string{"mayor-tool"}}, true},
		{"longest prefix", implicit, beads.Bead{ID: "ci-code-1", Labels: []string{"mayor-tool"}}, true},
		{"explicit same name", explicit, beads.Bead{ID: "FE-1"}, false},
		{"city slot", citySlot, beads.Bead{ID: "FE-1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := implicitSlingRefusal(cfg, tc.a, tc.b)
			if (got != "") != tc.refuse {
				t.Fatalf("refusal = %q, want refusal=%v", got, tc.refuse)
			}
			if tc.refuse && (!strings.Contains(got, "frontend/worker-a, frontend/worker-b") || strings.Contains(got, "backend/elsewhere")) {
				t.Fatalf("refusal must list only configured rig lanes in order: %q", got)
			}
		})
	}
	cfg.Agents = nil
	if got := implicitSlingRefusal(cfg, implicit, beads.Bead{ID: "FE-1"}); !strings.Contains(got, "none; configure") {
		t.Fatalf("missing-lane diagnostic = %q", got)
	}
}
