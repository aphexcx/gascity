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
	for _, provider := range []string{"claude", "claude-opus", "claude-hub", "codex", "codex-astra"} {
		t.Run(provider, func(t *testing.T) {
			cityDir := setupCmdSlingBeadExistsFixture(t)
			configPath := filepath.Join(cityDir, "city.toml")
			content, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			content = append(content, []byte("\n[providers.claude]\nbase = \"builtin:claude\"\n[providers.codex]\nbase = \"builtin:codex\"\n[providers.codex-astra]\nbase = \"builtin:codex\"\n")...)
			content = append(content, []byte("\n[providers.claude-opus]\nbase = \"builtin:claude\"\n[providers.claude-hub]\nbase = \"builtin:claude\"\n")...)
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
			if strings.HasPrefix(provider, "claude-") && !strings.Contains(stderr.String(), "(base builtin:claude)") {
				t.Fatalf("wrapper refusal must name the builtin base: %q", stderr.String())
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
			b, err = store.Get("FE-1")
			if err != nil || b.Metadata["gc.routed_to"] != "" {
				t.Fatalf("numbered refusal changed routing: bead=%+v err=%v", b, err)
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

func TestCmdSlingAllowsConfiguredProviderLane(t *testing.T) {
	cityDir := setupCmdSlingBeadExistsFixture(t)
	configPath := filepath.Join(cityDir, "city.toml")
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	content = append(content, []byte(`
[providers.claude-opus]
base = "builtin:claude"

[[agent]]
name = "claude-opus"
provider = "claude-opus"
dir = "frontend"
`)...)
	if err := os.WriteFile(configPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(cityDir, "frontend")
	writeTestFileStoreBeads(t, rigDir, []beads.Bead{{ID: "FE-1", Title: "Code change", Status: "open", Type: "task"}})
	var stdout, stderr bytes.Buffer
	cmd := newSlingCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"frontend/claude-opus", "FE-1", "--no-convoy", "--no-formula"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("configured lane must route without opt-in: err=%v stderr=%q", err, stderr.String())
	}
	store, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Get("FE-1")
	if err != nil || b.Metadata["gc.routed_to"] != "frontend/claude-opus" {
		t.Fatalf("configured lane did not route: bead=%+v err=%v", b, err)
	}
}

func TestCmdSlingRejectsCityConvoyOnImplicitRigSlot(t *testing.T) {
	cityDir := setupCmdSlingBeadExistsFixture(t)
	configPath := filepath.Join(cityDir, "city.toml")
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	content = []byte(strings.Replace(string(content), "name = \"demo\"", "name = \"demo\"\nprefix = \"ci\"", 1) + "\n[providers.codex]\nbase = \"builtin:codex\"\n")
	if err := os.WriteFile(configPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestFileStoreBeads(t, cityDir, []beads.Bead{
		{ID: "ci-1", Title: "Operations", Status: "open", Type: "convoy", Labels: []string{"mayor-tool"}},
		{ID: "ci-2", ParentID: "ci-1", Title: "Allowed operation", Status: "open", Type: "task", Labels: []string{"mayor-tool"}},
		{ID: "ci-3", ParentID: "ci-1", Title: "Code change", Status: "open", Type: "task"},
	})
	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, flag, refusal string }{
		{"exact review command", "", "refusing cross-rig route: bead ci-1"},
		{"force still cannot cross stores", "--force", "refusing cross-store route: bead ci-3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			cmd := newSlingCmd(&stdout, &stderr)
			args := []string{"frontend/codex", "ci-1", "--no-formula"}
			if tc.flag != "" {
				args = append(args, tc.flag)
			}
			cmd.SetArgs(args)
			if err := cmd.Execute(); err == nil || !strings.Contains(stderr.String(), tc.refusal) {
				t.Fatalf("convoy must refuse: err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
			for _, id := range []string{"ci-1", "ci-2", "ci-3"} {
				b, err := store.Get(id)
				if err != nil {
					t.Fatal(err)
				}
				if b.Metadata["gc.routed_to"] != "" || b.Assignee != "" || b.Status != "open" {
					t.Fatalf("refusal mutated %s: %+v", id, b)
				}
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
		{"custom provider", config.Agent{Name: "custom", Provider: "custom", Dir: "frontend", Implicit: true}, beads.Bead{ID: "FE-1"}, true},
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

func TestImplicitSlingRefusalWrapperBase(t *testing.T) {
	base := "builtin:claude"
	parent := "provider:wrapped"
	cfg := &config.City{Providers: map[string]config.ProviderSpec{
		"wrapped": {Base: &base},
		"nested":  {Base: &parent},
	}}
	for _, name := range []string{"wrapped", "wrapped-1", "nested"} {
		t.Run(name, func(t *testing.T) {
			a := config.Agent{Name: name, Provider: strings.TrimSuffix(name, "-1"), Dir: "frontend", Implicit: true}
			got := implicitSlingRefusal(cfg, a, beads.Bead{ID: "FE-1"})
			want := `implicit provider slot "frontend/` + name + `" (base builtin:claude)`
			if !strings.Contains(got, want) {
				t.Fatalf("refusal = %q, want %q", got, want)
			}
		})
	}
}
