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
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
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
	desired, err := resolveReloadSessionTemplates(bp, cfg, infos)
	if err != nil {
		t.Fatal(err)
	}
	infos[0].StartedConfigHash = runtime.CoreFingerprint(sessionCoreConfigForHashInfo(desired["worker-1"], infos[0]))
	if changes := reloadSessionChanges(infos, desired, cfg, false); len(changes) != 0 {
		t.Fatalf("unchanged template reported drift: %+v", changes)
	}
	cfg.Agents[0].StartCommand = "echo new"
	desired, err = resolveReloadSessionTemplates(bp, cfg, infos)
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
	reply := cr.previewReload(&reloadRequest{dryRun: true}, dir)
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
				reply := cr.previewReload(&reloadRequest{dryRun: true}, dir)
				if reply.Outcome != reloadOutcomePreview || len(reply.Changes) != 2 {
					t.Fatalf("preview = %+v", reply)
				}
			} else if changes, err := cr.previewReloadChanges(result.Cfg, false); err != nil || len(changes) != 2 {
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
				changes, err := cr.previewReloadChanges(cfg, soft)
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
	desired, err := resolveReloadSessionTemplates(bp, cfg, infos)
	if err != nil {
		t.Fatal(err)
	}
	for _, soft := range []bool{false, true} {
		if changes := reloadSessionChanges(infos, desired, cfg, soft); len(changes) != 0 {
			t.Fatalf("soft=%v unchanged manual session reports drift: %+v", soft, changes)
		}
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
