package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/cacheexec"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestHistoricalSyntheticCacheBindsPersistedAddress(t *testing.T) {
	pack := builtinpacks.All()[0]
	source, ok := builtinpacks.Source(pack.Name)
	if !ok {
		t.Fatal("missing bundled source")
	}
	for _, source := range []string{source, strings.Replace(source, ".git//", "//", 1)} {
		t.Run(source, func(t *testing.T) { testHistoricalSyntheticCacheAddress(t, source) })
	}
}

func testHistoricalSyntheticCacheAddress(t *testing.T, source string) {
	ResetRemoteCacheValidationCache()
	t.Cleanup(ResetRemoteCacheValidationCache)
	repository, ok := builtinpacks.RepositoryForSource(source)
	if !ok {
		t.Fatal("missing bundled repository")
	}
	commit := strings.TrimPrefix(BundledSourcePinnedVersion(source), "sha:")
	dir := filepath.Join(t.TempDir(), RepoCacheKey(source, commit))
	if err := builtinpacks.MaterializeSyntheticRepo(dir, repository, commit); err != nil {
		t.Fatal(err)
	}
	RegisterHistoricalRemoteCache(dir)
	if err := cacheexec.ValidateCachedExecutable(t.Context(), filepath.Join(dir, "check.sh")); err != nil {
		t.Fatalf("intact historical bundled cache refused: %v", err)
	}
	markerPath := filepath.Join(dir, ".gc-bundled-pack-cache.toml")
	marker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	hash, _, _ := strings.Cut(builtinpacks.SyntheticCacheKeyComponent(), "+")
	changed := strings.ReplaceAll(string(marker), hash, "sha256:"+strings.Repeat("0", 64))
	if err := os.WriteFile(markerPath, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cacheexec.ValidateCachedExecutable(t.Context(), filepath.Join(dir, "check.sh")); err == nil {
		t.Fatal("changed historical marker authorized itself at the old address")
	}
}

func TestQuickCacheLoadUsesNoGitProcess(t *testing.T) {
	for _, shape := range []string{"detached", "branch", "packed"} {
		t.Run(shape, func(t *testing.T) {
			city, cache := setupQuickCacheLoad(t)
			switch shape {
			case "branch":
				writeTestFile(t, cache, ".git/HEAD", "ref: refs/heads/main\n")
				writeTestFile(t, cache, ".git/refs/heads/main", lockedPackRefCommit+"\n")
			case "packed":
				writeTestFile(t, cache, ".git/HEAD", "ref: refs/heads/main\n")
				writeTestFile(t, cache, ".git/packed-refs", "# pack-refs with: peeled fully-peeled sorted\n"+lockedPackRefCommit+" refs/heads/main\n")
			}
			calls := 0
			runRepoCacheGit = func(_ string, args ...string) (string, error) {
				calls++
				if args[0] == "rev-parse" {
					return lockedPackRefCommit, nil
				}
				return "", nil
			}
			_, _, err := LoadWithIncludesOptions(fsys.OSFS{}, filepath.Join(city, "city.toml"), LoadOptions{SkipRevisionSnapshot: true, RemoteCacheValidation: CacheValidationQuick})
			if err != nil {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Fatalf("QUICK %s invoked git %d times; want zero child processes", shape, calls)
			}
		})
	}
}

func TestQuickCacheLoadRejectsWrongCommit(t *testing.T) {
	city, cache := setupQuickCacheLoad(t)
	wrong := strings.Repeat("1", 40)
	writeTestFile(t, cache, ".git/HEAD", wrong+"\n")
	_, _, err := LoadWithIncludesOptions(fsys.OSFS{}, filepath.Join(city, "city.toml"), LoadOptions{SkipRevisionSnapshot: true, RemoteCacheValidation: CacheValidationQuick})
	want := fmt.Sprintf("cached import https://example.com/pack.git is checked out at %s, expected %s; run \"gc import install\"", wrong, lockedPackRefCommit)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v; want %s", err, want)
	}
}

