package api

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/session"
)

type migratedResumeProvider struct {
	*runtime.Fake
	rejected int
}

func (p *migratedResumeProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if strings.Contains(cfg.Command, "expired-key") {
		p.rejected++
		return runtime.ErrSessionDiedDuringStartup
	}
	return p.Fake.Start(ctx, name, cfg)
}

func TestAPIWorkerRestartAfterACPProviderSwitch(t *testing.T) {
	for _, tc := range []struct{ name, command, resume, overrides, key, currentResume string }{
		{name: "MCP evidence", command: "/bin/echo"},
		{name: "stored ACP command", command: "/bin/echo acp --resume old-session"},
		{name: "stored resume command", command: "/bin/echo", resume: "/bin/echo acp"},
		{name: "invalid overrides fallback", command: "/bin/echo acp --resume old-session", overrides: "invalid-json"},
		{name: "expired generated resume", command: "/bin/echo acp", key: "expired-key"},
		{name: "expired explicit resume", command: "/bin/echo acp", key: "expired-key", currentResume: "/bin/echo --continue {{.SessionKey}}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newSessionFakeState(t)
			fs.cfg.Agents[0].Provider = "stub"
			fs.cfg.Agents[0].Session = "tmux"
			supportsACP := true
			fs.cfg.Providers["stub"] = config.ProviderSpec{Command: "/bin/echo", SupportsACP: &supportsACP, ACPArgs: []string{"acp"}, ResumeFlag: "--resume", ResumeStyle: "flag", ResumeCommand: tc.currentResume}
			b, err := fs.cityBeadStore.Create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{
				"template": "myrig/worker", "provider": "stub", "session_name": "legacy-session", "state": "asleep",
				"command": tc.command, "resume_command": tc.resume, "template_overrides": tc.overrides, "work_dir": fs.cityPath,
				"session_key": tc.key, "resume_flag": "--resume", "resume_style": "flag",
				session.MCPIdentityMetadataKey: "legacy-agent", session.MCPServersSnapshotMetadataKey: "[]",
			}})
			if err != nil {
				t.Fatal(err)
			}
			acp := runtime.NewFake()
			terminal := &migratedResumeProvider{Fake: fs.sp}
			sp := sessionauto.New(terminal, acp)
			sp.RouteACP("legacy-session")
			fs.sessionProvider = sp
			factory, err := New(fs).workerFactory(fs.cityBeadStore)
			if err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if tc.key != "" {
					if err := fs.cityBeadStore.SetMetadata(b.ID, "session_key", tc.key); err != nil {
						t.Fatal(err)
					}
				}
				handle, err := factory.SessionByID(b.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := handle.Start(context.Background()); err != nil {
					t.Fatal(err)
				}
				start := fs.sp.LastStartConfig("legacy-session")
				if start == nil {
					t.Fatal("terminal was not started")
				}
				if start.Command != "/bin/echo" || len(start.MCPServers) != 0 {
					t.Fatalf("terminal command = %q, MCP servers = %d, want interactive command without ACP hints", start.Command, len(start.MCPServers))
				}
				if acp.IsRunning("legacy-session") {
					t.Fatal("restarted on ACP")
				}
				updated, err := fs.cityBeadStore.Get(b.ID)
				if err != nil {
					t.Fatal(err)
				}
				if updated.Metadata[session.MCPIdentityMetadataKey] != "" || updated.Metadata[session.MCPServersSnapshotMetadataKey] != "" {
					t.Fatal("restart retained stale MCP metadata")
				}
				if tc.key != "" && (updated.Metadata["session_key"] != "" || terminal.rejected != attempt+1) {
					t.Fatalf("expired key recovery: key=%q rejected=%d, want empty key and %d resumed attempts", updated.Metadata["session_key"], terminal.rejected, attempt+1)
				}
				if err := handle.Stop(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestAPIWorkerRestartRefusesUnavailableACPReplacement(t *testing.T) {
	for _, phase := range []string{"resolve", "start"} {
		t.Run(phase, func(t *testing.T) {
			fs := newSessionFakeState(t)
			missingCommand := filepath.Join(fs.cityPath, "unavailable-terminal")
			fs.cfg.Agents[0].Provider = "replacement"
			fs.cfg.Agents[0].Session = "tmux"
			supportsACP := true
			fs.cfg.Providers["replacement"] = config.ProviderSpec{Command: missingCommand}
			fs.cfg.Providers["previous"] = config.ProviderSpec{Command: "/bin/echo", SupportsACP: &supportsACP, ACPArgs: []string{"acp"}}
			b, err := fs.cityBeadStore.Create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{
				"template": "myrig/worker", "provider": "previous", "session_name": "legacy-session", "state": "asleep",
				"command": "/bin/echo acp", "resume_command": "/bin/echo acp", "work_dir": fs.cityPath,
				"session_key": "saved-key", "resume_flag": "--resume", "resume_style": "flag",
				session.MCPIdentityMetadataKey: "legacy-agent", session.MCPServersSnapshotMetadataKey: "[]",
			}})
			if err != nil {
				t.Fatal(err)
			}
			acp := runtime.NewFake()
			sp := sessionauto.New(fs.sp, acp)
			sp.RouteACP("legacy-session")
			fs.sessionProvider = sp
			factory, err := New(fs).workerFactory(fs.cityBeadStore)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "start" {
				fs.cfg.Providers["replacement"] = config.ProviderSpec{Command: "/bin/echo"}
			}
			handle, err := factory.SessionByID(b.ID)
			if phase == "start" && err != nil {
				t.Fatalf("resolve available replacement: %v", err)
			}
			fs.cfg.Providers["replacement"] = config.ProviderSpec{Command: missingCommand}
			if err == nil {
				err = handle.Start(context.Background())
			}
			if err == nil || !strings.Contains(err.Error(), missingCommand) || !strings.Contains(err.Error(), "replacement") {
				t.Errorf("restart error = %v, want unavailable replacement provider and executable", err)
			}
			stored, err := fs.cityBeadStore.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			for key, value := range b.Metadata {
				if stored.Metadata[key] != value {
					t.Errorf("refused migration changed %s from %q to %q", key, value, stored.Metadata[key])
				}
			}
			if fs.sp.LastStartConfig("legacy-session") != nil || acp.LastStartConfig("legacy-session") != nil {
				t.Fatal("refused migration started a runtime")
			}
			// A direct start probes the retained route independently of bead metadata.
			if err := sp.Start(context.Background(), "legacy-session", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			if !acp.IsRunning("legacy-session") || fs.sp.IsRunning("legacy-session") {
				t.Fatal("refused migration removed the ACP route")
			}
		})
	}
}

type createTransportCapableProvider struct {
	*runtime.Fake
}

func (p *createTransportCapableProvider) SupportsTransport(transport string) bool {
	return transport == "acp"
}

func TestProviderSessionTransportUsesExplicitACPConfigOnCustomProvider(t *testing.T) {
	transport, err := providerSessionTransport(&config.ResolvedProvider{
		Name:        "custom-acp",
		SupportsACP: true,
		ACPCommand:  "/bin/echo",
	}, &createTransportCapableProvider{Fake: runtime.NewFake()})
	if err != nil {
		t.Fatalf("providerSessionTransport: %v", err)
	}
	if transport != "acp" {
		t.Fatalf("providerSessionTransport() = %q, want %q", transport, "acp")
	}
}

func TestProviderSessionTransportSupportsACPAloneStaysDefault(t *testing.T) {
	transport, err := providerSessionTransport(&config.ResolvedProvider{
		Name:        "custom-acp",
		SupportsACP: true,
	}, &createTransportCapableProvider{Fake: runtime.NewFake()})
	if err != nil {
		t.Fatalf("providerSessionTransport: %v", err)
	}
	if transport != "" {
		t.Fatalf("providerSessionTransport() = %q, want empty transport", transport)
	}
}

func TestValidateSessionTransportAcceptsTmuxTransport(t *testing.T) {
	transport, err := validateSessionTransport(&config.ResolvedProvider{
		Name: "custom",
	}, config.SessionTransportTmux, runtime.NewFake())
	if err != nil {
		t.Fatalf("validateSessionTransport: %v", err)
	}
	if transport != config.SessionTransportTmux {
		t.Fatalf("validateSessionTransport() = %q, want %q", transport, config.SessionTransportTmux)
	}
}

func TestValidateSessionTransportRejectsTmuxWhenSessionProviderIsACPOnly(t *testing.T) {
	_, err := validateSessionTransport(&config.ResolvedProvider{
		Name: "custom",
	}, config.SessionTransportTmux, &createTransportCapableProvider{Fake: runtime.NewFake()})
	if err == nil || !strings.Contains(err.Error(), "requires tmux transport") {
		t.Fatalf("validateSessionTransport() error = %v, want tmux routing error", err)
	}
}

func TestValidateSessionTransportRejectsUnknownTransport(t *testing.T) {
	_, err := validateSessionTransport(&config.ResolvedProvider{
		Name: "custom",
	}, "stdio", runtime.NewFake())
	if err == nil {
		t.Fatal("validateSessionTransport() error = nil, want unknown transport error")
	}
}

func TestResolveSessionTemplateForCreateUsesProviderACPDefault(t *testing.T) {
	fs := newSessionFakeState(t)
	supportsACP := true
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "worker",
			Dir:      "myrig",
			Provider: "custom-acp",
		}},
		Providers: map[string]config.ProviderSpec{
			"custom-acp": {
				Command:     "/bin/echo",
				PathCheck:   "true",
				SupportsACP: &supportsACP,
				ACPCommand:  "/bin/echo",
				ACPArgs:     []string{"acp"},
			},
		},
	}

	srv := New(fs)
	_, _, transport, _, err := srv.resolveSessionTemplateForCreate("myrig/worker")
	if err != nil {
		t.Fatalf("resolveSessionTemplateForCreate: %v", err)
	}
	if transport != "acp" {
		t.Fatalf("transport = %q, want %q", transport, "acp")
	}
}

