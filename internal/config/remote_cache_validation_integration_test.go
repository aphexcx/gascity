//go:build integration && !windows

package config

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestRemoteCacheValidationAcrossProcesses(t *testing.T) {
	city, dir := setupLockedPackRefTest(t, validationTestSource)
	writeTestFile(t, city, "city.toml", fmt.Sprintf("[workspace]\nname = \"fixture\"\n[imports.fixture]\nsource = %q\n", validationTestSource))
	writeTestFile(t, dir, "pack.toml", "[pack]\nname = \"fixture\"\nschema = 2\n")
	for i, want := range []string{"calls=2", "calls=0"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRemoteCacheValidationProcessHelper$", "--", city, os.Getenv("GC_HOME"))
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), want) {
			t.Fatalf("process %d: %v; want %s; output: %s", i+1, err, want, out)
		}
	}
}

func TestCacheStatusDoesNotRefreshRealGitIndex(t *testing.T) {
	repo := initBareRepo(t, "readonly-status")
	dir := filepath.Join(t.TempDir(), "cache")
	mustGit(t, "", "clone", repo, dir)
	mustGit(t, dir, "update-index", "--refresh")
	before, err := os.ReadFile(filepath.Join(dir, ".git/index"))
	if err != nil {
		t.Fatal(err)
	}
	// Unchanged content with stale stat data makes ordinary git status refresh
	// the index. Read-only status must report clean without writing it.
	old := time.Unix(1000000, 0)
	if err := os.Chtimes(filepath.Join(dir, "pack.toml"), old, old); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_OPTIONAL_LOCKS", "1")
	status, err := defaultRunRepoCacheGit(dir, "status", "--porcelain")
	if err != nil || status != "" {
		t.Fatalf("status = %q, err = %v", status, err)
	}
	after, err := os.ReadFile(filepath.Join(dir, ".git/index"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("validation status rewrote the index")
	}
	if _, err := os.Stat(filepath.Join(dir, ".git/index.lock")); !os.IsNotExist(err) {
		t.Fatalf("status left an index lock: %v", err)
	}
}

func TestRemoteCacheValidationProcessHelper(t *testing.T) {
	args := os.Args
	if len(args) < 4 || args[len(args)-3] != "--" {
		t.Skip("subprocess helper")
	}
	t.Setenv("GC_HOME", args[len(args)-1])
	calls := 0
	runRepoCacheGit = func(_ string, args ...string) (string, error) {
		calls++
		if args[0] == "rev-parse" {
			return lockedPackRefCommit, nil
		}
		return "", nil
	}
	if _, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(args[len(args)-2], "city.toml")); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("calls=%d\n", calls)
}

func TestKilledCacheStatusDoesNotLeaveIndexLockOrRepeatContentReads(t *testing.T) {
	dir, _, _ := validationFixture(t)
	bin := t.TempDir()
	script := `#!/bin/sh
readonly_status=false
operation=
for arg in "$@"; do
  case "$arg" in
    --version) echo 'git version 2.50.0'; exit 0 ;;
    --no-optional-locks) readonly_status=true ;;
    rev-parse|status|update-index) operation=$arg ;;
  esac
done
printf '%s\n' "$operation" >> .git/calls
case "$operation" in
  rev-parse) cat .git/HEAD ;;
  update-index) touch .git/index-refreshed ;;
  status)
    if [ ! -f .git/index-refreshed ]; then echo content-read >> .git/content-reads; fi
    if [ "$readonly_status" = false ]; then touch .git/index.lock; fi
    if [ -f .git/kill-status ]; then
      rm -f .git/kill-status
      kill -KILL $$
    fi
    if [ "$readonly_status" = false ]; then rm -f .git/index.lock; fi
    ;;
  *) exit 99 ;;
esac
`
	writeTestFile(t, bin, "git", script)
	if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	runRepoCacheGit = defaultRunRepoCacheGit
	// Installation owns this explicit write. The packman unit test verifies
	// every successful install reaches update-index under its write lock.
	if _, err := defaultRunRepoCacheGit(dir, "update-index", "--refresh"); err != nil {
		t.Fatal(err)
	}
	if err := validateFixture(dir, lockedPackRefCommit); err != nil {
		t.Fatal(err)
	}
	if err := InvalidateRemoteCacheValidation(dir); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, ".git/kill-status", "kill after status starts")
	if err := validateFixture(dir, lockedPackRefCommit); err == nil || !strings.Contains(err.Error(), "worktree status") {
		t.Fatalf("killed status must fail validation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git/index.lock")); !os.IsNotExist(err) {
		t.Fatalf("killed status left an index lock: %v", err)
	}
	if err := validateFixture(dir, lockedPackRefCommit); err != nil {
		t.Fatalf("check after killed status: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git/content-reads")); !os.IsNotExist(err) {
		t.Fatalf("status re-read contents despite the installed index: %v", err)
	}
	calls, err := os.ReadFile(filepath.Join(dir, ".git/calls"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "update-index\n" + strings.Repeat("rev-parse\nstatus\n", 3); string(calls) != want {
		t.Fatalf("git calls after interruption = %q, want %q", calls, want)
	}
}
