//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWithRepoCacheReadLockDoesNotCreateMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	called := false
	if err := WithRepoCacheReadLock(root, func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("WithRepoCacheReadLock: %v", err)
	}
	if !called {
		t.Fatal("read lock callback was not called")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("root stat err = %v, want not exist", err)
	}
}

func TestWithRepoCacheReadLockCreatesLockFileForExistingRoot(t *testing.T) {
	root := t.TempDir()
	if err := WithRepoCacheReadLock(root, func() error { return nil }); err != nil {
		t.Fatalf("WithRepoCacheReadLock: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, repoCacheLockName)); err != nil {
		t.Fatalf("lock file stat: %v", err)
	}
}

func TestRepoCacheRootForPathUsesKnownCacheRootsOnly(t *testing.T) {
	home := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "gc-home")
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	t.Setenv("GC_HOME", gcHome)

	homeRoot := filepath.Join(home, ".gc", "cache", "repos")
	gcHomeRoot := filepath.Join(gcHome, "cache", "repos")
	for _, tc := range []struct {
		name string
		path string
		root string
		ok   bool
	}{
		{
			name: "home cache child",
			path: filepath.Join(homeRoot, "abc123", "pack.toml"),
			root: homeRoot,
			ok:   true,
		},
		{
			name: "gc home cache child",
			path: filepath.Join(gcHomeRoot, "def456", "pack.toml"),
			root: gcHomeRoot,
			ok:   true,
		},
		{
			name: "unrelated cache repos substring",
			path: filepath.Join(t.TempDir(), "cache", "repos", "project", "pack.toml"),
			ok:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := repoCacheRootForPath(tc.path)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (root %q)", ok, tc.ok, got)
			}
			if ok && got != tc.root {
				t.Fatalf("root = %q, want %q", got, tc.root)
			}
		})
	}
}

func TestIsRepoCachePathIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"empty_outside", false},
		{"empty_inside", true},
		{"missing_inside", true},
		{"missing_outside", false},
		{"root", true},
		{"case_variant", true},
		{"outer_link", true},
		{"outer_file_link", true},
		{"outer_directory_link", true},
		{"linked_home", true},
		{"loop", true},
		{"outside", false},
		{"mimic", false},
		{"relative_link_behind_parent_link", true},
		{"not_directory", true},
		{"root_aliases_volume", true},
		{"missing_before_parent", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			home := filepath.Join(base, "gchome")
			root := filepath.Join(home, "cache", "repos")
			t.Setenv("GC_HOME", home)
			t.Setenv("HOME", base)
			write := func(path string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			link := func(target, path string) string {
				t.Helper()
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
				return path
			}
			script := filepath.Join(root, "pack", "run.sh")
			outside := filepath.Join(base, "outside", "run.sh")
			write(script)
			write(outside)
			path := outside
			switch tc.name {
			case "empty_outside", "empty_inside":
				cwd := base
				if tc.name == "empty_inside" {
					cwd = root
				}
				t.Chdir(cwd)
				path = ""
			case "missing_inside":
				path = filepath.Join(root, "missing", "leaf")
			case "missing_outside":
				path = filepath.Join(base, "missing", "leaf")
			case "missing_before_parent":
				path = link("missing/../gchome/cache/repos/pack/run.sh", filepath.Join(base, "link"))
			case "root":
				path = root
			case "case_variant":
				path = strings.Replace(script, "/gchome/", "/GCHOME/", 1)
				original, err := os.Stat(script)
				if err != nil {
					t.Fatal(err)
				}
				other, err := os.Stat(path)
				if os.IsNotExist(err) || (err == nil && !os.SameFile(original, other)) {
					t.Skip("filesystem does not fold letter case")
				}
				if err != nil {
					t.Fatal(err)
				}
			case "outer_link":
				path = link(script, filepath.Join(base, "link"))
			case "outer_file_link":
				if err := os.Remove(script); err != nil {
					t.Fatal(err)
				}
				link(outside, script)
				path = link(script, filepath.Join(base, "link"))
			case "outer_directory_link":
				cached := link(filepath.Dir(outside), filepath.Join(root, "linked-dir"))
				path = link(filepath.Join(cached, "run.sh"), filepath.Join(base, "link"))
			case "linked_home":
				t.Setenv("GC_HOME", link(home, filepath.Join(base, "linked-home")))
				var err error
				path, err = filepath.EvalSymlinks(script)
				if err != nil {
					t.Fatal(err)
				}
			case "loop":
				path = link("loop", filepath.Join(base, "loop"))
			case "mimic":
				path = filepath.Join(base, "mimic", "cache", "repos", strings.Repeat("a", 64), "run.sh")
				write(path)
			case "relative_link_behind_parent_link":
				parent := filepath.Dir(outside)
				rel, err := filepath.Rel(parent, script)
				if err != nil {
					t.Fatal(err)
				}
				link(rel, filepath.Join(parent, "relative"))
				path = filepath.Join(link(parent, filepath.Join(base, "parent")), "relative")
			case "not_directory":
				path = filepath.Join(outside, "leaf")
			case "root_aliases_volume":
				if err := os.RemoveAll(root); err != nil {
					t.Fatal(err)
				}
				link(string(os.PathSeparator), root)
			}
			if got := IsRepoCachePath(path); got != tc.want {
				t.Errorf("IsRepoCachePath(%q) = %v, want %v", path, got, tc.want)
			}
		})
	}
}

func TestWithRepoCacheReadLockWaitsOnCacheRoot(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, repoCacheLockName)
	lockDir, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("Open(root): %v", err)
	}
	defer lockDir.Close() //nolint:errcheck
	if err := syscall.Flock(int(lockDir.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("Flock(exclusive): %v", err)
	}

	entered := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- WithRepoCacheReadLock(root, func() error {
			close(entered)
			return nil
		})
	}()

	select {
	case <-entered:
		t.Fatal("read lock entered while exclusive lock was held")
	case <-time.After(50 * time.Millisecond):
	}

	if err := syscall.Flock(int(lockDir.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("Flock(unlock): %v", err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("read lock did not enter after exclusive lock released")
	}
	if err := <-errCh; err != nil {
		t.Fatalf("WithRepoCacheReadLock: %v", err)
	}
}
