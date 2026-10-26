package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGolangCILintMatchesVersionAndCompiler(t *testing.T) {
	const current = "lint: go1.26.6\n\tmod\tgithub.com/golangci/golangci-lint/v2\tv2.12.0\n"
	for _, tc := range []struct {
		name, before string
		failInstall  bool
		badBuild     bool
		wantInstall  bool
	}{
		{name: "missing", wantInstall: true},
		{name: "matching", before: current},
		{name: "older compiler", before: strings.ReplaceAll(current, "go1.26.6", "go1.25.0"), wantInstall: true},
		{name: "older linter", before: strings.ReplaceAll(current, "v2.12.0", "v2.11.0"), wantInstall: true},
		{name: "failed rebuild preserves binary", before: "old binary", failInstall: true, wantInstall: true},
		{name: "mismatched rebuild preserves binary", before: "old binary", badBuild: true, wantInstall: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := t.TempDir()
			bin := filepath.Join(fixture, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			lint := filepath.Join(fixture, "tools", "golangci-lint")
			if tc.before != "" {
				writeTestFile(t, lint, tc.before)
				if err := os.Chmod(lint, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			installLog := filepath.Join(fixture, "install.log")
			writeExecutable(t, filepath.Join(bin, "go"), `#!/bin/sh
set -eu
case "$1" in
  env) test "$2" = GOVERSION; echo go1.26.6 ;;
  version) test "$2" = -m; cat "$3" ;;
  install)
    test "$2" = github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.0
    echo install >> `+shellQuote(installLog)+`
    test "${FAIL_INSTALL:-0}" != 1 || exit 23
    printf '%s' `+shellQuote(current)+` > "$GOBIN/golangci-lint"
    if [ "${BAD_BUILD:-0}" = 1 ]; then
      printf 'wrong compiler\n' > "$GOBIN/golangci-lint"
    fi
    chmod +x "$GOBIN/golangci-lint"
    ;;
  *) exit 99 ;;
esac
`)
			cmd := testCommand("sh", filepath.Join(repoRoot(t), "scripts", "ensure-golangci-lint"), lint, "2.12.0")
			cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH")}
			if tc.failInstall {
				cmd.Env = append(cmd.Env, "FAIL_INSTALL=1")
			}
			if tc.badBuild {
				cmd.Env = append(cmd.Env, "BAD_BUILD=1")
			}
			out, err := cmd.CombinedOutput()
			if (err != nil) != (tc.failInstall || tc.badBuild) {
				t.Fatalf("ensure linter: %v\n%s", err, out)
			}
			installed, _ := os.ReadFile(installLog)
			if (len(installed) != 0) != tc.wantInstall {
				t.Fatalf("install calls = %q, want install=%v", installed, tc.wantInstall)
			}
			got, err := os.ReadFile(lint)
			if err != nil {
				t.Fatal(err)
			}
			want := current
			if tc.failInstall || tc.badBuild {
				want = tc.before
			}
			if string(got) != want {
				t.Fatalf("published linter = %q, want %q", got, want)
			}
		})
	}
}
