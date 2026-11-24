package packman

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
)

func TestEnsureRepoInCacheRefreshFailurePreservesCleanCheckout(t *testing.T) {
	for _, bundled := range []bool{false, true} {
		t.Run(fmt.Sprintf("bundled_%t", bundled), func(t *testing.T) {
			t.Setenv("GC_HOME", t.TempDir())
			source, commit := "https://example.invalid/unreachable.git", "abcdef1234567890abcdef1234567890abcdef12"
			if bundled {
				source = builtinpacks.MustSource("core")
				commit = canonicalBundledCommit(source)
			}
			dir, err := RepoCachePath(source, commit)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"fixture\"\nschema = 1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			lock := filepath.Join(dir, ".git", "index.lock")
			if err := os.WriteFile(lock, []byte("stale lock"), 0o600); err != nil {
				t.Fatal(err)
			}
			var calls []string
			old, oldNetwork := runGit, runNetworkGit
			runNetworkGit = func(_, _, _ string, args ...string) (string, error) {
				calls = append(calls, strings.Join(args, " "))
				return "", fmt.Errorf("source unreachable")
			}
			runGit = func(_ string, args ...string) (string, error) {
				calls = append(calls, strings.Join(args, " "))
				switch args[0] {
				case "rev-parse":
					return commit, nil
				case "status":
					return "", nil
				case "update-index":
					return "", fmt.Errorf("index.lock already exists")
				default:
					return "", fmt.Errorf("source unreachable")
				}
			}
			t.Cleanup(func() { runGit, runNetworkGit = old, oldNetwork })
			_, err = EnsureRepoInCache("", source, commit)
			if err == nil || !strings.Contains(err.Error(), "refreshing cached repo index") {
				t.Errorf("refresh failure must be reported independently: %v", err)
			}
			if data, err := os.ReadFile(lock); err != nil || string(data) != "stale lock" {
				t.Errorf("clean checkout was replaced or deleted: lock=%q err=%v", data, err)
			}
			if want := []string{"rev-parse HEAD", "status --porcelain", "update-index --refresh"}; !reflect.DeepEqual(calls, want) {
				t.Errorf("refresh failure entered recovery: calls=%q", calls)
			}
		})
	}
}

func TestRepoCacheKeyDeterministic(t *testing.T) {
	a := RepoCacheKey("https://github.com/example/repo", "abc123")
	b := RepoCacheKey("https://github.com/example/repo", "abc123")
	c := RepoCacheKey("https://github.com/example/repo", "def456")
	if a != b {
		t.Fatalf("equal inputs produced different keys: %q != %q", a, b)
	}
	if a == c {
		t.Fatalf("different commits produced same key: %q", a)
	}
}

func TestEnsureRepoInCacheInvalidatesValidationAndRefreshesIndex(t *testing.T) {
	home := t.TempDir()
	t.Setenv("GC_HOME", home)
	const source = "https://example.invalid/pack.git"
	const commit = "abcdef1234567890abcdef1234567890abcdef12"
	dir, err := RepoCachePath(source, commit)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"fixture\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(dir, ".git/gc-validation-v1.json")
	if err := os.WriteFile(receipt, []byte("old validation"), 0o600); err != nil {
		t.Fatal(err)
	}
	var refreshed bool
	old := runGit
	runGit = func(_ string, args ...string) (string, error) {
		if _, err := os.Stat(receipt); !os.IsNotExist(err) {
			t.Fatalf("install must invalidate before invoking git: %v", err)
		}
		switch args[0] {
		case "rev-parse":
			return commit, nil
		case "status":
			return "", nil
		case "update-index":
			if !reflect.DeepEqual(args, []string{"update-index", "--refresh"}) {
				t.Fatalf("refresh args: %q", args)
			}
			refreshed = true
			return "", nil
		default:
			t.Fatalf("unexpected git args: %q", args)
			return "", nil
		}
	}
	t.Cleanup(func() { runGit = old })
	if _, err := EnsureRepoInCache("", source, commit); err != nil {
		t.Fatal(err)
	}
	if !refreshed {
		t.Fatal("install did not refresh the index for read-only validation")
	}
}

func TestRepoCachePathUsesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	got, err := RepoCachePath("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if !strings.HasPrefix(got, filepath.Join(home, ".gc", "cache", "repos")) {
		t.Fatalf("RepoCachePath = %q", got)
	}
}

func TestRepoCacheKeyNormalizesSubpathSources(t *testing.T) {
	plain := RepoCacheKey("file:///tmp/repo.git", "abc123")
	subpath := RepoCacheKey("file:///tmp/repo.git//packs/base", "abc123")
	if plain != subpath {
		t.Fatalf("RepoCacheKey should ignore subpath for cache identity: %q != %q", plain, subpath)
	}
}

