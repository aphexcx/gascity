package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

const packCacheTestCommit = "1234567890abcdef1234567890abcdef12345678"

type packCacheFixture struct {
	city   string
	caches []string
	gitLog string
	marker string
}

func newPackCacheFixture(t *testing.T, count int) packCacheFixture {
	t.Helper()
	base := t.TempDir()
	f := packCacheFixture{city: filepath.Join(base, "city"), gitLog: filepath.Join(base, "git-calls"), marker: filepath.Join(base, "executed")}
	gcHome := filepath.Join(base, "gchome")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("GC_CITY", f.city)
	t.Setenv("GC_DIR", "")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_NO_API", "1")
	restoreFlag(t, &cityFlag)
	restoreFlag(t, &rigFlag)
	config.ResetRemoteCacheValidationCache()
	t.Cleanup(config.ResetRemoteCacheValidationCache)
	write := func(path, text string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), mode); err != nil {
			t.Fatal(err)
		}
	}
	var city, lock strings.Builder
	city.WriteString("[workspace]\nname = \"cache-city\"\n")
	lock.WriteString("schema = 1\n")
	for i := range count {
		source := fmt.Sprintf("https://example.com/cache%d.git", i)
		cache := filepath.Join(gcHome, "cache", "repos", config.RepoCacheKey(source, packCacheTestCommit))
		f.caches = append(f.caches, cache)
		fmt.Fprintf(&city, "[imports.sample%d]\nsource = %q\n", i, source)
		fmt.Fprintf(&lock, "[packs.%q]\ncommit = %q\n", source, packCacheTestCommit)
		write(filepath.Join(cache, ".git", "HEAD"), packCacheTestCommit+"\n", 0o644)
		write(filepath.Join(cache, ".git", "index"), "fixture", 0o644)
		write(filepath.Join(cache, "pack.toml"), fmt.Sprintf("[pack]\nname = \"sample%d\"\nschema = 1\n", i), 0o644)
		write(filepath.Join(cache, "commands", "report", "run.sh"), fmt.Sprintf("#!/bin/sh\nprintf executed > %q\n", f.marker), 0o755)
	}
	write(filepath.Join(f.city, "city.toml"), city.String(), 0o644)
	write(filepath.Join(f.city, "packs.lock"), lock.String(), 0o644)
	gitPath := filepath.Join(base, "bin", "git")
	write(gitPath, fmt.Sprintf(`#!/bin/sh
printf '%%s %%s\n' "$PWD" "$*" >> %q
case "$*" in
  --version) printf 'git version 2.50.0\n' ;;
  *'rev-parse HEAD') cat .git/HEAD ;;
  *'status --porcelain') if test -f .dirty; then printf ' M commands/report/run.sh\n'; fi ;;
  *) printf 'unexpected git invocation\n' >&2; exit 1 ;;
esac
`, f.gitLog), 0o755)
	t.Setenv("PATH", filepath.Dir(gitPath)+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

func (f packCacheFixture) gitCalls(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.gitLog)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestPackCacheDiscoveryAndOrdinaryLoadDoNotSpawnGit(t *testing.T) {
	f := newPackCacheFixture(t, 1)
	root := &cobra.Command{Use: "gc"}
	registerPackCommands(root, []string{"version"}, io.Discard, io.Discard)
	if findSubcommand(root, "sample0") == nil {
		t.Fatal("healthy cached command was not discovered")
	}
	if _, err := loadCityConfigWithoutBuiltinPackRefresh(f.city, io.Discard); err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	if _, err := loadCityConfig(f.city, &warnings); err != nil {
		t.Fatal(err)
	}
	if calls := f.gitCalls(t); calls != "" {
		t.Fatalf("discovery/ordinary read spawned Git:\n%s", calls)
	}
}

func TestPackCacheVersionAndHelpDoNotSpawnGit(t *testing.T) {
	for _, command := range []string{"version", "help", "import"} {
		t.Run(command, func(t *testing.T) {
			f := newPackCacheFixture(t, 1)
			var stdout, stderr bytes.Buffer
			if code := run([]string{command}, &stdout, &stderr); code != 0 {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			if command == "help" && !strings.Contains(stdout.String(), "sample0") {
				t.Fatal("help omitted discovered pack")
			}
			if calls := f.gitCalls(t); calls != "" {
				t.Fatalf("%s spawned Git:\n%s", command, calls)
			}
		})
	}
}

func TestPackCacheWrongCommitRefusesDiscoveryAndOrdinaryLoad(t *testing.T) {
	f := newPackCacheFixture(t, 1)
	wrong := strings.Repeat("f", 40)
	if err := os.WriteFile(filepath.Join(f.caches[0], ".git", "HEAD"), []byte(wrong+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := &cobra.Command{Use: "gc"}
	registerPackCommands(root, nil, io.Discard, io.Discard)
	if findSubcommand(root, "sample0") != nil {
		t.Fatal("discovery accepted the wrong commit")
	}
	_, err := loadCityConfigWithoutBuiltinPackRefresh(f.city, io.Discard)
	want := fmt.Sprintf("cached import https://example.com/cache0.git is checked out at %s, expected %s; run \"gc import install\"", wrong, packCacheTestCommit)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want %s", err, want)
	}
	if calls := f.gitCalls(t); calls != "" {
		t.Fatalf("commit mismatch spawned Git:\n%s", calls)
	}
}

func TestPackCacheExecutionScansOnlyOwningImport(t *testing.T) {
	f := newPackCacheFixture(t, 3)
	root := &cobra.Command{Use: "gc"}
	var stderr bytes.Buffer
	registerPackCommands(root, nil, io.Discard, &stderr)
	root.SetArgs([]string{"sample1", "report"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v; %s", err, stderr.String())
	}
	if _, err := os.Stat(f.marker); err != nil {
		t.Fatalf("script did not execute: %v", err)
	}
	calls := f.gitCalls(t)
	if strings.Count(calls, "status --porcelain") != 1 || !strings.Contains(calls, f.caches[1]+" -c core.fsmonitor=false") {
		t.Fatalf("expected one scan of owning cache, got:\n%s", calls)
	}
	for _, other := range []string{f.caches[0], f.caches[2]} {
		if strings.Contains(calls, other+" ") {
			t.Fatalf("execution checked unrelated import %s:\n%s", other, calls)
		}
	}
}

func TestPackCacheDirtyCommandRefusedBeforeExecution(t *testing.T) {
	f := newPackCacheFixture(t, 1)
	// Make the change after discovery: execution must not reuse a QUICK result.
	root := &cobra.Command{Use: "gc"}
	var stderr bytes.Buffer
	registerPackCommands(root, nil, io.Discard, &stderr)
	if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	root.SetArgs([]string{"sample0", "report"})
	if err := root.Execute(); err == nil {
		t.Fatal("dirty command executed successfully")
	}
	want := "cached import https://example.com/cache0.git has local worktree changes; run \"gc import install\""
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want %s", stderr.String(), want)
	}
	if _, err := os.Stat(f.marker); !os.IsNotExist(err) {
		t.Fatalf("script side effect exists or stat failed: %v", err)
	}
}

func TestPackCacheFullLifecycleLoadsRejectDirtyImport(t *testing.T) {
	for name, load := range map[string]func(string) error{
		"start":             func(city string) error { _, _, err := loadStartCityConfig(city); return err },
		"supervisor":        func(city string) error { _, _, err := loadSupervisorCityConfig(city); return err },
		"config-validation": func(city string) error { _, _, err := loadConfigCommandCityConfig(city); return err },
		"doctor":            func(city string) error { _, err := loadCityConfigAllowMissingProviderReferences(city); return err },
		"import":            validateComposedConfigAfterInstall,
	} {
		t.Run(name, func(t *testing.T) {
			f := newPackCacheFixture(t, 1)
			if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), []byte("changed"), 0o644); err != nil {
				t.Fatal(err)
			}
			err := load(f.city)
			if err == nil || !strings.Contains(err.Error(), "has local worktree changes; run \"gc import install\"") {
				t.Fatalf("FULL load error = %v", err)
			}
			if calls := f.gitCalls(t); !strings.Contains(calls, "status --porcelain") {
				t.Fatalf("FULL scan count: %s", calls)
			}
		})
	}
}

func TestPackCacheImportReadCommandsValidateCaches(t *testing.T) {
	for _, args := range [][]string{{"list"}, {"why", "sample0"}} {
		t.Run(args[0], func(t *testing.T) {
			f := newPackCacheFixture(t, 1)
			if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), []byte("changed"), 0o644); err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd := newImportCmd(io.Discard, &stderr)
			cmd.SetArgs(args)
			if err := cmd.Execute(); err == nil {
				t.Fatal("import inspection accepted dirty cache")
			}
			if calls := f.gitCalls(t); !strings.Contains(calls, "status --porcelain") {
				t.Fatalf("import did not scan: %s", calls)
			}
		})
	}
}

