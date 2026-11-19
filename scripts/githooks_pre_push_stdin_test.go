package scripts_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
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
	// Git setup, including the first init, must use only fixture-owned state.
	f.env = slices.DeleteFunc(os.Environ(), func(entry string) bool { return strings.HasPrefix(entry, "GIT_") })
	f.env = append(f.env,
		"PATH="+binDir+":/usr/bin:/bin",
		"GIT_CONFIG_GLOBAL="+filepath.Join(recordDir, "gitconfig"),
		"GIT_CONFIG_SYSTEM="+filepath.Join(recordDir, "gitconfig-system"),
		"BD_STDIN_RECORD="+f.bdStdin,
		"MAKE_RECORD="+f.makeRuns,
		"GC_PREPUSH_BASE_REF=",
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
	for _, name := range []string{"test-push-gate-lock.sh", "test-local-concurrency.sh"} {
		writeExecutable(t, filepath.Join(repo, "scripts", name), "#!/bin/sh\nexit 93\n")
	}

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

func TestPrePushAnnotatedTagTarget(t *testing.T) {
	for _, target := range []string{"HEAD", "HEAD^"} {
		t.Run(target, func(t *testing.T) {
			f := newPrePushFixture(t)
			f.git(t, "tag", "-a", "release", "-m", "release", target)
			tag := f.gitOut(t, "rev-parse", "refs/tags/release")
			code, out := f.run(t, "refs/tags/release "+tag+" refs/tags/release "+f.commitOld+"\n")
			if target == "HEAD" {
				if code != 0 {
					t.Fatalf("annotated tag at HEAD: exit %d, %s", code, out)
				}
				f.checkGoPush(t, f.commitOld, out)
			} else if code != 1 || !strings.Contains(out, "HEAD") || f.read(t, f.makeRuns) != "" {
				t.Fatalf("annotated tag at older commit: exit %d, %s", code, out)
			}
		})
	}
}

func TestPrePushBuildTaggedPackages(t *testing.T) {
	for _, broken := range []bool{false, true} {
		name := "tagged alongside buildable package"
		if broken {
			name = "conflicting package declarations"
		}
		t.Run(name, func(t *testing.T) {
			f := newPrePushFixture(t)
			f.runRealLintRecipe(t)
			files := map[string]string{
				"go.mod":        "module example.org/fixture\n\ngo 1.23\n",
				"tagged/one.go": "//go:build integration\n\npackage tagged\n",
			}
			if broken {
				files["tagged/one.go"] = "package one\n"
				files["tagged/two.go"] = "package two\n"
			}
			for path, body := range files {
				dest := filepath.Join(f.repo, path)
				if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dest, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			f.git(t, "add", "-A")
			f.git(t, "commit", "-qm", "add packages")
			head := f.gitOut(t, "rev-parse", "HEAD")
			code, out := f.run(t, "HEAD "+head+" refs/heads/main "+f.commitOld+"\n")
			calls := f.read(t, f.makeRuns)
			if broken {
				if code == 0 || !strings.Contains(out, "found packages") || strings.Contains(calls, "test-pkg") {
					t.Fatalf("package error: exit %d, %s\n%s", code, out, calls)
				}
			} else if code != 0 || !strings.Contains(out, "./tagged left to CI (build-tagged)") ||
				!strings.HasSuffix(calls, "--no-print-directory test-pkg PKG=. COUNT=1\n") {
				t.Fatalf("build-tagged package: exit %d, %s\n%s", code, out, calls)
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

func (f *prePushFixture) write(t *testing.T, rel, body string) {
	t.Helper()
	path := filepath.Join(f.repo, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *prePushFixture) commit(t *testing.T, msg string) string {
	t.Helper()
	f.git(t, "add", "-A")
	f.git(t, "commit", "-qm", msg)
	return f.gitOut(t, "rev-parse", "HEAD")
}

// Exercise the real Go package loader in lint-changed and record the final
// package list. Other Make targets and the linter itself stay stubbed.
func (f *prePushFixture) runRealLintRecipe(t *testing.T) {
	t.Helper()
	f.env = append(f.env, "TOOL_PATH="+os.Getenv("PATH"), "REPO_MAKEFILE="+filepath.Join(repoRoot(t), "Makefile"))
	writeExecutable(t, filepath.Join(f.binDir, "make"), `#!/bin/sh
printf '%s\n' "$*" >> "$MAKE_RECORD"
if [ "$2" = lint-changed ]; then
    PATH="$TOOL_PATH" GOTOOLCHAIN=local exec make -f "$REPO_MAKEFILE" GOLANGCI_LINT=/usr/bin/true "$@"
fi
`)
}

func TestPrePushPackagesOnBothSides(t *testing.T) {
	for _, tc := range []struct{ change, want string }{
		{"delete file", "./src"},
		{"move file", "./dst ./src"},
		{"delete package", ""},
		{"only ignored Go file remains", ""},
	} {
		t.Run(tc.change, func(t *testing.T) {
			f := newPrePushFixture(t)
			f.write(t, "go.mod", "module example.org/fixture\n\ngo 1.23\n")
			f.write(t, "src/stays.go", "package fixture\n")
			f.write(t, "src/café.go", "package fixture\n")
			f.write(t, "dst/stays.go", "package fixture\n")
			base := f.commit(t, "add packages")
			switch tc.change {
			case "delete file":
				f.git(t, "rm", "src/café.go")
			case "move file":
				f.git(t, "mv", "src/café.go", "dst/café.go")
			case "delete package":
				f.git(t, "rm", "-r", "src")
			case "only ignored Go file remains":
				f.git(t, "rm", "-r", "src")
				f.write(t, "src/_example.go", "package fixture\n")
			}
			head := f.commit(t, tc.change)
			f.runRealLintRecipe(t)
			code, out := f.run(t, "HEAD "+head+" refs/heads/main "+base+"\n")
			calls := f.read(t, f.makeRuns)
			if code != 0 || !strings.Contains(calls, "--no-print-directory vet\n") {
				t.Fatalf("exit %d: %s\n%s", code, out, calls)
			}
			if tc.want == "" {
				if strings.Contains(calls, "test-pkg") {
					t.Errorf("passed a deleted package to test-pkg: %s", calls)
				}
			} else if !strings.Contains(calls, "test-pkg PKG="+tc.want+" COUNT=1\n") ||
				!strings.Contains(out, "lint-changed: "+tc.want) {
				t.Errorf("expected both lint and tests on %q: %s\n%s", tc.want, out, calls)
			}
		})
	}
}

func TestPrePushHarnessSelfTests(t *testing.T) {
	wf := readCriticalPathWorkflow(t, "ci.yml")
	const jobID = "local-harness-self-tests"
	job := wf.Jobs[jobID]
	if !slices.Contains(wf.Jobs["ci-required"].Needs, jobID) || job.ContinueOnError {
		t.Fatal("CI / required must depend on the harness self-tests")
	}
	for _, script := range []string{"test-push-gate-lock.sh", "test-local-concurrency.sh"} {
		if !slices.ContainsFunc(job.Steps, func(s ciCriticalPathStep) bool {
			return s.Run == "bash scripts/"+script && s.If == "" && !s.ContinueOnError
		}) {
			t.Errorf("missing required CI step for %s", script)
		}
	}
	f := newPrePushFixture(t)
	f.write(t, "scripts/push-gate-lock-lib.sh", "# harness change\n")
	head := f.commit(t, "change harness")
	if code, out := f.run(t, "HEAD "+head+" refs/heads/main "+f.commitNew+"\n"); code != 0 {
		t.Fatalf("script-only push ran a CI fixture: exit %d: %s", code, out)
	}
}

func TestPrePushFixtureIsolatesCallerIndex(t *testing.T) {
	index := filepath.Join(t.TempDir(), "caller-index")
	t.Setenv("GIT_INDEX_FILE", index)
	newPrePushFixture(t)
	if _, err := os.Stat(index); !os.IsNotExist(err) {
		t.Fatalf("caller index after fixture setup: want absent, got %v", err)
	}
}

func TestPrePushSelfTestEnvironment(t *testing.T) {
	f := newPrePushFixture(t)
	// Ownership checks need the same Git context as the push. The actual
	// test-pkg Make recipe must give its test process an independent scope.
	f.write(t, "scripts/push-ownership-guard.sh", `assert_bead_still_claimed() { test "$(git config probe.owner)" = caller; }`)
	f.write(t, "scripts/test-gitconfig-path", "#!/bin/sh\nprintf '/dev/null\\n'\n")
	f.write(t, "scripts/go-test-observable", `#!/bin/sh
set -eu
test -z "${GIT_DIR-}${GIT_WORK_TREE-}${GIT_INDEX_FILE-}${GIT_CONFIG_COUNT-}" || exit 31
test "$GIT_CONFIG_NOSYSTEM" = 1 && test "$GIT_CONFIG_GLOBAL" = /dev/null || exit 32
test ! -e "$HOME/caller-marker" || exit 33
test "$(go env CGO_ENABLED)" = 0 || exit 34
test "${GC_REAL_PROCESS_SIGNAL_TESTS-}" = 1 || exit 35
scratch=$(mktemp -d "$HOME/fixture.XXXXXX")
git -C "$scratch" init -q
git -C "$scratch" config user.name fixture
git -C "$scratch" config user.email fixture@example.invalid
git -C "$scratch" commit -q --allow-empty -m fixture
echo isolated-test-ran
`)
	for _, script := range []string{"test-gitconfig-path", "go-test-observable"} {
		if err := os.Chmod(filepath.Join(f.repo, "scripts", script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	callerHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(callerHome, "caller-marker"), []byte("caller\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.write(t, "go.mod", "module example.org/fixture\n\ngo 1.23\n")
	f.write(t, "touched.go", "package fixture\n// run tests\n")
	// Use the caller's default Go environment file under its private home.
	// Both paths must observe the persisted build profile.
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(goBin, filepath.Join(f.binDir, "go")); err != nil {
		t.Fatal(err)
	}
	f.env = append(f.env, "HOME="+callerHome, "GOENV=", "XDG_CONFIG_HOME=",
		"CGO_ENABLED=", "GOTOOLCHAIN=local")
	configure := testCommand(goBin, "env", "-w", "CGO_ENABLED=0")
	configure.Dir, configure.Env = f.repo, f.env
	if out, err := configure.CombinedOutput(); err != nil {
		t.Fatalf("persist fixture build profile: %v: %s", err, out)
	}
	head := f.commit(t, "exercise isolated test environment")
	f.env = append(f.env, "GIT_DIR="+filepath.Join(f.repo, ".git"), "GIT_WORK_TREE="+f.repo,
		"GIT_INDEX_FILE="+filepath.Join(f.repo, ".git", "index"),
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=probe.owner", "GIT_CONFIG_VALUE_0=caller",
		"TOOL_PATH="+os.Getenv("PATH"), "REPO_MAKEFILE="+filepath.Join(repoRoot(t), "Makefile"))
	writeExecutable(t, filepath.Join(f.binDir, "make"), `#!/bin/sh
if [ "$2" = test-pkg ]; then
    PATH="$TOOL_PATH" GOTOOLCHAIN=local exec make -f "$REPO_MAKEFILE" CHECK_ICU=true "$@"
fi
`)
	f.env = append(f.env, "EXTRA_TEST_ENV=GC_REAL_PROCESS_SIGNAL_TESTS=1")
	for _, goroot := range []string{"", filepath.Join(callerHome, "stale-go-root")} {
		t.Run("GOROOT="+filepath.Base(goroot), func(t *testing.T) {
			fixture := *f
			fixture.env = append(slices.Clone(f.env), "GOROOT="+goroot)
			code, out := fixture.run(t, "HEAD "+head+" refs/heads/main "+f.commitNew+"\n")
			if code != 0 || !strings.Contains(out, "isolated-test-ran") || f.gitOut(t, "config", "user.name") != "test" || f.gitOut(t, "rev-parse", "HEAD") != head {
				t.Fatalf("self-test environment: exit %d, %s", code, out)
			}
		})
	}
}

func TestPrePushSelfTestsRequireScratch(t *testing.T) {
	for _, script := range []string{"test-push-gate-lock.sh", "test-local-concurrency.sh"} {
		t.Run(script, func(t *testing.T) {
			root := repoRoot(t)
			path := filepath.Join(root, "scripts", script)
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// Exercise only initialization, ending before the cleanup trap or
			// any fixture workload. The parent of the scratch path is absent.
			setup, _, ok := strings.Cut(string(source), "\ntrap ")
			if !ok {
				t.Fatal("missing scratch cleanup boundary")
			}
			dir := t.TempDir()
			lib := "push-gate-lock-lib.sh"
			if err := os.Symlink(filepath.Join(root, "scripts", lib), filepath.Join(dir, lib)); err != nil {
				t.Fatal(err)
			}
			probe := filepath.Join(dir, script)
			writeExecutable(t, probe, setup+"\nprintf 'reached cleanup registration\\n'\n")
			cmd := testCommand("bash", probe)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "TMPDIR="+filepath.Join(cmd.Dir, "absent"))
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("scratch initialization succeeded without a directory: %s", out)
			}
		})
	}
}

func TestPrePushLocalEnvironmentDisablesHostSweep(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "scripts", "test-local-environment.sh")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Inspect the environment passed to the first runner. Its direct and Make
	// entry points must both opt out before any process inspection happens.
	setup, _, ok := strings.Cut(string(source), "\noutput=")
	if !ok {
		t.Fatal("missing runner invocation")
	}
	dir := t.TempDir()
	probe := filepath.Join(dir, "probe")
	writeExecutable(t, probe, setup+`
test "${GC_TEST_NO_ORPHAN_SWEEP-}" = 1
case " ${EXTRA_TEST_ENV-} " in *" GC_TEST_NO_ORPHAN_SWEEP=1 "*) ;; *) exit 1 ;; esac
`)
	cmd := testCommand("bash", probe)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GC_TEST_NO_ORPHAN_SWEEP=", "EXTRA_TEST_ENV=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("direct and Make runner environments must disable the host sweep: %v, %s", err, out)
	}
}

func TestPrePushLockFixtureCancellation(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, "scripts", "test-push-gate-lock.sh")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	setup, _, ok := strings.Cut(string(body), "\nSLOTS=")
	if !ok {
		t.Fatal("missing lock fixture setup")
	}
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(root, "scripts", "push-gate-lock-lib.sh"), filepath.Join(dir, "push-gate-lock-lib.sh")); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(dir, "probe")
	writeExecutable(t, probe, setup+`
mkfifo "$WORK/fifo"
exec 8<>"$WORK/fifo"
for n in 1 2; do
    bash -c 'echo $$ > "$1"; read -r -t 30 line <&8' _ "$RECORD/$n" &
done
wait
`)
	cmd := testCommand("bash", probe)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TMPDIR="+dir, "RECORD="+dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	})
	first := waitForPIDFile(t, filepath.Join(dir, "1"), 10*time.Second)
	second := waitForPIDFile(t, filepath.Join(dir, "2"), 10*time.Second)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("lock fixture did not exit after cancellation")
	}
	live := 0
	for _, pid := range []int{first, second} {
		if !processGone(pid, time.Second) {
			live++
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	}
	if live != 0 {
		t.Fatalf("%d fixture FIFO readers survived cancellation", live)
	}
}

func TestPrePushConcurrencyReportsDeniedTrace(t *testing.T) {
	root := repoRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "scripts", "test-local-concurrency.sh"))
	if err != nil {
		t.Fatal(err)
	}
	const start = "if command -v strace"
	_, after, ok := strings.Cut(string(body), start)
	block, _, end := strings.Cut(after, "\nfi\n")
	if !ok || !end {
		t.Fatal("missing trace probe")
	}
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "strace"), "#!/bin/sh\necho 'strace: ptrace: Operation not permitted' >&2\nexit 1\n")
	cmd := testCommand("bash", "-c", "record_pass() { exit 2; }; record_fail() { exit 3; }; "+start+block+"\nfi")
	cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin", "JOB_COUNT=/bin/true")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "skip loadavg.default_path_opens_proc_loadavg") || !strings.Contains(string(out), "Operation not permitted") {
		t.Fatalf("denied trace should be a named skip with reason: %v, %s", err, out)
	}
}

func TestPrePushCommandTestBatchSize(t *testing.T) {
	f := newPrePushFixture(t)
	var source strings.Builder
	source.WriteString("package main\nimport \"testing\"\n")
	var want []string
	for i := range 2200 {
		name := fmt.Sprintf("TestSelectedCommandWithLongName%04d", i)
		want = append(want, name)
		fmt.Fprintf(&source, "func %s(t *testing.T) {}\n", name)
	}
	f.write(t, "cmd/gc/selected_test.go", source.String())
	head := f.commit(t, "add command tests")
	code, out := f.run(t, "HEAD "+head+" refs/heads/main "+f.commitNew+"\n")
	if code != 0 {
		t.Fatalf("exit %d: %.500s", code, out)
	}
	var got []string
	for _, line := range strings.Split(f.read(t, f.makeRuns), "\n") {
		for _, arg := range strings.Fields(line) {
			if strings.HasPrefix(arg, "RUN=^(") {
				if len(arg) >= 64*1024 {
					t.Errorf("RUN argument has %d bytes", len(arg))
				}
				names := strings.TrimSuffix(strings.TrimPrefix(arg, "RUN=^("), ")$")
				got = append(got, strings.Split(names, "|")...)
			}
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("expected %d sorted names exactly once, got %d", len(want), len(got))
	}
}

func TestPrePushSelectionUsesPushedCommit(t *testing.T) {
	f := newPrePushFixture(t)
	// Move just the ref after the tracked-tree check. The index and worktree
	// still contain exactly the pushed commit, so its Go file must be tested.
	writeExecutable(t, filepath.Join(f.binDir, "git"), `#!/bin/sh
PATH=/usr/bin:/bin
if [ "$1 $2" = "diff --quiet" ]; then
    git "$@" && git update-ref HEAD "$MOVE_HEAD_TO"
else
    exec git "$@"
fi
`)
	f.env = append(f.env, "MOVE_HEAD_TO="+f.commitOld)
	code, out := f.run(t, "HEAD "+f.commitNew+" refs/heads/main "+f.commitOld+"\n")
	if code != 0 || !strings.Contains(f.read(t, f.makeRuns), "test-pkg PKG=. COUNT=1") {
		t.Fatalf("missing tests for pushed commit, exit %d: %s", code, out)
	}
}

func TestPrePushPinsMakeChecks(t *testing.T) {
	for _, flags := range []string{"", "n"} {
		t.Run("MAKEFLAGS="+flags, func(t *testing.T) {
			f := newPrePushFixture(t)
			makePath, err := exec.LookPath("make")
			if err != nil {
				t.Fatal(err)
			}
			record := filepath.Join(t.TempDir(), "selectors")
			f.env = append(f.env, "REAL_MAKE="+makePath, "SELECTORS="+record,
				"RUN=^$", "LINT_FLAGS=--fix", "MAKEFLAGS="+flags)
			// Probe the values GNU Make will actually use in its recipes.
			makefile := "RUN ?= .\n" +
				"lint-changed:\n\t@printf 'lint=%s\\n' '$(LINT_FLAGS)' >> \"$$SELECTORS\"\n" +
				"test-pkg:\n\t@printf 'run=%s\\n' '$(value RUN)' >> \"$$SELECTORS\"\n"
			if err := os.WriteFile(filepath.Join(f.repo, "Makefile"), []byte(makefile), 0o644); err != nil {
				t.Fatal(err)
			}
			writeExecutable(t, filepath.Join(f.binDir, "make"), `#!/bin/sh
case "$2" in lint-changed|test-pkg) exec "$REAL_MAKE" "$@";; esac
`)
			line := "HEAD " + f.commitNew + " refs/heads/main " + f.commitOld + "\n"
			code, out := f.run(t, line)
			if got := f.read(t, record); code != 0 || got != "lint=--allow-serial-runners\nrun=.\n" {
				t.Fatalf("exit %d, selectors %q, output %s", code, got, out)
			}
		})
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