func TestRepoCacheKeyNormalizesGitHubShortcut(t *testing.T) {
	shortcut := RepoCacheKey("github.com/example/repo", "abc123")
	https := RepoCacheKey("https://github.com/example/repo", "abc123")
	if shortcut != https {
		t.Fatalf("RepoCacheKey should normalize bare github shortcut: %q != %q", shortcut, https)
	}
}

func TestEnsureRepoInCacheUsesExistingCloneWhenCheckoutMatches(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	path, err := RepoCachePath("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "pack.toml"), []byte("[pack]\nname = \"repo\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}

	var calls [][]string
	prev := runGit
	runGit = func(_ string, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		if reflect.DeepEqual(args, []string{"rev-parse", "HEAD"}) {
			return "abc123", nil
		}
		if reflect.DeepEqual(args, []string{"status", "--porcelain"}) || reflect.DeepEqual(args, []string{"update-index", "--refresh"}) {
			return "", nil
		}
		return "", fmt.Errorf("unexpected git call: %v", args)
	}
	t.Cleanup(func() { runGit = prev })

	got, err := EnsureRepoInCache("", "https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("EnsureRepoInCache: %v", err)
	}
	if got != path {
		t.Fatalf("EnsureRepoInCache path = %q, want %q", got, path)
	}
	want := [][]string{
		{"rev-parse", "HEAD"},
		{"status", "--porcelain"},
		{"update-index", "--refresh"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("git calls = %#v, want %#v", calls, want)
	}
}

func TestEnsureRepoInCacheRepairsDirtyMatchingCheckout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	path, err := RepoCachePath("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "pack.toml"), []byte("not toml"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}

	var calls [][]string
	prev := runGit
	runGit = func(_ string, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "rev-parse":
			return "abc123", nil
		case "status":
			return " M pack.toml", nil
		case "reset":
			if err := os.WriteFile(filepath.Join(path, "pack.toml"), []byte("[pack]\nname = \"repo\"\nschema = 1\n"), 0o644); err != nil {
				return "", err
			}
			return "", nil
		case "clean", "update-index":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected git call: %v", args)
		}
	}
	t.Cleanup(func() { runGit = prev })

	got, err := EnsureRepoInCache("", "https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("EnsureRepoInCache: %v", err)
	}
	if got != path {
		t.Fatalf("EnsureRepoInCache path = %q, want %q", got, path)
	}
	want := [][]string{
		{"rev-parse", "HEAD"},
		{"status", "--porcelain"},
		{"reset", "--hard", "--quiet", "abc123"},
		{"clean", "-ffdx", "--quiet"},
		{"update-index", "--refresh"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("git calls = %#v, want %#v", calls, want)
	}
}

func TestEnsureRepoInCacheRepairsExistingCloneCheckout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	path, err := RepoCachePath("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "pack.toml"), []byte("[pack]\nname = \"repo\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}

	var calls [][]string
	prev := runGit
	runGit = func(_ string, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "rev-parse":
			return "def456", nil
		case "checkout":
			return "", nil
		case "reset":
			return "", nil
		case "clean", "update-index":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected git call: %v", args)
		}
	}
	t.Cleanup(func() { runGit = prev })

	got, err := EnsureRepoInCache("", "https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("EnsureRepoInCache: %v", err)
	}
	if got != path {
		t.Fatalf("EnsureRepoInCache path = %q, want %q", got, path)
	}
	want := [][]string{
		{"rev-parse", "HEAD"},
		{"checkout", "--quiet", "abc123"},
		{"reset", "--hard", "--quiet", "abc123"},
		{"clean", "-ffdx", "--quiet"},
		{"update-index", "--refresh"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("git calls = %#v, want %#v", calls, want)
	}
}

func TestEnsureRepoInCacheReclonesInvalidExistingCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	path, err := RepoCachePath("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	var calls [][]string
	prev := runGit
	runGit = func(_ string, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "rev-parse":
			return "abc123", nil
		case "status":
			return "", nil
		case "checkout", "update-index":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected git call: %v", args)
		}
	}
	t.Cleanup(func() { runGit = prev })
	prevNet := runNetworkGit
	runNetworkGit = func(_, _, _ string, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "clone" {
			target := args[len(args)-1]
			if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(target, "pack.toml"), []byte("[pack]\nname = \"repo\"\nschema = 1\n"), 0o644); err != nil {
				return "", err
			}
			return "", nil
		}
		return "", fmt.Errorf("unexpected network git call: %v", args)
	}
	t.Cleanup(func() { runNetworkGit = prevNet })

	got, err := EnsureRepoInCache("", "https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("EnsureRepoInCache: %v", err)
	}
	if got != path {
		t.Fatalf("EnsureRepoInCache path = %q, want %q", got, path)
	}
	want := [][]string{
		{"rev-parse", "HEAD"},
		{"status", "--porcelain"},
		{"update-index", "--refresh"},
		{"clone", "--quiet", "https://github.com/example/repo", path},
		{"checkout", "--quiet", "abc123"},
		{"update-index", "--refresh"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("git calls = %#v, want %#v", calls, want)
	}
}

func TestEnsureRepoInCacheCleansFreshCloneAfterPackValidationFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	path, err := RepoCachePath("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}

	prev := runGit
	runGit = func(_ string, args ...string) (string, error) {
		switch args[0] {
		case "checkout", "update-index":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected git call: %v", args)
		}
	}
	t.Cleanup(func() { runGit = prev })
	prevNet := runNetworkGit
	runNetworkGit = func(_, _, _ string, args ...string) (string, error) {
		if args[0] == "clone" {
			target := args[len(args)-1]
			if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
				return "", err
			}
			return "", nil
		}
		return "", fmt.Errorf("unexpected network git call: %v", args)
	}
	t.Cleanup(func() { runNetworkGit = prevNet })

	if _, err := EnsureRepoInCache("", "https://github.com/example/repo", "abc123"); err == nil {
		t.Fatal("EnsureRepoInCache succeeded, want pack validation error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cache path still exists after validation failure: %v", err)
	}
}