func TestPackCacheDiscovery3000FilesTiming(t *testing.T) {
	path := os.Getenv("PATH")
	f := newPackCacheFixture(t, 1)
	t.Setenv("PATH", path)
	cache := f.caches[0]
	if err := os.RemoveAll(filepath.Join(cache, ".git")); err != nil {
		t.Fatal(err)
	}
	for i := range 3000 {
		dir := filepath.Join(cache, "fixture", fmt.Sprintf("%02d", i/100))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%04d.txt", i)), []byte("fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runRegistryPublishGit(t, cache, "init", "-q")
	runRegistryPublishGit(t, cache, "add", ".")
	runRegistryPublishGit(t, cache, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "-c", "commit.gpgsign=false", "commit", "-qm", "fixture")
	commit := runRegistryPublishGit(t, cache, "rev-parse", "HEAD")
	newCache := filepath.Join(filepath.Dir(cache), config.RepoCacheKey("https://example.com/cache0.git", commit))
	if err := os.Rename(cache, newCache); err != nil {
		t.Fatal(err)
	}
	lock := fmt.Sprintf("schema = 1\n[packs.\"https://example.com/cache0.git\"]\ncommit = %q\n", commit)
	if err := os.WriteFile(filepath.Join(f.city, "packs.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := advisoryLoad
	t.Cleanup(func() { advisoryLoad = previous })
	for _, mode := range []config.CacheValidationMode{config.CacheValidationFull, config.CacheValidationQuick} {
		config.ResetRemoteCacheValidationCache()
		advisoryLoad.RemoteCacheValidation = mode
		root := &cobra.Command{Use: "gc"}
		started := time.Now()
		registerPackCommands(root, []string{"version"}, io.Discard, io.Discard)
		elapsed := time.Since(started)
		if findSubcommand(root, "sample0") == nil {
			t.Fatal("fixture command was not discovered")
		}
		t.Logf("discovery fixture_files=3000 mode=%d elapsed=%s (FULL=0 is previous behavior; QUICK=1)", mode, elapsed)
	}
}
