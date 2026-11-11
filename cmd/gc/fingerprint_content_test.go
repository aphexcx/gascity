package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestFingerprintContentLockedSourcesAndRelock(t *testing.T) {
	root := t.TempDir()
	nextRoot := t.TempDir()
	c := fingerprintContentForConfig(t.TempDir(), &config.City{}, io.Discard)
	c.rememberRoot(root)
	c.rememberRoot(nextRoot)
	for _, name := range []string{"skills", "prompts", "overlays", "copied-files"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(root, name)
			write := func(path, value string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write(path, "accepted")
			before := c.hashPath(path)
			write(path, "stray edit")
			if got := c.hashPath(path); got != before {
				t.Fatalf("locked source drifted: %s -> %s", before, got)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if got := c.hashPath(path); got != before {
				t.Fatalf("unavailable source replaced known hash: %s", got)
			}
			newPath := filepath.Join(nextRoot, name)
			write(newPath, "relocked change")
			if got := c.hashPath(newPath); got == "" || got == before {
				t.Fatalf("relock did not change source hash: %s", got)
			}
		})
	}
}

func TestFingerprintContentLocalChangeAndUnavailableHash(t *testing.T) {
	c := fingerprintContentForConfig(t.TempDir(), &config.City{}, io.Discard)
	path := filepath.Join(t.TempDir(), "local")
	if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := c.hashPath(path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := c.hashPath(path); got != first {
		t.Fatalf("unknown hash replaced known hash: %s", got)
	}
	if err := os.WriteFile(path, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := c.hashPath(path); got == first || got != runtime.HashPathContent(path) {
		t.Fatalf("local change was hidden: %s", got)
	}
}

func TestFingerprintAgentCatalogRelockAndLocalMembership(t *testing.T) {
	c := fingerprintContentForConfig(t.TempDir(), &config.City{}, io.Discard)
	locked, next, local := t.TempDir(), t.TempDir(), t.TempDir()
	c.rememberRoot(locked)
	c.rememberRoot(next)
	writeSkill := func(root, name string) {
		t.Helper()
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("A skill"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	count := func(root string) int {
		t.Helper()
		cat, err := c.agentCatalog(root)
		if err != nil {
			t.Fatal(err)
		}
		return len(cat.Entries)
	}
	for _, root := range []string{locked, local} {
		writeSkill(root, "first")
		if count(root) != 1 {
			t.Fatal("missing accepted skill")
		}
		writeSkill(root, "second")
	}
	if count(locked) != 1 || count(local) != 2 {
		t.Fatal("locked and local catalog views did not retain their distinct membership contracts")
	}
	writeSkill(next, "first")
	writeSkill(next, "second")
	if count(next) != 2 {
		t.Fatal("relocked agent catalog did not expose new membership")
	}
}

func TestFingerprintPromptSourceSnapshot(t *testing.T) {
	root := t.TempDir()
	c := fingerprintContentForConfig(t.TempDir(), &config.City{}, io.Discard)
	c.rememberRoot(root)
	fs := fingerprintFS{FS: fsys.OSFS{}, content: c}
	path := filepath.Join(root, "prompt.template.md")
	if err := os.WriteFile(path, []byte("accepted {{.AgentName}}"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("stray edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "extra.template.md"), []byte("extra"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := fs.ReadFile(path); err != nil || string(got) != string(before) {
		t.Fatalf("prompt changed: %q, %v", got, err)
	}
	if got, err := fs.ReadDir(root); err != nil || len(got) != len(entries) {
		t.Fatalf("fragment membership changed: %v, %v", got, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got, err := fs.ReadFile(path); err != nil || string(got) != string(before) {
		t.Fatalf("unavailable prompt changed: %q, %v", got, err)
	}
}

func TestTemplateLockedImportContentSnapshots(t *testing.T) {
	f := newCachedImportFixture(t)
	root := f.cacheDirs[1]
	f.cfg.Agents[0].PromptTemplate = filepath.Join(root, "prompt.template.md")
	f.cfg.PackOverlayDirs = []string{filepath.Join(root, "overlay")}
	if err := os.Remove(filepath.Join(f.cityPath, ".gc", "settings.json")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	settingsPath := citylayout.ClaudeHookFilePath(f.cityPath)
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "settings.json"), settingsPath); err != nil {
		t.Fatal(err)
	}
	resolve := func() TemplateParams {
		t.Helper()
		bp := newAgentBuildParams("test-city", f.cityPath, f.cfg, f.sp, time.Time{}, nil, f.stderr)
		bp.readOnly = true
		tp, err := resolveTemplate(bp, &f.cfg.Agents[0], "worker", nil)
		if err != nil {
			t.Fatal(err)
		}
		return tp
	}
	before := resolve()
	if before.Prompt == "" || len(before.OverlaySourceHashes) == 0 {
		t.Fatal("fixture did not resolve prompt and overlay sources")
	}
	found := false
	for _, entry := range before.Hints.CopyFiles {
		if entry.Src == settingsPath && entry.Probed && entry.ContentHash != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("fixture did not fingerprint the copied settings source")
	}
	for _, source := range []string{"prompt.template.md", "settings.json", "overlay/per-provider/claude/instructions.md"} {
		if err := os.WriteFile(filepath.Join(root, source), []byte("stray edit"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	after := resolve()
	if canonicalConfigHash(before, nil) != canonicalConfigHash(after, nil) {
		t.Fatal("locked prompt changed the session fingerprint")
	}
	beforeCfg, afterCfg := templateParamsToConfig(before), templateParamsToConfig(after)
	if runtime.CoreFingerprint(beforeCfg) != runtime.CoreFingerprint(afterCfg) {
		t.Fatal("locked copied source changed the runtime fingerprint")
	}
	for source, hash := range runtime.OverlaySourceHashes(beforeCfg) {
		if hash == "" || runtime.OverlaySourceHashes(afterCfg)[source] != hash {
			t.Fatalf("locked overlay source hash changed: %s", source)
		}
	}
}
