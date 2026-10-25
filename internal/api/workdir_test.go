package api

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

func TestCanAttributeSessionRetainedPoolWorkDir(t *testing.T) {
	const pattern = ".worktrees/{{.Rig}}/lane-{{.AgentBase}}"
	named := config.Agent{Name: "worker-2", Dir: "demo", WorkDir: pattern, MaxActiveSessions: intPtr(1)}
	cfg := &config.City{
		Workspace: config.Workspace{Provider: "claude"},
		Rigs:      []config.Rig{{Name: "demo", Path: "/repos/demo"}},
		Agents:    []config.Agent{named, {Name: "worker", Dir: "demo", WorkDir: pattern, MaxActiveSessions: intPtr(2)}},
	}
	for _, tc := range []struct {
		name string
		info session.Info
		want bool
	}{
		{"retained canonical", session.Info{Template: "demo/worker", WorkDirCanonical: "/city/.worktrees/demo/lane-worker-2"}, false},
		{"retained legacy relative", session.Info{Template: "demo/worker", WorkDir: ".worktrees/demo/lane-worker-2"}, false},
		{"historical transcript", session.Info{Template: "demo/worker", WorkDir: "/city/.worktrees/demo/lane-worker-2", Closed: true}, false},
		{"codex transcript", session.Info{Template: "demo/worker", Provider: "codex", WorkDir: "/city/.worktrees/demo/lane-worker-2"}, true},
		{"historical wrapped codex", session.Info{Template: "demo/worker", Provider: "custom", BuiltinAncestor: "codex", WorkDir: "/city/.worktrees/demo/lane-worker-2", Closed: true}, true},
		{"wrapped claude transcript", session.Info{Template: "demo/worker", Provider: "custom", ProviderKind: "claude", WorkDir: "/city/.worktrees/demo/lane-worker-2"}, false},
		{"unknown provider transcript", session.Info{Template: "demo/worker", Provider: "custom", WorkDir: "/city/.worktrees/demo/lane-worker-2"}, false},
		{"migrated", session.Info{Template: "demo/worker", WorkDirCanonical: "/city/.worktrees/demo/lane-worker.pool-2", WorkDir: "/city/.worktrees/demo/lane-worker-2"}, true},
		{"own session", session.Info{Template: "demo/worker-2", WorkDir: "/city/.worktrees/demo/lane-worker-2"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := canAttributeSession(named, named.QualifiedName(), cfg, "/city", tc.info); got != tc.want {
				t.Fatalf("canAttributeSession = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCanAttributeSessionUsesResolvedWorkDir(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gastown", Provider: "claude"},
		Rigs:      []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}},
		Agents: []config.Agent{
			{Name: "refinery", Dir: "demo", WorkDir: ".gc/worktrees/{{.Rig}}/refinery", MaxActiveSessions: intPtr(1)},
			{Name: "witness", Dir: "demo", WorkDir: ".gc/agents/{{.Rig}}/witness", MaxActiveSessions: intPtr(1)},
		},
	}

	if !canAttributeSession(cfg.Agents[0], "demo/refinery", cfg, cityPath) {
		t.Fatal("canAttributeSession() = false, want true for distinct workdirs")
	}
}

func TestCanAttributeSessionRejectsSharedRigRootWhenClaudePoolExists(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot := filepath.Join(t.TempDir(), "demo-repo")
	cfg := &config.City{
		Workspace: config.Workspace{Provider: "claude"},
		Rigs:      []config.Rig{{Name: "demo", Path: rigRoot}},
		Agents: []config.Agent{
			{Name: "refinery", Dir: "demo", MaxActiveSessions: intPtr(1)},
			{Name: "polecat", Dir: "demo", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)},
		},
	}

	if canAttributeSession(cfg.Agents[0], "demo/refinery", cfg, cityPath) {
		t.Fatal("canAttributeSession() = true, want false when Claude pool shares rig root")
	}
}

func TestCanAttributeSessionRejectsSharedPoolTemplateEvenWhenItMentionsAgentIdentity(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Provider: "claude"},
		Rigs:      []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}},
		Agents: []config.Agent{
			{Name: "refinery", Dir: "demo", WorkDir: ".gc/shared", MaxActiveSessions: intPtr(1)},
			{
				Name:              "polecat",
				Dir:               "demo",
				WorkDir:           `{{if .AgentBase}}.gc/shared{{end}}`,
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2),
			},
		},
	}

	if canAttributeSession(cfg.Agents[0], "demo/refinery", cfg, cityPath) {
		t.Fatal("canAttributeSession() = true, want false when pooled template still resolves to a shared path")
	}
}

func TestCanAttributeSessionRejectsSharedSingleSlotPoolTemplate(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Provider: "claude"},
		Rigs:      []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}},
		Agents: []config.Agent{
			{Name: "observer", Dir: "demo", WorkDir: ".gc/shared/polecat", MaxActiveSessions: intPtr(1)},
			{
				Name:              "polecat",
				Dir:               "demo",
				WorkDir:           ".gc/shared/{{.AgentBase}}",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1),
			},
		},
	}

	if canAttributeSession(cfg.Agents[0], "demo/observer", cfg, cityPath) {
		t.Fatal("canAttributeSession() = true, want false when a single-slot pool shares the observed workdir")
	}
}

func TestResolveSessionTemplateUsesConfiguredWorkDir(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Agents[0].WorkDir = ".gc/worktrees/{{.Rig}}/{{.AgentBase}}"
	srv := New(state)

	_, workDir, _, _, err := srv.resolveSessionTemplate("myrig/worker")
	if err != nil {
		t.Fatalf("resolveSessionTemplate: %v", err)
	}

	want := filepath.Join(state.cityPath, ".gc", "worktrees", "myrig", "worker")
	if workDir != want {
		t.Fatalf("resolveSessionTemplate() workDir = %q, want %q", workDir, want)
	}
}

func TestResolveSessionTemplateUsesCityNameFallbackForWorkDirTemplates(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Workspace.Name = ""
	state.cfg.Agents[0].WorkDir = ".gc/agents/{{.CityName}}/{{.AgentBase}}"
	srv := New(state)

	_, workDir, _, _, err := srv.resolveSessionTemplate("myrig/worker")
	if err != nil {
		t.Fatalf("resolveSessionTemplate: %v", err)
	}

	want := filepath.Join(state.cityPath, ".gc", "agents", filepath.Base(state.cityPath), "worker")
	if workDir != want {
		t.Fatalf("resolveSessionTemplate() workDir = %q, want %q", workDir, want)
	}
}

func TestResolveSessionTemplateUsesQualifiedNameForWorkDirTemplates(t *testing.T) {
	state := newFakeState(t)
	state.cfg.Agents[0].WorkDir = ".gc/worktrees/{{.Agent}}"
	srv := New(state)

	_, workDir, _, _, err := srv.resolveSessionTemplate("myrig/worker")
	if err != nil {
		t.Fatalf("resolveSessionTemplate: %v", err)
	}

	want := filepath.Join(state.cityPath, ".gc", "worktrees", "myrig", "worker")
	if workDir != want {
		t.Fatalf("resolveSessionTemplate() workDir = %q, want %q", workDir, want)
	}
}
