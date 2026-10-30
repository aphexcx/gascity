package main

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestCodexOverlayHookExistsBeforeFingerprint(t *testing.T) {
	cityDir := t.TempDir()
	workDir := t.TempDir()
	overlayDir := seedCodexOverlay(t)
	base := "builtin:codex"
	cfg := &config.City{
		Workspace:       config.Workspace{Name: "test-city"},
		Agents:          []config.Agent{{Name: "worker", Provider: "codex-local", WorkDir: workDir}},
		Providers:       map[string]config.ProviderSpec{"codex-local": {Base: &base, Command: "/bin/echo --config-dir={{.ConfigDir}} --agent={{.Agent}}", ResumeCommand: "/bin/echo resume {{.SessionKey}}"}},
		PackOverlayDirs: []string{overlayDir},
	}
	bp := newAgentBuildParams("test-city", cityDir, cfg, runtime.NewFake(), time.Now().UTC(), nil, io.Discard)
	if _, err := config.ResolveProvider(&cfg.Agents[0], bp.workspace, bp.providers, bp.lookPath); err != nil {
		t.Fatal(err)
	}
	prepareTemplateResolution(bp, &cfg.Agents[0], "worker", io.Discard)
	tp, err := resolveTemplate(bp, &cfg.Agents[0], "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	foundTrust := false
	for _, command := range tp.Hints.PreStart {
		if strings.Contains(command, "internal trust-codex-hooks ") {
			foundTrust = strings.Contains(command, "--config-dir="+cityDir) && strings.Contains(command, "--agent=worker") && strings.Contains(command, workDir) && !strings.Contains(command, "{{")
		}
	}
	if !foundTrust {
		t.Fatalf("missing trust pre_start for resolved Codex family: %v", tp.Hints.PreStart)
	}
	before := stageHookFiles(nil, cityDir, workDir, []string{"codex"})
	if len(before) != 1 || before[0].Src != filepath.Join(workDir, ".codex", "hooks.json") {
		t.Fatalf("initial fingerprint omits the generated Codex hook: %+v", before)
	}
	startCfg := runtime.Config{WorkDir: workDir, ProviderName: "codex", PackOverlayDirs: []string{overlayDir}, CopyFiles: before}
	stored := runtime.CoreFingerprint(startCfg)
	if err := runtime.StageSessionWorkDirWithWarnings(startCfg, io.Discard); err != nil {
		t.Fatal(err)
	}
	startCfg.CopyFiles = stageHookFiles(nil, cityDir, workDir, []string{"codex"})
	if got := runtime.CoreFingerprint(startCfg); got != stored {
		t.Fatalf("gc's own staging changed the fingerprint: stored=%s current=%s", stored, got)
	}
}