func TestEnsureRepoInCacheReclonesCacheDirWithoutGit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	path, err := RepoCachePath("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "leftover.txt"), []byte("stale"), 0o644); err != nil {
		t.Fatalf("WriteFile(leftover): %v", err)
	}

	var calls [][]string
	prev := runGit
	runGit = func(_ string, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "checkout", "update-index":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected git call: %v", args)
		}
	}
	t.Cleanup(func() { runGit = prev })
	prevNet := runNetworkGit
	runNetworkGit = func(_, _, _ string, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "clone" {
			target := args[len(args)-1]
			if _, err := os.Stat(filepath.Join(target, "leftover.txt")); !os.IsNotExist(err) {
				return "", fmt.Errorf("stale cache directory was not removed before clone")
			}
			if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(target, "pack.toml"), []byte("[pack]\nname = \"repo\"\nschema = 1\n"), 0o644); err != nil {
				return "", err
			}
			return "", nil
		}
		return "", fmt.Errorf("unexpected network git call: %v", args)
	}
	t.Cleanup(func() { runNetworkGit = prevNet })

	got, err := EnsureRepoInCache("", "https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("EnsureRepoInCache: %v", err)
	}
	if got != path {
		t.Fatalf("EnsureRepoInCache path = %q, want %q", got, path)
	}
	want := [][]string{
		{"clone", "--quiet", "https://github.com/example/repo", path},
		{"checkout", "--quiet", "abc123"},
		{"update-index", "--refresh"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("git calls = %#v, want %#v", calls, want)
	}
}

func TestEnsureRepoInCacheReclonesCacheFileWithoutGit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	path, err := RepoCachePath("https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatalf("WriteFile(cachePath): %v", err)
	}

	prev := runGit
	runGit = func(_ string, args ...string) (string, error) {
		switch args[0] {
		case "checkout", "update-index":
			return "", nil
		default:
			return "", fmt.Errorf("unexpected git call: %v", args)
		}
	}
	t.Cleanup(func() { runGit = prev })
	prevNet := runNetworkGit
	runNetworkGit = func(_, _, _ string, args ...string) (string, error) {
		if args[0] == "clone" {
			target := args[len(args)-1]
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				return "", fmt.Errorf("stale cache file was not removed before clone")
			}
			if err := os.MkdirAll(filepath.Join(target, ".git"), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(target, "pack.toml"), []byte("[pack]\nname = \"repo\"\nschema = 1\n"), 0o644); err != nil {
				return "", err
			}
			return "", nil
		}
		return "", fmt.Errorf("unexpected network git call: %v", args)
	}
	t.Cleanup(func() { runNetworkGit = prevNet })

	got, err := EnsureRepoInCache("", "https://github.com/example/repo", "abc123")
	if err != nil {
		t.Fatalf("EnsureRepoInCache: %v", err)
	}
	if got != path {
		t.Fatalf("EnsureRepoInCache path = %q, want %q", got, path)
	}
}

// TestDefaultRunGitBlocksDisallowedTransport is the regression for the API
// pack-import SSRF hardening: defaultRunGit drives the attacker-influenced
// clone/ls-remote, so it must constrain git transports. An ext:: URL (which
// would otherwise execute an arbitrary command) must be refused by the
// protocol allowlist rather than run.
func TestDefaultRunGitBlocksDisallowedTransport(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// ext:: is not in the allowlist; git must refuse it before running the
	// command. Without the hardening, git would execute `true` and fail with a
	// different (protocol-parse) error instead.
	_, err := defaultRunGit("", "ls-remote", "ext::true")
	if err == nil {
		t.Fatal("defaultRunGit ran a disallowed ext:: transport; want a protocol block")
	}
	msg := err.Error()
	blocked := strings.Contains(msg, "ext") &&
		(strings.Contains(msg, "not allowed") || strings.Contains(msg, "protocol"))
	if !blocked {
		t.Fatalf("error = %q; want a git transport 'ext' not allowed / protocol block", msg)
	}
}
