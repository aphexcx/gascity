package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// prePushFixture is a throwaway git repo wired to run the real .githooks/pre-push
// against fake `bd` and `make` binaries.
type prePushFixture struct {
	repo      string
	binDir    string
	bdStdin   string
	makeRuns  string
	commitOld string
	commitNew string
	env       []string
}

// newPrePushFixture builds a repo with two commits — the second adds a Go file —
// so the hook's `git diff -- '*.go'` scan has something real to resolve.
func newPrePushFixture(t *testing.T) *prePushFixture {
	t.Helper()
	root := repoRoot(t)
	repo := t.TempDir()
	binDir := t.TempDir()
	recordDir := t.TempDir()

	f := &prePushFixture{
		repo:     repo,
		binDir:   binDir,
		bdStdin:  filepath.Join(recordDir, "bd-stdin"),
		makeRuns: filepath.Join(recordDir, "make-runs"),
	}
	f.env = append(os.Environ(),
		"PATH="+binDir+":/usr/bin:/bin",
		"GIT_CONFIG_GLOBAL="+filepath.Join(recordDir, "gitconfig"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(recordDir, "gitconfig-system"),
		"BD_STDIN_RECORD="+f.bdStdin,
		"MAKE_RECORD="+f.makeRuns,
	)

	// `bd` records exactly what the chain handed it on stdin.
	writeExecutable(t, filepath.Join(binDir, "bd"), `#!/usr/bin/env sh
cat > "$BD_STDIN_RECORD"
`)
	// Record each target and its Make variables in invocation order.
	writeExecutable(t, filepath.Join(binDir, "make"), `#!/usr/bin/env sh
printf '%s\n' "$*" >> "$MAKE_RECORD"
`)

	for _, dir := range []string{".githooks/lib", "scripts"} {
		if err := os.MkdirAll(filepath.Join(repo, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	for _, rel := range []string{".githooks/pre-push", ".githooks/lib/beads-chain.sh"} {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		writeExecutable(t, filepath.Join(repo, rel), string(body))
	}
	// The ownership guard talks to a live bead store; stub it so this test
	// isolates the hook's stdin composition.
	writeExecutable(t, filepath.Join(repo, "scripts", "push-ownership-guard.sh"), `#!/usr/bin/env bash
assert_bead_still_claimed() { return 0; }
`)

	f.git(t, "init", "-q", "-b", "main")
	f.git(t, "config", "user.email", "test@example.com")
	f.git(t, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	f.git(t, "add", "-A")
	f.git(t, "commit", "-q", "--no-verify", "-m", "base")
	f.commitOld = f.gitOut(t, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(repo, "touched.go"), []byte("package fixture\n"), 0o644); err != nil {
		t.Fatalf("write go file: %v", err)
	}
	f.git(t, "add", "-A")
	f.git(t, "commit", "-q", "--no-verify", "-m", "add go file")
	f.commitNew = f.gitOut(t, "rev-parse", "HEAD")

	return f
}

func (f *prePushFixture) git(t *testing.T, args ...string) {
	t.Helper()
	cmd := testCommand("git", args...)
	cmd.Dir = f.repo
	cmd.Env = f.env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (f *prePushFixture) gitOut(t *testing.T, args ...string) string {
	t.Helper()
	cmd := testCommand("git", args...)
	cmd.Dir = f.repo
	cmd.Env = f.env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// run invokes the fixture's pre-push hook with the given ref lines on stdin.
func (f *prePushFixture) run(t *testing.T, stdin string) (int, string) {
	t.Helper()
	cmd := testCommand(filepath.Join(f.repo, ".githooks", "pre-push"), "origin", "git@example.com:fixture.git")
	cmd.Dir = f.repo
	cmd.Env = f.env
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	if !asExitError(err, &exitErr) {
		t.Fatalf("run pre-push: %v\n%s", err, out)
	}
	return exitErr.ExitCode(), string(out)
}

func (f *prePushFixture) read(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// Git delivers the ref list once. Both beads and the checks must see it.
func TestPrePushReplaysRefListToBeadsAndSuiteScan(t *testing.T) {
	f := newPrePushFixture(t)
	refLine := "refs/heads/main " + f.commitNew + " refs/heads/main " + f.commitOld + "\n"

	code, out := f.run(t, refLine)
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}

	if got := f.read(t, f.bdStdin); got != refLine {
		t.Fatalf("beads received stdin %q, want %q", got, refLine)
	}
	f.checkGoPush(t, f.commitOld, out)
}

func (f *prePushFixture) checkGoPush(t *testing.T, base, output string) {
	t.Helper()
	scope := " LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=" + base
	targets := []string{
		"check-inventories GC_CHECK_INVENTORIES_TIMEOUT=300",
		"fmt-check-changed" + scope,
		"lint-changed" + scope,
		"vet",
		"test-fsys-darwin-compile",
		"test-pkg PKG=. COUNT=1",
	}
	var want strings.Builder
	for _, target := range targets {
		want.WriteString("--no-print-directory " + target + "\n")
		name := strings.Fields(target)[0]
		timing := regexp.MustCompile("(?m)^pre-push: " + name + ` took [0-9]+ s at load [0-9]+(\.[0-9]+)?$`)
		if !timing.MatchString(output) {
			t.Errorf("missing timing for %s in %q", name, output)
		}
	}
	if got := f.read(t, f.makeRuns); got != want.String() {
		t.Errorf("make invocations:\n%s\nwant:\n%s", got, want.String())
	}
}

func TestPrePushNewBranchBase(t *testing.T) {
	for _, ref := range []string{"refs/remotes/origin/integration", "refs/heads/alternate", "refs/heads/missing"} {
		t.Run(ref, func(t *testing.T) {
			f := newPrePushFixture(t)
			if ref != "refs/remotes/origin/integration" {
				f.env = append(f.env, "GC_PREPUSH_BASE_REF="+ref)
			}
			if ref != "refs/heads/missing" {
				// The target advanced independently after the branch point.
				tree := f.gitOut(t, "rev-parse", f.commitOld+"^{tree}")
				tip := f.gitOut(t, "commit-tree", tree, "-p", f.commitOld, "-m", "target")
				f.git(t, "update-ref", ref, tip)
			}
			code, out := f.run(t, "HEAD "+f.commitNew+" refs/heads/new "+strings.Repeat("0", 40)+"\n")
			if ref == "refs/heads/missing" {
				if code != 1 || !strings.Contains(out, "git fetch origin integration") ||
					strings.Contains(f.read(t, f.makeRuns), "test-") {
					t.Fatalf("missing base: exit %d, output %q, make %q", code, out, f.read(t, f.makeRuns))
				}
				return
			}
			if code != 0 {
				t.Fatalf("exit %d: %s", code, out)
			}
			f.checkGoPush(t, f.commitOld, out)
		})
	}
}

func TestPrePushValidatesTreeBeingChecked(t *testing.T) {
	for _, which := range []string{"non-HEAD", "tracked"} {
		t.Run(which, func(t *testing.T) {
			f := newPrePushFixture(t)
			line := "HEAD " + f.commitNew + " refs/heads/main " + f.commitOld + "\n"
			if which == "non-HEAD" {
				// A second ref must be validated even when the first matches HEAD.
				line += "refs/heads/old " + f.commitOld + " refs/heads/old " + f.commitOld + "\n"
			} else if err := os.WriteFile(filepath.Join(f.repo, "README.md"), []byte("dirty"), 0o644); err != nil {
				t.Fatal(err)
			}
			code, out := f.run(t, line)
			if code != 1 || !strings.Contains(out, strings.TrimPrefix(which, "non-")) || f.read(t, f.makeRuns) != "" {
				t.Fatalf("exit %d, output %q, make %q", code, out, f.read(t, f.makeRuns))
			}
		})
	}
}

func TestPrePushChangedCommandTests(t *testing.T) {
	f := newPrePushFixture(t)
	dir := filepath.Join(f.repo, "cmd", "gc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := "package main\nimport \"testing\"\nfunc TestAlpha(t *testing.T) {}\nfunc helper() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "alpha_test.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	f.git(t, "add", "cmd")
	f.git(t, "commit", "-qm", "add command test")
	head := f.gitOut(t, "rev-parse", "HEAD")
	code, out := f.run(t, "HEAD "+head+" refs/heads/main "+f.commitNew+"\n")
	calls := f.read(t, f.makeRuns)
	var packages []string
	for _, call := range strings.Split(calls, "\n") {
		if strings.Contains(call, "test-pkg") {
			packages = append(packages, call)
		}
	}
	want := "--no-print-directory test-pkg PKG=./cmd/gc RUN=^$ COUNT=1\n" +
		"--no-print-directory test-pkg PKG=./cmd/gc RUN=^(TestAlpha)$ COUNT=1"
	if code != 0 || strings.Join(packages, "\n") != want {
		t.Fatalf("exit %d: %s\nmake calls:\n%s", code, out, calls)
	}
}

// TestPrePushSkipsSuiteForBranchDeletion keeps the pre-existing carve-out
// intact through the stdin rework: a deletion has nothing to test, but beads
// must still see the ref line.
func TestPrePushSkipsSuiteForBranchDeletion(t *testing.T) {
	f := newPrePushFixture(t)
	zero := strings.Repeat("0", 40)
	refLine := "(delete) " + zero + " refs/heads/gone " + f.commitOld + "\n"

	code, out := f.run(t, refLine)
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0\n%s", code, out)
	}

	if got := f.read(t, f.bdStdin); got != refLine {
		t.Fatalf("beads received stdin %q, want %q", got, refLine)
	}
	if got := f.read(t, f.makeRuns); got != "" {
		t.Fatalf("push-time suite ran for a branch deletion: %q", got)
	}
}

// TestPrePushHandlesEmptyRefList guards the emptiness check around the replay:
// re-terminating an empty buffer would manufacture a blank ref line, which the
// scan would read as a real non-deletion push.
func TestPrePushHandlesEmptyRefList(t *testing.T) {
	f := newPrePushFixture(t)

	code, out := f.run(t, "")
	if code != 0 {
		t.Fatalf("pre-push exit = %d, want 0 for an empty ref list\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); got != "" {
		t.Fatalf("push-time suite ran with no refs pushed: %q", got)
	}
}

// TestPrePushPropagatesBeadsRejection keeps beads authoritative at push time:
// its rejection must abort before the suite runs.
func TestPrePushPropagatesBeadsRejection(t *testing.T) {
	f := newPrePushFixture(t)
	writeExecutable(t, filepath.Join(f.binDir, "bd"), `#!/usr/bin/env sh
cat > "$BD_STDIN_RECORD"
echo "beads rejected this push" >&2
exit 9
`)
	refLine := "refs/heads/main " + f.commitNew + " refs/heads/main " + f.commitOld + "\n"

	code, out := f.run(t, refLine)
	if code != 9 {
		t.Fatalf("pre-push exit = %d, want 9 (beads rejection must abort the push)\n%s", code, out)
	}
	if got := f.read(t, f.makeRuns); got != "" {
		t.Fatalf("push-time suite ran after a beads rejection: %q", got)
	}
}