func TestResolveSessionTemplateUsesProviderACPDefaultForLegacyRuntimeTransport(t *testing.T) {
	fs := newSessionFakeState(t)
	supportsACP := true
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "worker",
			Dir:      "myrig",
			Provider: "custom-acp",
		}},
		Providers: map[string]config.ProviderSpec{
			"custom-acp": {
				Command:     "/bin/echo",
				PathCheck:   "true",
				SupportsACP: &supportsACP,
				ACPCommand:  "/bin/echo",
				ACPArgs:     []string{"acp"},
			},
		},
	}

	srv := New(fs)
	_, _, transport, _, err := srv.resolveSessionTemplate("myrig/worker")
	if err != nil {
		t.Fatalf("resolveSessionTemplate: %v", err)
	}
	if transport != "acp" {
		t.Fatalf("transport = %q, want %q", transport, "acp")
	}
}

func TestConfiguredSessionTransportUsesProviderACPDefaultForAgentTemplates(t *testing.T) {
	supportsACP := true
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "worker",
			Dir:      "myrig",
			Provider: "custom-acp",
		}},
		Providers: map[string]config.ProviderSpec{
			"custom-acp": {
				Command:     "/bin/echo",
				PathCheck:   "true",
				SupportsACP: &supportsACP,
				ACPCommand:  "/bin/echo",
				ACPArgs:     []string{"acp"},
			},
		},
	}

	transport := configuredSessionTransport(cfg, "myrig/worker", "")
	if transport != "acp" {
		t.Fatalf("configuredSessionTransport() = %q, want %q", transport, "acp")
	}
}

