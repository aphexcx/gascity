package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

func entryLaunchContextFixture(t *testing.T) (string, *config.City) {
	t.Helper()
	cityPath := t.TempDir()
	writePhase0InterfaceCity(t, cityPath, `[workspace]
name = "test-city"
[beads]
provider = "file"
[[agent]]
name = "runner"
provider = "test-provider"
work_dir = "work/{{.AgentBase}}"
session_setup = ["echo {{.Session}} {{.WorkDir}} {{.Agent}}"]
[agent.pool]
min = 0
max = 4
[providers.test-provider]
command = "/bin/echo"
prompt_mode = "none"
`)
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	return cityPath, cfg
}

func TestWorkerEntryLaunchPreservesSessionContext(t *testing.T) {
	for _, name := range []string{"resume", "create", "create-automatic-name"} {
		t.Run(name, func(t *testing.T) {
			create := name != "resume"
			cityPath, cfg := entryLaunchContextFixture(t)
			store, sp := beads.NewMemStore(), runtime.NewFake()
			workDir := filepath.Join(cityPath, "chosen-workspace")
			metadata := map[string]string{"session_origin": "manual", "agent_name": "task-run", "gc.trigger_bead_id": "task-123", "gc.trigger_bead_store_ref": "rig:test"}
			resolved, err := config.ResolveProvider(&cfg.Agents[0], &cfg.Workspace, cfg.Providers, func(s string) (string, error) { return s, nil })
			if err != nil {
				t.Fatal(err)
			}
			var h worker.Handle
			runtimeName := "task-runtime"
			if name == "create-automatic-name" {
				runtimeName = ""
			}
			if create {
				h, err = newWorkerSessionHandleForResolvedRuntimeWithConfig(cityPath, store, sp, cfg, "task-run", runtimeName, "runner", "test", "/bin/echo", "test-provider", workDir, "", resolved, metadata)
			} else {
				mgr := session.NewManagerWithOptions(store, sp)
				info, createErr := mgr.CreateSession(t.Context(), session.CreateOptions{BeadOnly: true, Alias: "task-run", ExplicitName: "task-runtime", Template: "runner", Provider: "test-provider", Command: "/bin/echo", WorkDir: workDir, ExtraMeta: metadata})
				if createErr != nil {
					t.Fatal(createErr)
				}
				h, err = workerHandleForSessionWithConfig(cityPath, store, sp, cfg, info.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if create {
				var info session.Info
				info, err = h.Create(t.Context(), worker.CreateModeStarted)
				runtimeName = info.SessionName
			} else {
				err = h.Start(t.Context())
			}
			if err != nil {
				t.Fatal(err)
			}
			launch := sp.LastStartConfig(runtimeName)
			if launch == nil {
				t.Fatal("runtime was not started")
			}
			if create {
				if launch.WorkDir != workDir || len(launch.SessionSetup) != 0 {
					t.Error("direct create changed its supplied base launch")
				}
				if name == "create-automatic-name" && !strings.HasPrefix(runtimeName, "s-") {
					t.Error("direct create lost its bead-derived name")
				}
				return
			}
			if launch.WorkDir != workDir || launch.Env["GC_AGENT"] != "task-run" || !strings.Contains(strings.Join(launch.SessionSetup, " "), runtimeName+" "+workDir+" task-run") {
				t.Errorf("session context lost: runtime=%q workdir preserved=%t agent=%q setup=%q", runtimeName, launch.WorkDir == workDir, launch.Env["GC_AGENT"], strings.ReplaceAll(strings.Join(launch.SessionSetup, " "), cityPath, "<city>"))
			}
			if launch.Env["GC_TRIGGER_BEAD_ID"] != "task-123" {
				t.Error("stored trigger context lost")
			}
		})
	}
}

func TestCLIStartedCreatesRetainBaseNaming(t *testing.T) {
	for _, name := range []string{"singleton-materialize", "aliased-pool", "tmux-alias", "adhoc-pool", "configured-named"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("GC_SESSION", "fake")
			t.Setenv("GC_BEADS", "file")
			cityPath := t.TempDir()
			extra := ""
			if name == "singleton-materialize" {
				extra = "max_active_sessions = 1"
			}
			if name == "aliased-pool" || name == "adhoc-pool" {
				extra = "max_active_sessions = 3"
			}
			if name == "tmux-alias" {
				extra = `tmux_alias = "chosen-runtime"`
			}
			if name == "configured-named" {
				extra = "[[named_session]]\nname = \"seat\"\ntemplate = \"runner\"\nmode = \"on_demand\""
			}
			writePhase0InterfaceCity(t, cityPath, fmt.Sprintf("[workspace]\nname = \"test-city\"\n[beads]\nprovider = \"file\"\n[[agent]]\nname = \"runner\"\nstart_command = \"true\"\nprompt_template = \"missing-launch-only.md\"\n%s\n", extra))
			t.Setenv("GC_CITY", cityPath)
			t.Setenv("GC_DIR", t.TempDir())
			cfg, err := loadCityConfig(cityPath)
			if err != nil {
				t.Fatal(err)
			}
			var b beads.Bead
			if name == "aliased-pool" || name == "tmux-alias" {
				alias := ""
				if name == "aliased-pool" {
					alias = "task-run"
				}
				var stdout, stderr bytes.Buffer
				if code := cmdSessionNew([]string{"runner"}, alias, "test", "", true, false, 0, &stdout, &stderr); code != 0 {
					t.Fatalf("create exit=%d: %s", code, stderr.String())
				}
				b = onlySessionBead(t, cityPath)
			} else {
				store := beads.NewMemStore()
				var runtimeName string
				if name == "configured-named" {
					runtimeName, err = materializeSessionForTemplateWithOptions(cityPath, cfg, store, "seat", nil, ensureSessionForTemplateOptions{})
				} else {
					runtimeName, err = materializeSessionForAgentConfig(cityPath, cfg, store, &cfg.Agents[0])
				}
				if err != nil {
					t.Fatal(err)
				}
				id, resolveErr := session.ResolveSessionID(store, runtimeName)
				if resolveErr != nil {
					t.Fatal(resolveErr)
				}
				b, err = store.Get(id)
				if err != nil {
					t.Fatal(err)
				}
			}
			runtimeName, explicit := b.Metadata["session_name"], b.Metadata["session_name_explicit"]
			switch name {
			case "singleton-materialize", "aliased-pool":
				if runtimeName != "s-"+b.ID || explicit != "" {
					t.Fatalf("bead-derived name changed: name=%q explicit=%q", runtimeName, explicit)
				}
			case "tmux-alias":
				if runtimeName != "chosen-runtime" || explicit != "true" {
					t.Fatalf("tmux alias changed: name=%q explicit=%q", runtimeName, explicit)
				}
			case "adhoc-pool":
				if !strings.HasPrefix(runtimeName, "runner-adhoc-") || explicit != "true" {
					t.Fatalf("ad-hoc identity changed: name=%q explicit=%q", runtimeName, explicit)
				}
			case "configured-named":
				if runtimeName != config.NamedSessionRuntimeName(cfg.Workspace.Name, cfg.Workspace, "seat") || explicit != "true" {
					t.Fatalf("configured identity changed: name=%q explicit=%q", runtimeName, explicit)
				}
			}
			if b.Metadata["creation_complete_at"] == "" || b.Metadata["awake_started_at"] == "" || b.Metadata["pending_create_claim"] != "" {
				t.Error("base creation stamps changed")
			}
		})
	}
}