func TestQuickDirtyCacheIsRefusedByNextFullLoad(t *testing.T) {
	city, _ := setupQuickCacheLoad(t)
	statusCalls := 0
	runRepoCacheGit = func(_ string, args ...string) (string, error) {
		if args[0] == "rev-parse" {
			return lockedPackRefCommit, nil
		}
		statusCalls++
		return " M commands/report/run.sh\n", nil
	}
	path := filepath.Join(city, "city.toml")
	if _, _, err := LoadWithIncludesOptions(fsys.OSFS{}, path, LoadOptions{SkipRevisionSnapshot: true, RemoteCacheValidation: CacheValidationQuick}); err != nil {
		t.Fatalf("QUICK rejected dirty cache: %v", err)
	}
	if statusCalls != 0 {
		t.Fatalf("QUICK scanned worktree %d times", statusCalls)
	}
	_, _, err := LoadWithIncludesOptions(fsys.OSFS{}, path, LoadOptions{SkipRevisionSnapshot: true})
	want := "cached import https://example.com/pack.git has local worktree changes; run \"gc import install\""
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("FULL error = %v; want %s", err, want)
	}
	if statusCalls == 0 {
		t.Fatal("FULL did not scan the worktree")
	}
}

func setupQuickCacheLoad(t *testing.T) (string, string) {
	t.Helper()
	original := runRepoCacheGitContext
	t.Cleanup(func() { runRepoCacheGitContext = original })
	runRepoCacheGitContext = func(_ context.Context, dir string, args ...string) (string, error) {
		return runRepoCacheGit(dir, args...)
	}
	city, cache := setupLockedPackRefTest(t, "https://example.com/pack.git")
	writeTestFile(t, city, "city.toml", "[workspace]\nname = \"cache-test\"\n[imports.sample]\nsource = \"https://example.com/pack.git\"\n")
	writeTestFile(t, cache, "pack.toml", "[pack]\nname = \"sample\"\nschema = 1\n")
	writeTestFile(t, cache, ".git/HEAD", lockedPackRefCommit+"\n")
	return city, cache
}

func TestQuickCacheUnknownHEADFallsBackOnceWithoutScan(t *testing.T) {
	_, cache := setupQuickCacheLoad(t)
	writeTestFile(t, cache, ".git/HEAD", "ref: refs/heads/unavailable\n")
	var calls []string
	runRepoCacheGit = func(_ string, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return lockedPackRefCommit, nil
	}
	if err := validateLockedRemoteCache("https://example.com/pack.git", cache, lockedPackRefCommit, CacheValidationQuick); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != "rev-parse HEAD" {
		t.Fatalf("fallback calls = %v", calls)
	}
}

func TestQuickGitfileFallsBackWithoutScan(t *testing.T) {
	_, cache := setupQuickCacheLoad(t)
	if err := os.RemoveAll(filepath.Join(cache, ".git")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, cache, ".git", "gitdir: elsewhere\n")
	var calls []string
	runRepoCacheGit = func(_ string, args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return lockedPackRefCommit, nil
	}
	if err := validateInstalledRemoteCache("https://example.com/pack.git", cache, lockedPackRefCommit, CacheValidationQuick); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != "rev-parse HEAD" {
		t.Fatalf("fallback calls = %v", calls)
	}
}