func TestBuildSessionResumeDoesNotInferProviderACPDefaultForStoppedLegacyTemplateSession(t *testing.T) {
	fs := newSessionFakeState(t)
	supportsACP := true
	fs.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:     "worker",
			Dir:      "myrig",
			Provider: "custom-acp",
		}},
		Providers: map[string]config.ProviderSpec{
			"custom-acp": {
				Command:     "/bin/echo",
				PathCheck:   "true",
				SupportsACP: &supportsACP,
				ACPCommand:  "/bin/echo",
				ACPArgs:     []string{"acp"},
			},
		},
	}

	srv := New(fs)
	cmd, _, err := srv.buildSessionResume(session.Info{
		ID:       "gc-1",
		Template: "myrig/worker",
		Command:  "/bin/echo",
		WorkDir:  "/tmp/workdir",
	})
	if err != nil {
		t.Fatalf("buildSessionResume: %v", err)
	}
	if cmd != "/bin/echo" {
		t.Fatalf("resume command = %q, want %q", cmd, "/bin/echo")
	}
}

func TestResolvedSessionRuntimeCommandReplaysTemplateOverrides(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	resolved := &config.ResolvedProvider{
		Name:    "custom",
		Command: "/bin/echo",
		OptionsSchema: []config.ProviderOption{{
			Key:  "effort",
			Type: "select",
			Choices: []config.OptionChoice{{
				Value:    "high",
				FlagArgs: []string{"--effort", "high"},
			}},
		}},
	}

	command, err := srv.resolvedSessionRuntimeCommand(
		resolved,
		"",
		"/bin/echo",
		map[string]string{"template_overrides": `{"effort":"high","initial_message":"hello"}`},
	)
	if err != nil {
		t.Fatalf("resolvedSessionRuntimeCommand: %v", err)
	}
	if command != "/bin/echo --effort high" {
		t.Fatalf("command = %q, want %q", command, "/bin/echo --effort high")
	}
}

func TestShouldPreserveStoredRuntimeCommandForTransportRejectsExecutableOnlyMatch(t *testing.T) {
	if shouldPreserveStoredRuntimeCommandForTransport(
		"claude",
		"claude --settings /tmp/settings.json",
		"",
		nil,
	) {
		t.Fatal("shouldPreserveStoredRuntimeCommandForTransport() = true, want false")
	}
}