func TestCLIReadControlsIgnoreInvalidLaunchInputs(t *testing.T) {
	for _, input := range []string{"missing-prompt", "undeclared-upstream"} {
		t.Run(input, func(t *testing.T) {
			cityPath, cfg := entryLaunchContextFixture(t)
			if input == "missing-prompt" {
				cfg.Agents[0].PromptTemplate = "absent-launch-prompt.md"
			} else {
				cfg.Agents[0].Upstream = "absent-upstream"
			}
			store, sp := beads.NewMemStore(), runtime.NewFake()
			mgr := session.NewManagerWithOptions(store, sp)
			info, err := mgr.CreateSession(t.Context(), session.CreateOptions{Template: "runner", Provider: "test-provider", Command: "/bin/echo", WorkDir: cityPath, ExtraMeta: map[string]string{"real_world_app_session_kind": "agent"}})
			if err != nil {
				t.Fatal(err)
			}
			h, err := workerHandleForSessionWithConfig(cityPath, store, sp, cfg, info.ID)
			if err != nil {
				t.Fatalf("handle construction used invalid launch input: %v", err)
			}
			if _, err := h.Peek(t.Context(), 10); err != nil {
				t.Fatal(err)
			}
			if _, err := h.History(t.Context(), worker.HistoryRequest{}); err != nil && !errors.Is(err, worker.ErrHistoryUnavailable) {
				t.Fatal(err)
			}
			if _, err := h.LiveObservation(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := h.Stop(t.Context()); err != nil {
				t.Fatal(err)
			}
			if input == "missing-prompt" {
				return // The canonical builder permits an absent prompt template.
			}
			starts := sp.CountCalls("Start", info.SessionName)
			if err := h.Start(t.Context()); err == nil {
				t.Fatal("start accepted invalid launch input")
			}
			if sp.CountCalls("Start", info.SessionName) != starts {
				t.Fatal("invalid launch reached runtime Start")
			}
		})
	}
}

func TestEntryLaunchOptionCanRemoveDefaultFlag(t *testing.T) {
	cityPath, cfg := entryLaunchContextFixture(t)
	p := cfg.Providers["test-provider"]
	p.OptionsSchema = []config.ProviderOption{{Key: "feature", Type: "select", Default: "on", Choices: []config.OptionChoice{
		{Value: "on", FlagArgs: []string{"--feature"}}, {Value: "off"},
	}}}
	cfg.Providers["test-provider"] = p
	metadata := map[string]string{"real_world_app_session_kind": "agent", "template_overrides": `{"feature":"off"}`}
	info := session.Info{Template: "runner", Provider: "test-provider", WorkDir: cityPath, ConfiguredNamedSession: true}
	base, err := resolvedWorkerRuntimeWithConfigAndMetadata(cityPath, cfg, info, "agent", metadata)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(base.Command, "--feature") {
		t.Fatal("base control retained the default flag")
	}
	launch, err := buildWorkerEntryLaunch(cityPath, cfg, beads.NewMemStore(), runtime.NewFake(), info, "agent", metadata, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(launch.Command, "--feature") {
		t.Fatal("full entry launch retained a flag explicitly disabled by the session")
	}
}

func TestEntryLaunchPreservesStoredTransport(t *testing.T) {
	for _, stored := range []string{"tmux", "acp"} {
		t.Run(stored, func(t *testing.T) {
			cityPath, cfg := entryLaunchContextFixture(t)
			p := cfg.Providers["test-provider"]
			supportsACP := true
			p.SupportsACP = &supportsACP
			p.ACPCommand = "/bin/echo"
			p.ACPArgs = []string{"acp-only"}
			p.Args = []string{"cli-only"}
			cfg.Providers["test-provider"] = p
			cfg.Agents[0].Session = "acp"
			if stored == "acp" {
				cfg.Agents[0].Session = "tmux"
			}
			info := session.Info{Template: "runner", Provider: "test-provider", WorkDir: cityPath, Transport: stored, ConfiguredNamedSession: true}
			metadata := map[string]string{"real_world_app_session_kind": "agent", "session_transport": stored}
			base, err := resolvedWorkerRuntimeWithConfigAndMetadata(cityPath, cfg, info, "agent", metadata)
			if err != nil {
				t.Fatal(err)
			}
			var sp runtime.Provider = runtime.NewFake()
			if stored == "acp" {
				sp = &transportCapableSessionProvider{runtime.NewFake()}
			}
			launch, err := buildWorkerEntryLaunch(cityPath, cfg, beads.NewMemStore(), sp, info, "agent", metadata, true)
			if err != nil {
				t.Fatal(err)
			}
			if launch.Command != base.Command {
				t.Fatalf("transport changed: command=%q base=%q", launch.Command, base.Command)
			}
		})
	}
}

func TestCLIEntryLaunchClearsCallerContext(t *testing.T) {
	cityPath, cfg := entryLaunchContextFixture(t)
	keys := []string{"GC_SESSION_ID", "GC_SESSION_NAME", "GC_SESSION_KEY", "GC_RUNTIME_EPOCH", "GC_CONTINUATION_EPOCH", "GC_INSTANCE_TOKEN", "GC_ALIAS", "GC_TEMPLATE", "GC_SESSION_ORIGIN", "GC_AGENT", "GC_SPAWN_ORIGIN", "GC_WORKTREE_BEAD_ID", "GC_PACKER_PACK", "GC_DIR", "GC_PRESTART_LOG", "GC_STARTUP_PROMPT_DELIVERED", "GC_TRIGGER_BEAD_ID", "GC_TRIGGER_FUTURE_KEY", "GC_READY_PROMPT_PREFIX", "GC_WORKTREE_CLAIM_GUARD"}
	for _, key := range keys {
		t.Setenv(key, "calling-session")
	}
	t.Setenv("GC_TEST_AMBIENT_SWEEP", "keep")
	info := session.Info{Template: "runner", Alias: "destination", SessionNameMetadata: "destination-runtime", SessionNameExplicit: "true", Provider: "test-provider", SessionOrigin: "manual", WorkDir: cityPath}
	launch, err := buildWorkerEntryLaunch(cityPath, cfg, beads.NewMemStore(), runtime.NewFake(), info, "agent", map[string]string{"session_origin": "manual"}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if launch.Hints.Env[key] == "calling-session" {
			t.Errorf("caller key %s reached destination", key)
		}
	}
	if launch.Hints.Env["GC_TEST_AMBIENT_SWEEP"] != "keep" {
		t.Error("ambient key lost")
	}
}

func TestEntryLaunchProviderSessionKeepsCheapResolver(t *testing.T) {
	cityPath, cfg := entryLaunchContextFixture(t)
	cfg.Agents[0].Upstream = "undeclared"
	info := session.Info{Template: "runner", Provider: "test-provider", Command: "/bin/echo", WorkDir: cityPath}
	launch, err := buildWorkerEntryLaunch(cityPath, cfg, beads.NewMemStore(), runtime.NewFake(), info, "provider", map[string]string{"real_world_app_session_kind": "provider"}, true)
	if err != nil || launch == nil || launch.Command != "/bin/echo" {
		t.Fatalf("provider session used agent launch: launch=%v err=%v", launch != nil, err)
	}
}

func TestWorkerEntryLaunchRetainsResumeSettings(t *testing.T) {
	for _, explicitCommand := range []bool{false, true} {
		name := "flag"
		if explicitCommand {
			name = "command"
		}
		t.Run(name, func(t *testing.T) {
			cityPath, cfg := entryLaunchContextFixture(t)
			store, sp := beads.NewMemStore(), runtime.NewFake()
			metadata := map[string]string{
				"template": "runner", "provider": "test-provider", "command": "/bin/echo",
				"state": "asleep", "session_name": "resume-runtime", "session_key": "resume-key",
				"resume_flag": "--resume", "resume_style": "flag", "session_origin": "manual", "work_dir": cityPath,
				"agent_name": "runner-2", "real_world_app_session_kind": "agent",
			}
			want := "/bin/echo --resume resume-key"
			if explicitCommand {
				metadata["resume_command"] = "/bin/echo continue {{.SessionKey}}"
				want = "/bin/echo continue resume-key"
			}
			b, err := store.Create(beads.Bead{Type: session.BeadType, Metadata: metadata})
			if err != nil {
				t.Fatal(err)
			}
			h, err := workerHandleForSessionWithConfig(cityPath, store, sp, cfg, b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := h.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			launch := sp.LastStartConfig("resume-runtime")
			if launch == nil || launch.Command != want {
				t.Fatalf("resume command lost: launch=%+v want=%q", launch, want)
			}
		})
	}
}

func TestWorkerEntryLaunchResumePromptDelivery(t *testing.T) {
	cityPath, cfg := entryLaunchContextFixture(t)
	promptFile := filepath.Join(cityPath, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("startup instructions"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Agents[0].PromptTemplate = promptFile
	provider := cfg.Providers["test-provider"]
	provider.PromptMode = "arg"
	provider.ResumeFlag = "--resume"
	cfg.Providers["test-provider"] = provider
	for _, resumed := range []bool{false, true} {
		name := "first-start"
		if resumed {
			name = "resume"
		}
		t.Run(name, func(t *testing.T) {
			info := session.Info{Template: "runner", Provider: "test-provider", AgentName: "runner-2", SessionNameMetadata: "prompt-runtime", State: session.StateStartPending, SessionKey: "key-123"}
			if resumed {
				info.State = session.StateAsleep
				info.StartedConfigHash = "existing-hash"
			}
			launch, err := buildWorkerEntryLaunch(cityPath, cfg, beads.NewMemStore(), runtime.NewFake(), info, "agent", map[string]string{"real_world_app_session_kind": "agent"}, true)
			if err != nil {
				t.Fatal(err)
			}
			if resumed {
				if launch.Hints.PromptSuffix != "" || launch.Hints.PromptFlag != "" || !strings.Contains(launch.Hints.Nudge, "startup instructions") {
					t.Errorf("resume prompt must use restart delivery: suffix=%q flag=%q nudge=%q", launch.Hints.PromptSuffix, launch.Hints.PromptFlag, launch.Hints.Nudge)
				}
			} else if !strings.Contains(launch.Hints.PromptSuffix, "startup instructions") {
				t.Error("first start lost its configured prompt delivery")
			}
		})
	}
}