func TestCachedExecutableCannotEscapeValidationThroughSymlink(t *testing.T) {
	city, cache := setupQuickCacheLoad(t)
	local := filepath.Join(t.TempDir(), "local.sh")
	if err := os.WriteFile(local, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(cache, "run.sh")
	if err := os.Symlink(local, script); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadWithIncludesOptions(fsys.OSFS{}, filepath.Join(city, "city.toml"), LoadOptions{SkipRevisionSnapshot: true, RemoteCacheValidation: CacheValidationQuick}); err != nil {
		t.Fatal(err)
	}
	runRepoCacheGit = func(_ string, args ...string) (string, error) {
		if args[0] == "rev-parse" {
			return lockedPackRefCommit, nil
		}
		return " M run.sh\n", nil
	}
	if err := cacheexec.ValidateCachedExecutable(context.Background(), script); err == nil || !strings.Contains(err.Error(), "has local worktree changes") {
		t.Fatalf("symlink escaped cached import validation: %v", err)
	}
}

func TestFullReloadDoesNotForgetOtherCitiesExecutableOwners(t *testing.T) {
	city, cache := setupQuickCacheLoad(t)
	script := filepath.Join(cache, "run.sh")
	writeTestFile(t, cache, "run.sh", "#!/bin/sh\nexit 0\n")
	if _, _, err := LoadWithIncludesOptions(fsys.OSFS{}, filepath.Join(city, "city.toml"), LoadOptions{SkipRevisionSnapshot: true, RemoteCacheValidation: CacheValidationQuick}); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	writeTestFile(t, other, "city.toml", "[workspace]\nname = \"other\"\n")
	if _, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(other, "city.toml")); err != nil {
		t.Fatal(err)
	}
	InvalidateRemoteCacheValidations()
	statusCalls := 0
	runRepoCacheGit = func(_ string, args ...string) (string, error) {
		if args[0] == "rev-parse" {
			return lockedPackRefCommit, nil
		}
		statusCalls++
		return "", nil
	}
	if err := cacheexec.ValidateCachedExecutable(context.Background(), script); err != nil {
		t.Fatalf("another city reload discarded owner: %v", err)
	}
	if statusCalls != 1 {
		t.Fatalf("owning import scans = %d, want 1", statusCalls)
	}
}

func TestCachedShellExecutionChecksCodeButNotReadArguments(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		scans         int
	}{
		{"direct", "\"$PACK_SCRIPT\" --flag", 1},
		{"interpreter", "sh \"$PACK_SCRIPT\"", 1},
		{"read", "cat \"$PACK_SCRIPT\"", 0},
		{"copy", "cp \"$PACK_SCRIPT\" copied.sh", 0},
		{"conditional-read", "if true; then cat \"$PACK_SCRIPT\"; fi", 0},
		{"nested-shell", "sh -c 'sh -c \"$LAUNCHER\"'", 1},
		{"recursive-eval", "LOOP='eval \"$LOOP\"'; eval \"$LOOP\"", 1},
		{"substitution", "echo $(\"$PACK_SCRIPT\")", 1},
		{"dynamic", "eval \"$LAUNCHER\"", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city, cache := setupQuickCacheLoad(t)
			script := filepath.Join(cache, "run.sh")
			writeTestFile(t, cache, "run.sh", "#!/bin/sh\nexit 0\n")
			if _, _, err := LoadWithIncludesOptions(fsys.OSFS{}, filepath.Join(city, "city.toml"), LoadOptions{SkipRevisionSnapshot: true, RemoteCacheValidation: CacheValidationQuick}); err != nil {
				t.Fatal(err)
			}
			statusCalls := 0
			runRepoCacheGit = func(_ string, args ...string) (string, error) {
				if args[0] == "rev-parse" {
					return lockedPackRefCommit, nil
				}
				statusCalls++
				return "", nil
			}
			if err := cacheexec.ValidateCachedShellCommand(context.Background(), tc.command, city, append(os.Environ(), "PACK_SCRIPT="+script, "LAUNCHER="+script)); err != nil {
				t.Fatal(err)
			}
			if statusCalls != tc.scans {
				t.Fatalf("status calls = %d, want %d", statusCalls, tc.scans)
			}
		})
	}
}

