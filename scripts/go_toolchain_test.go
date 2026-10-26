package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The resolver must select the module toolchain even when the host driver and
// inherited GOROOT disagree, and leave child commands with the matching PATH.
func TestGoToolchainResolvesHostAndStaleRoot(t *testing.T) {
	root := repoRoot(t)
	fixture := t.TempDir()
	module := filepath.Join(fixture, "module")
	writeTestFile(t, filepath.Join(module, "go.mod"), "module example.com/toolchain\n\ngo 1.26.6\n")
	toolRoot := filepath.Join(fixture, "selected toolchain")
	bin := filepath.Join(fixture, "host")
	for _, dir := range []string{bin, filepath.Join(toolRoot, "bin")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, filepath.Join(bin, "go"), `#!/bin/sh
test -z "${GOROOT-}" || exit 41
test "$GOTOOLCHAIN" = go1.26.6 || exit 42
test "$*" = 'env GOROOT' || exit 43
printf '%s\n' `+shellQuote(toolRoot)+`
`)
	writeExecutable(t, filepath.Join(toolRoot, "bin", "go"), "#!/bin/sh\nprintf 'selected\\n'\n")
	for _, selection := range []string{"", "auto", "go1.26.6"} {
		t.Run("selection="+selection, func(t *testing.T) {
			cmd := testCommand("sh", "-eu", "-c", `. "$1"; gc_go_toolchain "$2"; printf '%s\n' "$GOTOOLCHAIN" "$GOROOT"; go version`, "probe",
				filepath.Join(root, "scripts", "lib", "go-toolchain.sh"), module)
			cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "GOROOT=/stale/root", "GOTOOLCHAIN=" + selection}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("resolve: %v\n%s", err, out)
			}
			if want := "go1.26.6\n" + toolRoot + "\nselected\n"; string(out) != want {
				t.Fatalf("resolved environment = %q, want %q", out, want)
			}
		})
	}
}

func TestGoToolchainFailureStopsCaller(t *testing.T) {
	root := repoRoot(t)
	fixture := t.TempDir()
	writeTestFile(t, filepath.Join(fixture, "go.mod"), "module example.com/toolchain\n\ngo 1.26.6\n")
	writeExecutable(t, filepath.Join(fixture, "go"), "#!/bin/sh\necho 'toolchain unavailable' >&2\nexit 23\n")
	cmd := testCommand("sh", "-eu", "-c", `. "$1"; gc_go_toolchain "$2"; echo MUST_NOT_RUN`, "probe",
		filepath.Join(root, "scripts", "lib", "go-toolchain.sh"), fixture)
	cmd.Env = []string{"PATH=" + fixture + ":" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "toolchain unavailable") || strings.Contains(string(out), "MUST_NOT_RUN") {
		t.Fatalf("resolution failure must stop caller: err=%v output=%s", err, out)
	}
}

func TestMakefileTestEnvKeepsResolvedToolchain(t *testing.T) {
	root := repoRoot(t)
	probe := filepath.Join(t.TempDir(), "probe.mk")
	writeTestFile(t, probe, `probe-toolchain:
	@$(TEST_ENV) sh -ec 'test "$$(command -v go)" = "$$GOROOT/bin/go"; test "$$(go env GOVERSION)" = "$$GOTOOLCHAIN"; test "$$(go env GOTOOLDIR)" = "$$GOROOT/pkg/tool/$$(go env GOHOSTOS)_$$(go env GOHOSTARCH)"; go version'
`)
	cmd := makeCommand("--no-print-directory", "-f", "Makefile", "-f", probe, "probe-toolchain")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=auto", "GOROOT=/stale/root")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("TEST_ENV must preserve the resolved driver, root and compile tools: %v\n%s", err, out)
	}
}
