package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/materialize"
)

func TestResolveTemplateLanePaths(t *testing.T) {
	city := t.TempDir()
	rig := t.TempDir()
	lane := t.TempDir()
	writeTemplateResolveCityConfig(t, city, "file")
	params := &agentBuildParams{
		cityName: "city", cityPath: city,
		workspace: &config.Workspace{Provider: "test"},
		providers: map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:  func(string) (string, error) { return "/bin/echo", nil },
		fs:        fsys.OSFS{}, rigs: []config.Rig{{Name: "repo", Path: rig}},
		beaconTime: time.Unix(0, 0), beadNames: make(map[string]string), stderr: io.Discard,
		sessionProvider: "tmux",
	}
	a := &config.Agent{Name: "worker", Dir: "repo", WorkDir: lane, PreStart: []string{"true"}}
	tp, err := resolveTemplate(params, a, a.QualifiedName(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"GC_CITY_PATH": city, "GC_RIG_ROOT": rig, "GC_RIG": "repo",
		"BEADS_DIR":       filepath.Join(rig, ".beads"),
		"GC_PRESTART_LOG": filepath.Join(lane, ".gc", "pre-start.log"),
	} {
		t.Run(key, func(t *testing.T) {
			if got := tp.Env[key]; got != want {
				t.Errorf("launch %s = %q, want %q", key, got, want)
			}
			if !strings.Contains(tp.Prompt, key+"=") || !strings.Contains(tp.Prompt, want) {
				t.Errorf("prompt must expose %s=%s even when tool shells filter env: %s", key, want, tp.Prompt)
			}
		})
	}
	if !strings.Contains(tp.Prompt, filepath.Join(lane, ".gc", "worker-env.sh")) {
		t.Errorf("prompt does not locate the sourceable lane environment: %s", tp.Prompt)
	}
}

func TestResolveTemplateSkillHintUsesMaterializedPath(t *testing.T) {
	city := t.TempDir()
	lane := t.TempDir()
	writeTemplateResolveCityConfig(t, city, "file")
	sink := filepath.Join(lane, ".agents", "skills", "planner")
	if err := os.MkdirAll(sink, 0o755); err != nil {
		t.Fatal(err)
	}
	materialized := filepath.Join(sink, "SKILL.md")
	if err := os.WriteFile(materialized, []byte("plan"), 0o600); err != nil {
		t.Fatal(err)
	}
	params := &agentBuildParams{
		cityName: "city", cityPath: city,
		workspace: &config.Workspace{Provider: "codex"},
		providers: map[string]config.ProviderSpec{"codex": {Command: "echo", PromptMode: "none"}},
		lookPath:  func(string) (string, error) { return "/bin/echo", nil },
		fs:        fsys.OSFS{}, beaconTime: time.Unix(0, 0), beadNames: make(map[string]string), stderr: io.Discard,
		sessionProvider: "tmux",
		skillCatalog:    &materialize.CityCatalog{Entries: []materialize.SkillEntry{{Name: "planner", Source: "/absent/cache/planner"}}},
	}
	a := &config.Agent{Name: "worker", WorkDir: lane}
	for _, exists := range []bool{true, false} {
		if !exists {
			if err := os.Remove(materialized); err != nil {
				t.Fatal(err)
			}
		}
		tp, err := resolveTemplate(params, a, a.QualifiedName(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(tp.Prompt, materialized); got != exists {
			t.Errorf("materialized path hint present = %v, file exists = %v: %s", got, exists, tp.Prompt)
		}
		if strings.Contains(tp.Prompt, "/absent/cache") {
			t.Errorf("prompt contains a catalog cache path: %s", tp.Prompt)
		}
	}
}

func TestPrimeLaneContextUsesResolvedPathsWithoutLaunchEnvironment(t *testing.T) {
	a := &config.Agent{Name: "worker", WorkDir: "/lane", PreStart: []string{"true"}}
	ctx := PromptContext{CityRoot: "/city", RigName: "repo", RigRoot: "/repo", WorkDir: "/lane"}
	got := primeLaneContextSuffix(a, &config.City{}, ctx)
	for _, want := range []string{"GC_CITY_PATH=", "/city", "GC_RIG_ROOT=", "/repo", "BEADS_DIR=", "/repo/.beads"} {
		if !strings.Contains(got, want) {
			t.Errorf("prime context lacks %q: %s", want, got)
		}
	}
	if strings.Contains(got, "/lane/.gc/") {
		t.Fatalf("prime must not advertise missing artifacts in pre-upgrade sessions: %s", got)
	}
	ctx.WorkDir = t.TempDir()
	if err := os.Mkdir(filepath.Join(ctx.WorkDir, ".gc"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"worker-env.sh", "pre-start.log"} {
		if err := os.WriteFile(filepath.Join(ctx.WorkDir, ".gc", name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got = primeLaneContextSuffix(a, &config.City{}, ctx)
	for _, name := range []string{"worker-env.sh", "pre-start.log"} {
		if !strings.Contains(got, filepath.Join(ctx.WorkDir, ".gc", name)) {
			t.Errorf("prime omitted materialized artifact %s: %s", name, got)
		}
	}
	for _, scope := range []string{"/city", "/repo"} {
		ctx.WorkDir = scope
		if got := primeLaneContextSuffix(a, &config.City{}, ctx); got != "" {
			t.Errorf("scope root is not a lane: %s", got)
		}
	}
}

func TestLaneContextHonorsRuntimeOverride(t *testing.T) {
	for _, provider := range []string{"tmux", "herdr", "ssh", "k8s", "subprocess", "acp"} {
		a := &config.Agent{Name: "worker", WorkDir: "/lane", PreStart: []string{"true"}, Session: provider}
		want := provider == "tmux" || provider == "herdr"
		if got := hasLaneContext(a, "tmux", "/city", "/repo", "/lane"); got != want {
			t.Errorf("runtime override %s: lane artifacts = %v, want %v", provider, got, want)
		}
	}
}

func TestLaneContextRejectsSymlinkedScopeRoots(t *testing.T) {
	for _, scope := range []string{"city", "rig"} {
		t.Run(scope, func(t *testing.T) {
			root := t.TempDir()
			alias := filepath.Join(t.TempDir(), "scope-link")
			if err := os.Symlink(root, alias); err != nil {
				t.Fatal(err)
			}
			for _, paths := range [][2]string{{root, alias}, {alias, root}} {
				city, rig := t.TempDir(), t.TempDir()
				if scope == "city" {
					city = paths[0]
				} else {
					rig = paths[0]
				}
				a := &config.Agent{WorkDir: paths[1], PreStart: []string{"true"}}
				if hasLaneContext(a, "tmux", city, rig, paths[1]) {
					t.Errorf("physical %s root advertised as a lane: scope=%s workDir=%s", scope, paths[0], paths[1])
				}
				ctx := PromptContext{CityRoot: city, RigRoot: rig, WorkDir: paths[1]}
				if got := primeLaneContextSuffix(a, &config.City{}, ctx); got != "" {
					t.Errorf("prime advertises artifacts in a physical scope root: %s", got)
				}
			}
			lane := filepath.Join(alias, "new-lane")
			a := &config.Agent{WorkDir: lane, PreStart: []string{"true"}}
			if !hasLaneContext(a, "tmux", t.TempDir(), root, lane) {
				t.Fatal("a new directory below a symlinked parent should remain eligible")
			}
		})
	}
}
