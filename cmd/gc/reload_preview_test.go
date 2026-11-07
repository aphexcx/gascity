package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestReloadSessionChangesPreservesSoftAndDriftSemantics(t *testing.T) {
	oldHash := runtime.CoreFingerprint(runtime.Config{Command: "old"})
	desired := map[string]TemplateParams{
		"pool-1":    {Command: "new", TemplateName: "pool"},
		"same":      {Command: "old", TemplateName: "pool"},
		"unstarted": {Command: "new", TemplateName: "pool"},
		"legacy":    {Command: "new", TemplateName: "pool"},
	}
	infos := []session.Info{
		{ID: "one", Template: "pool", SessionNameMetadata: "pool-1", StartedConfigHash: oldHash},
		{ID: "two", Template: "removed", SessionNameMetadata: "orphan", StartedConfigHash: oldHash},
		{ID: "three", Template: "pool", SessionNameMetadata: "same", StartedConfigHash: oldHash},
		{ID: "four", Template: "pool", SessionNameMetadata: "unstarted"},
		{ID: "five", Template: "pool", SessionNameMetadata: "legacy", StartedConfigHash: "old-unversioned"},
	}
	cfg := &config.City{Agents: []config.Agent{{Name: "pool"}}}
	for _, soft := range []bool{false, true} {
		got := reloadSessionChanges(infos, desired, cfg, soft)
		action := "drain"
		legacyAction := "rebaseline"
		if soft {
			action, legacyAction = "accept", "accept"
		}
		want := map[string]string{"pool-1": action, "orphan": "drain", "legacy": legacyAction}
		if len(got) != len(want) {
			t.Fatalf("soft=%v changes=%+v", soft, got)
		}
		for _, change := range got {
			if change.Kind != "session" || change.Action != want[change.Name] || len(change.Reasons) == 0 {
				t.Errorf("soft=%v unexpected change=%+v", soft, change)
			}
		}
	}
	if infos[0].StartedConfigHash != oldHash {
		t.Fatal("preview accepted drift")
	}
}

func TestReloadPreviewResolvesChangedTemplateWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	writeTemplateResolveCityConfig(t, dir, "file")
	fs := fsys.NewFake()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "echo old"}}}
	bp := &agentBuildParams{
		cityName: "test", cityPath: dir, fs: fs, workspace: &config.Workspace{Provider: "test"},
		providers: map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:  func(string) (string, error) { return "/bin/echo", nil }, stderr: io.Discard,
	}
	infos := []session.Info{{ID: "one", Template: "worker", SessionNameMetadata: "worker-1"}}
	desired, _, err := resolveReloadSessionTemplates(bp, cfg, infos)
	if err != nil {
		t.Fatal(err)
	}
	infos[0].StartedConfigHash = runtime.CoreFingerprint(sessionCoreConfigForHashInfo(desired["worker-1"], infos[0]))
	if changes := reloadSessionChanges(infos, desired, cfg, false); len(changes) != 0 {
		t.Fatalf("unchanged template reported drift: %+v", changes)
	}
	cfg.Agents[0].StartCommand = "echo new"
	desired, _, err = resolveReloadSessionTemplates(bp, cfg, infos)
	if err != nil {
		t.Fatal(err)
	}
	changes := reloadSessionChanges(infos, desired, cfg, false)
	if len(changes) != 1 || changes[0].Name != "worker-1" || changes[0].Action != "drain" {
		t.Fatalf("re-rendered template=%+v", changes)
	}
	for _, call := range fs.Calls {
		if call.Method == "WriteFile" || call.Method == "MkdirAll" || call.Method == "Rename" {
			t.Fatalf("preview mutated filesystem: %+v", call)
		}
	}
}

func TestReloadDryRunRejectsInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(path, []byte("[invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	cr := &CityRuntime{cityPath: dir, cityName: "test", tomlPath: path, configRev: "current", cfg: &config.City{}}
	reply := cr.previewReload(&reloadRequest{dryRun: true}, dir, cr.cfg.Session.Provider)
	if reply.Outcome != reloadOutcomeFailed || reply.Error == "" || reply.Revision != "current" {
		t.Fatalf("invalid config preview=%+v", reply)
	}
}

func TestReloadPreviewPreservesRealFilesystem(t *testing.T) {
	for _, entry := range []string{"dry-run", "pre-apply"} {
		t.Run(entry, func(t *testing.T) {
			dir := t.TempDir()
			content := "[workspace]\nname = \"test\"\nprovider = \"claude\"\n" +
				"[session]\nprovider = \"tmux\"\n[beads]\nprovider = \"file\"\n" +
				"[providers.claude]\ncommand = \"echo\"\nprompt_mode = \"none\"\n"
			path := filepath.Join(dir, "city.toml")
			for name, data := range map[string]string{
				"city.toml":                  content,
				"agents/worker/agent.toml":   "work_dir = \"new-workdir\"\n",
				"agents/existing/agent.toml": "work_dir = \"existing-workdir\"\n",
				"pack.toml":                  "[pack]\nname = \"preview-test\"\nversion = \"0.1.0\"\nschema = 2\n",
				"skills/plan/SKILL.md":       "---\nname: plan\ndescription: test\n---\nbody\n",
				"existing-workdir/.gc/tmp/skill-catalog-existing.b64": "preserve this snapshot",
				".gc/settings.json": "{\"permissions\":{}}\n",
			} {
				file := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := tryReloadConfig(path, "test", dir)
			if err != nil {
				t.Fatal(err)
			}
			store := beads.NewMemStore()
			for _, name := range []string{"worker", "existing"} {
				if _, err := store.Create(beads.Bead{
					Type: sessionBeadType, Labels: []string{sessionBeadLabel},
					Metadata: map[string]string{"session_name": name, "template": name, "started_config_hash": "old"},
				}); err != nil {
					t.Fatal(err)
				}
			}
			cr := &CityRuntime{
				cityPath: dir, cityName: "test", tomlPath: path, cfg: result.Cfg,
				standaloneCityStore: store, stderr: io.Discard,
			}
			before := reloadPreviewTreeDigest(t, dir)
			if entry == "dry-run" {
				reply := cr.previewReload(&reloadRequest{dryRun: true}, dir, cr.cfg.Session.Provider)
				if reply.Outcome != reloadOutcomePreview || len(reply.Changes) != 2 {
					t.Fatalf("preview = %+v", reply)
				}
			} else if changes, err := cr.previewReloadChanges(result.Cfg, false, cr.cfg.Session.Provider, result.Revision); err != nil || len(changes) != 2 {
				t.Fatalf("pre-apply preview = %+v, %v", changes, err)
			}
			after := reloadPreviewTreeDigest(t, dir)
			t.Logf("filesystem digest before=%x after=%x", before, after)
			if before != after {
				t.Fatal("preview changed the real filesystem")
			}
		})
	}
}

func reloadPreviewTreeDigest(t *testing.T, root string) [sha256.Size]byte {
	t.Helper()
	hash := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(hash, "%s\x00%s\x00", rel, info.Mode()); err != nil {
			return err
		}
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			hash.Write(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return [sha256.Size]byte(hash.Sum(nil))
}

func TestReloadNoChangeIdentifiesLastWatcherApply(t *testing.T) {
	cr := &CityRuntime{configRev: "abcdef1234567890"}
	cr.lastConfigApply = reloadApplyInfo{Source: reloadSourceWatch, At: time.Date(2026, 11, 7, 1, 35, 0, 0, time.UTC)}
	got := cr.noChangeReloadMessage()
	for _, part := range []string{"No config changes detected.", "rev abcdef123456", "already applied by the watcher at 01:35:00Z"} {
		if !strings.Contains(got, part) {
			t.Errorf("message=%q missing %q", got, part)
		}
	}
}

func TestReloadPreviewNamesRuntimeReplacementSessions(t *testing.T) {
	sp := runtime.NewFake()
	for _, name := range []string{"session-b", "session-a"} {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	old := &config.City{
		Session:  config.SessionConfig{Provider: "pack-runtime"},
		Runtimes: map[string]config.DiscoveredRuntime{"pack-runtime": {Command: "/pack/old/runtime"}},
	}
	cr := &CityRuntime{cfg: old, sp: sp, standaloneCityStore: beads.NewMemStore()}
	for _, tc := range []struct {
		name     string
		provider string
		runtime  config.DiscoveredRuntime
		reason   string
	}{
		{"command", "pack-runtime", config.DiscoveredRuntime{Command: "/pack/new/runtime"}, "pack runtime command changed: pack-runtime"},
		{"protocol", "pack-runtime", config.DiscoveredRuntime{Command: "/pack/old/runtime", Protocol: 1}, "pack runtime protocol changed: pack-runtime"},
		{"provider", "other", config.DiscoveredRuntime{}, "session provider changed: pack-runtime → other"},
		{"attribution-only", "pack-runtime", config.DiscoveredRuntime{Command: "/pack/old/runtime", PackName: "renamed"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.City{
				Session:  config.SessionConfig{Provider: tc.provider},
				Runtimes: map[string]config.DiscoveredRuntime{"pack-runtime": tc.runtime},
			}
			for _, soft := range []bool{false, true} {
				changes, err := cr.previewReloadChanges(cfg, soft, cr.cfg.Session.Provider, "next")
				if err != nil {
					t.Fatal(err)
				}
				if tc.reason == "" {
					if len(changes) != 0 {
						t.Fatalf("attribution-only change replaces sessions: %+v", changes)
					}
					continue
				}
				want := []reloadChange{
					{Kind: "session", Name: "session-a", Action: "stop and restart", Reasons: []string{tc.reason}},
					{Kind: "session", Name: "session-b", Action: "stop and restart", Reasons: []string{tc.reason}},
				}
				if !reflect.DeepEqual(changes, want) {
					t.Fatalf("soft=%v plan=%+v, want %+v", soft, changes, want)
				}
				validateReloadChangesSchemas(t, changes)
				var output bytes.Buffer
				writeReloadChanges(&output, changes)
				if !strings.Contains(output.String(), "0 service(s), 2 session(s)") {
					t.Fatalf("missing counts: %s", output.String())
				}
			}
		})
	}
	for _, call := range sp.Calls {
		if call.Method != "Start" && call.Method != "ListRunning" {
			t.Fatalf("preview changed runtime: %+v", call)
		}
	}
}

func TestReloadPreviewUnchangedManualSession(t *testing.T) {
	dir := t.TempDir()
	writeTemplateResolveCityConfig(t, dir, "file")
	cfg := &config.City{
		Workspace: config.Workspace{Provider: "test"},
		Providers: map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		Agents:    []config.Agent{{Name: "worker"}},
	}
	bp := newAgentBuildParams("test", dir, cfg, nil, time.Time{}, nil, io.Discard)
	bp.readOnly = true
	info := session.Info{ID: "manual-one", Template: "worker", SessionNameMetadata: "manual-worker", SessionOrigin: "manual"}
	agent := &cfg.Agents[0]
	qualifiedName := sessionBeadQualifiedNameInfo(dir, agent, cfg.Rigs, info)
	resolveAgent := sessionBeadConfigAgent(agent, qualifiedName)
	started, err := resolveTemplateForSessionBeadInfo(bp, resolveAgent, qualifiedName, buildFingerprintExtra(resolveAgent), info)
	if err != nil {
		t.Fatal(err)
	}
	// Manual starts enable mouse mode by default, as normal reconciliation does.
	started.ManualSession = true
	core := sessionCoreConfigForHashInfo(started, info)
	if !core.MouseOn {
		t.Fatal("manual fixture did not enable mouse mode")
	}
	info.StartedConfigHash = runtime.CoreFingerprint(core)
	infos := []session.Info{info}
	desired, _, err := resolveReloadSessionTemplates(bp, cfg, infos)
	if err != nil {
		t.Fatal(err)
	}
	for _, soft := range []bool{false, true} {
		if changes := reloadSessionChanges(infos, desired, cfg, soft); len(changes) != 0 {
			t.Fatalf("soft=%v unchanged manual session reports drift: %+v", soft, changes)
		}
	}
}

func TestReloadPreviewUnchangedRelativeRigPath(t *testing.T) {
	dir := t.TempDir()
	writeTemplateResolveCityConfig(t, dir, "file")
	cfg := &config.City{
		Workspace: config.Workspace{Provider: "test"},
		Providers: map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		Agents:    []config.Agent{{Name: "worker", Dir: "demo"}},
		Rigs:      []config.Rig{{Name: "demo", Path: filepath.Join(dir, "demo")}},
	}
	info := session.Info{ID: "one", Template: cfg.Agents[0].QualifiedName(), SessionNameMetadata: "rig-session"}
	bp := newAgentBuildParams("test", dir, cfg, nil, time.Time{}, nil, io.Discard)
	desired, _, err := resolveReloadSessionTemplates(bp, cfg, []session.Info{info})
	if err != nil {
		t.Fatal(err)
	}
	started := desired[info.SessionNameMetadata]
	if started.Env["GC_RIG_ROOT"] != cfg.Rigs[0].Path {
		t.Fatalf("fixture rig root = %q", started.Env["GC_RIG_ROOT"])
	}
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Type: sessionBeadType, Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": info.SessionNameMetadata, "template": info.Template,
			"started_config_hash": runtime.CoreFingerprint(sessionCoreConfigForHashInfo(started, info)),
		},
	}); err != nil {
		t.Fatal(err)
	}
	candidate := *cfg
	candidate.Rigs = []config.Rig{{Name: "demo", Path: "demo"}}
	cr := &CityRuntime{cityName: "test", cityPath: dir, cfg: cfg, standaloneCityStore: store, stderr: io.Discard}
	for _, soft := range []bool{false, true} {
		changes, err := cr.previewReloadChanges(&candidate, soft, cr.cfg.Session.Provider, "next")
		if err != nil || len(changes) != 0 {
			t.Fatalf("soft=%v unchanged relative rig preview=%+v, %v", soft, changes, err)
		}
	}
	if candidate.Rigs[0].Path != "demo" {
		t.Fatal("preview mutated the candidate rig paths")
	}
}

func TestReloadDryRunDoesNotRunReconciliationOrConsumeWatcherChange(t *testing.T) {
	dir := t.TempDir()
	path := writeCityTOML(t, dir, "test", "worker")
	var stdout, stderr bytes.Buffer
	dirty := &atomic.Bool{}
	dirty.Store(true)
	req := &reloadRequest{dryRun: true, soft: true, doneCh: make(chan reloadControlReply, 1)}
	cr := &CityRuntime{
		cityPath: dir, cityName: "test", tomlPath: path, cfg: &config.City{},
		configRev: "current", activeReload: req, stdout: &stdout, stderr: &stderr,
	}
	// Nil reconciliation dependencies deliberately make entering a real tick
	// invalid. The preview must return before any runtime work begins.
	provider := "fake"
	var previous map[string]bool
	cr.tick(context.Background(), dirty, &provider, dir, &previous, "reload")
	select {
	case reply := <-req.doneCh:
		if reply.Outcome != reloadOutcomePreview || reply.Revision != "current" || reply.TargetRevision == "" {
			t.Fatalf("preview=%+v stderr=%s", reply, stderr.String())
		}
	default:
		t.Fatal("dry run did not reply")
	}
	if !dirty.Load() || cr.configRev != "current" || !reflect.DeepEqual(cr.cfg, &config.City{}) || cr.activeReload != nil {
		t.Fatal("dry run changed runtime configuration or consumed a watcher notification")
	}
}

func TestReloadPreviewRetainsValidSessionsWhenOneIsUnresolved(t *testing.T) {
	dir := t.TempDir()
	writeCatalogFile(t, dir, "city.toml", "[workspace]\nname = \"test\"\nprovider = \"test\"\n"+
		"[beads]\nprovider = \"file\"\n[providers.test]\ncommand = \"echo\"\nprompt_mode = \"none\"\n")
	writeCatalogFile(t, dir, "pack.toml", "[pack]\nname = \"preview-test\"\nversion = \"0.1.0\"\nschema = 2\n")
	writeCatalogFile(t, dir, "agents/bad/agent.toml", "work_dir = \"{{.Missing}}\"\n")
	writeCatalogFile(t, dir, "agents/good/agent.toml", "start_command = \"echo changed\"\n")
	path := filepath.Join(dir, "city.toml")
	result, err := tryReloadConfig(path, "test", dir)
	if err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStore()
	for _, name := range []string{"bad", "good"} {
		if _, err := store.Create(beads.Bead{
			Type: sessionBeadType, Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"session_name": name, "template": name,
				"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "old"}),
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	cr := &CityRuntime{
		cityName: "test", cityPath: dir, tomlPath: path, cfg: result.Cfg,
		standaloneCityStore: store, stderr: io.Discard,
	}
	for _, entry := range []string{"dry-run", "pre-apply"} {
		t.Run(entry, func(t *testing.T) {
			var changes []reloadChange
			if entry == "dry-run" {
				reply := cr.previewReload(&reloadRequest{dryRun: true}, dir, cr.cfg.Session.Provider)
				if reply.Outcome != reloadOutcomePreview || len(reply.Warnings) == 0 ||
					!strings.Contains(strings.Join(reply.Warnings, " "), "reload preview incomplete") {
					t.Fatalf("partial preview must be visible and flagged incomplete: %+v", reply)
				}
				changes = reply.Changes
			} else {
				var err error
				changes, err = cr.previewReloadChanges(result.Cfg, false, cr.cfg.Session.Provider, result.Revision)
				if err == nil || !strings.Contains(err.Error(), "bad") {
					t.Fatalf("missing unresolved-session diagnostic: %v", err)
				}
			}
			actions := make(map[string]string)
			for _, change := range changes {
				actions[change.Name] = change.Action
			}
			if len(changes) != 2 || actions["bad"] != "unresolved" || actions["good"] != "drain" {
				t.Fatalf("partial plan lost a session: %+v", changes)
			}
			validateReloadChangesSchemas(t, changes)
		})
	}
	t.Run("runtime replacement keeps one row per session", func(t *testing.T) {
		sp := runtime.NewFake()
		if err := sp.Start(context.Background(), "bad", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		current := *result.Cfg
		current.Session.Provider = "pack-runtime"
		current.Runtimes = map[string]config.DiscoveredRuntime{"pack-runtime": {Command: "old"}}
		next := current
		next.Runtimes = map[string]config.DiscoveredRuntime{"pack-runtime": {Command: "new"}}
		cr.cfg, cr.sp = &current, sp
		changes, err := cr.previewReloadChanges(&next, false, cr.cfg.Session.Provider, "next")
		if err == nil || len(changes) != 2 {
			t.Fatalf("runtime replacement partial plan = %+v, %v", changes, err)
		}
		for _, change := range changes {
			if change.Name == "bad" && (change.Action != "stop and restart" ||
				!strings.Contains(strings.Join(change.Reasons, " "), "template unresolved")) {
				t.Fatalf("replacement lost unresolved reason: %+v", change)
			}
		}
	})
}

func validateReloadChangesSchemas(t *testing.T, changes []reloadChange) {
	t.Helper()
	for _, role := range []string{jsonSchemaResultRole, jsonSchemaFailureRole} {
		t.Run(role+" schema", func(t *testing.T) {
			var output bytes.Buffer
			if role == jsonSchemaResultRole {
				if err := writeLifecycleActionJSON(&output, lifecycleActionJSON{
					Command: "reload", Action: "reload", CityPath: t.TempDir(),
					Async: lifecycleBoolPtr(false), Soft: lifecycleBoolPtr(false),
					Outcome: string(reloadOutcomePreview), Changes: changes,
				}); err != nil {
					t.Fatal(err)
				}
			} else if code := writeReloadFailureJSON(&output, io.Discard, reloadControlReply{
				Outcome: reloadOutcomeFailed, Error: "apply failed", Changes: changes,
			}); code != 1 {
				t.Fatalf("failure exit code = %d", code)
			}
			raw, err := readBuiltinSchema([]string{"reload"}, role)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			compiler := jsonschema.NewCompiler()
			url := "reload/" + role + ".schema.json"
			if err := compiler.AddResource(url, doc); err != nil {
				t.Fatal(err)
			}
			schema, err := compiler.Compile(url)
			if err != nil {
				t.Fatal(err)
			}
			instance, err := jsonschema.UnmarshalJSON(&output)
			if err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(instance); err != nil {
				t.Fatalf("preview actions violate %s schema: %v", role, err)
			}
		})
	}
}

func TestReloadAcceptedRevisionIsAtRequest(t *testing.T) {
	reply := reloadControlReply{Outcome: reloadOutcomeAccepted, Message: "Reload requested.", Revision: "abcdef1234567890"}
	if got := reloadReplyMessage(reply); got != "Reload requested. (rev abcdef123456 at request)" {
		t.Fatalf("acceptance revision is ambiguous: %q", got)
	}
}

func TestReloadPreviewUsesActiveProviderAfterFailedReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "city.toml")
	writeCityRuntimeConfig(t, path, "fake")
	cfg, err := config.Load(osFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	oldRegistry := runtimeRegistry
	runtimeRegistry = runtimeRegistry.Clone()
	t.Cleanup(func() { runtimeRegistry = oldRegistry })
	available := false
	constructorCalls := 0
	nextProvider := runtime.NewFake()
	if err := runtimeRegistry.Register("recovering", func(string, config.SessionConfig, string, string) (runtime.Provider, error) {
		constructorCalls++
		if !available {
			return nil, fmt.Errorf("runtime construction unavailable")
		}
		return nextProvider, nil
	}); err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	for _, name := range []string{"session-a", "session-b"} {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: dir, CityName: "test-city", TomlPath: path,
		Cfg: cfg, SP: sp, Dops: newDrainOps(sp), Rec: events.Discard, Stdout: io.Discard, Stderr: io.Discard,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
	})
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", dir)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)
	cr.sessionDrains = newDrainTracker()
	writeCityRuntimeConfig(t, path, "recovering")
	activeProvider := "fake"
	first := cr.reloadConfigTraced(context.Background(), &activeProvider, dir, nil, reloadSourceWatch)
	if first.Outcome != reloadOutcomeApplied || cr.cfg.Session.Provider != "recovering" || cr.sp != sp ||
		activeProvider != "fake" || constructorCalls != 1 {
		t.Fatalf("failed construction did not retain active provider: reply=%+v selected=%s active=%s calls=%d",
			first, cr.cfg.Session.Provider, activeProvider, constructorCalls)
	}
	available = true
	unchanged := cr.previewReload(&reloadRequest{dryRun: true}, dir, activeProvider)
	if unchanged.Outcome != reloadOutcomePreview || len(unchanged.Changes) != 0 {
		t.Fatalf("same-revision reload does not retry provider replacement: %+v", unchanged)
	}
	writeCityRuntimeConfigWithShutdownTimeout(t, path, "recovering", "1ms")
	checkPlan := func(changes []reloadChange) {
		t.Helper()
		if len(changes) != 2 {
			t.Fatalf("recovery preview omitted runtime-wide stop: %+v", changes)
		}
		for i, name := range []string{"session-a", "session-b"} {
			if changes[i].Name != name || changes[i].Action != "stop and restart" ||
				!strings.Contains(strings.Join(changes[i].Reasons, " "), "fake → recovering") {
				t.Fatalf("recovery preview = %+v", changes)
			}
		}
	}
	for _, soft := range []bool{false, true} {
		req := &reloadRequest{dryRun: true, soft: soft, doneCh: make(chan reloadControlReply, 1)}
		cr.activeReload = req
		var previous map[string]bool
		cr.tick(context.Background(), &atomic.Bool{}, &activeProvider, dir, &previous, "reload")
		var reply reloadControlReply
		select {
		case reply = <-req.doneCh:
		default:
			t.Fatal("dry-run did not reply")
		}
		if reply.Outcome != reloadOutcomePreview {
			t.Fatalf("dry-run recovery preview = %+v", reply)
		}
		checkPlan(reply.Changes)
	}
	if constructorCalls != 1 {
		t.Fatal("preview constructed a runtime")
	}
	progress := make(chan reloadControlReply, 1)
	cr.activeReload = &reloadRequest{soft: true, progressCh: progress}
	reply := cr.reloadConfigTraced(context.Background(), &activeProvider, dir, nil, reloadSourceManual)
	select {
	case preview := <-progress:
		checkPlan(preview.Changes)
	default:
		t.Fatal("pre-apply preview was not sent")
	}
	if reply.Outcome != reloadOutcomeApplied || activeProvider != "recovering" || cr.sp != nextProvider || constructorCalls != 2 {
		t.Fatalf("recovery apply = %+v active=%s calls=%d", reply, activeProvider, constructorCalls)
	}
	if sp.IsRunning("session-a") || sp.IsRunning("session-b") {
		t.Fatal("apply did not perform the previewed stop")
	}
}