func TestShellFallbackIncludesRemoteFragments(t *testing.T) {
	city, cache := setupQuickCacheLoad(t)
	writeTestFile(t, cache, "agents.toml", "[[agent]]\nname = \"fragment-worker\"\n")
	writeTestFile(t, city, "city.toml", "include = [\"https://example.com/pack.git//agents.toml\"]\n[workspace]\nname = \"fragment-city\"\n")
	if _, _, err := LoadWithIncludesOptions(fsys.OSFS{}, filepath.Join(city, "city.toml"), LoadOptions{SkipRevisionSnapshot: true, RemoteCacheValidation: CacheValidationQuick}); err != nil {
		t.Fatal(err)
	}
	scans := 0
	runRepoCacheGit = func(_ string, args ...string) (string, error) {
		if args[0] == "rev-parse" {
			return lockedPackRefCommit, nil
		}
		scans++
		return " M run.sh", nil
	}
	env := []string{"PACK_SCRIPT=" + filepath.Join(cache, "run.sh")}
	if err := cacheexec.ValidateCachedShellCommand(t.Context(), "cat \"$PACK_SCRIPT\"", city, env); err != nil || scans != 0 {
		t.Fatalf("fragment read must remain QUICK: err=%v scans=%d", err, scans)
	}
	if err := cacheexec.ValidateCachedShellCommand(t.Context(), "SCRIPT=\"$PACK_SCRIPT\"; \"$SCRIPT\"", city, env); err == nil || scans != 1 {
		t.Fatalf("dynamic fragment execution escaped FULL: err=%v scans=%d", err, scans)
	}
}

func TestShellFallbackUsesSuccessfulActiveConfiguration(t *testing.T) {
	for _, scenario := range []string{"removed-import", "other-city", "failed-load"} {
		t.Run(scenario, func(t *testing.T) {
			city, cache := setupQuickCacheLoad(t)
			path := filepath.Join(city, "city.toml")
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			opts := LoadOptions{SkipRevisionSnapshot: true, RemoteCacheValidation: CacheValidationQuick}
			empty := "[workspace]\nname = \"empty\"\n"
			if scenario == "failed-load" {
				writeTestFile(t, city, "city.toml", empty)
			}
			if _, _, err := LoadWithIncludesOptions(fsys.OSFS{}, path, opts); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "removed-import":
				writeTestFile(t, city, "city.toml", empty)
				writeTestFile(t, city, "packs.lock", "")
			case "other-city":
				city = t.TempDir()
				path = filepath.Join(city, "city.toml")
				writeTestFile(t, city, "city.toml", empty)
			case "failed-load":
				writeTestFile(t, city, "city.toml", string(original))
				writeTestFile(t, cache, "pack.toml", "[invalid TOML")
			}
			_, _, err = LoadWithIncludesOptions(fsys.OSFS{}, path, opts)
			if scenario == "failed-load" {
				if err == nil {
					t.Fatal("invalid pack unexpectedly loaded")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(cache); err != nil {
				t.Fatal(err)
			}
			if err := cacheexec.ValidateCachedShellCommand(context.Background(), "FLAG=1 /bin/true", city, nil); err != nil {
				t.Fatalf("historical import blocked active local execution: %v", err)
			}
		})
	}
}

func TestCachedExecutionCancellationDuringGitValidation(t *testing.T) {
	city, cache := setupQuickCacheLoad(t)
	if _, _, err := LoadWithIncludesOptions(fsys.OSFS{}, filepath.Join(city, "city.toml"), LoadOptions{SkipRevisionSnapshot: true, RemoteCacheValidation: CacheValidationQuick}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	runRepoCacheGitContext = func(ctx context.Context, _ string, args ...string) (string, error) {
		if args[0] == "rev-parse" {
			return lockedPackRefCommit, nil
		}
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- cacheexec.ValidateCachedExecutable(ctx, filepath.Join(cache, "run.sh"))
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Git validation error = %v; want cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Git validation ignored cancellation")
	}
}
