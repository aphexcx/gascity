package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMakefileLinuxCGOPathsIncludeExistingSystemLibLayouts(t *testing.T) {
	t.Setenv("CGO_CPPFLAGS", "-Iambient")
	t.Setenv("CGO_LDFLAGS", "-Lambient")

	repoRoot := repoRoot(t)
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	sysInclude := filepath.Join(tmp, "sysinclude")
	if err := os.MkdirAll(filepath.Join(sysInclude, "unicode"), 0o755); err != nil {
		t.Fatalf("mkdir unicode include: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sysInclude, "unicode", "uregex.h"), []byte(""), 0o644); err != nil {
		t.Fatalf("write ICU header: %v", err)
	}
	sysLib := filepath.Join(tmp, "syslib")
	sysLib64 := filepath.Join(tmp, "syslib64")
	for _, dir := range []string{filepath.Join(sysLib, "riscv64-linux-gnu"), sysLib, sysLib64} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	writeExecutable(t, filepath.Join(binDir, "uname"), "#!/usr/bin/env sh\necho Linux\n")
	writeExecutable(t, filepath.Join(binDir, "dpkg-architecture"), "#!/usr/bin/env sh\nexit 1\n")
	writeExecutable(t, filepath.Join(binDir, "cc"), `#!/usr/bin/env sh
case " $* " in
  *" -print-multiarch "*)
    echo riscv64-linux-gnu
    exit 0
    ;;
esac
printf '%s\n' '#include <...> search starts here:' ' /nix/store/include' 'End of search list.' >&2
`)

	out := runMakefileCGOPrintTarget(t, repoRoot, tmp, binDir,
		"SYS_USR_INCLUDE="+sysInclude,
		"SYS_USR_LIB_ROOT="+sysLib,
		"SYS_USR_LIB64_ROOT="+sysLib64,
		"CC=cc",
	)
	assertContains(t, out, "CGO_CPPFLAGS=-I"+sysInclude)
	assertContains(t, out, "-L"+filepath.Join(sysLib, "riscv64-linux-gnu"))
	assertContains(t, out, "-L"+sysLib64)
	assertContains(t, out, "-L"+sysLib)
	assertContains(t, out, "Linux system CGO fallback active:")
	if strings.Contains(out, "x86_64-linux-gnu") {
		t.Fatalf("CGO paths should not hardcode the x86_64 Debian fallback:\n%s", out)
	}
	if strings.Contains(out, "ambient") {
		t.Fatalf("Makefile CGO test helper should filter ambient CGO flags:\n%s", out)
	}
}

func TestMakefileDarwinICUPrefix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		installed  []string
		pkgConfig  string
		explicit   string
		want       string
		withoutICU []string
		makeArgs   []string
		goEnv      string
		defaultCGO bool
		cgoInclude string
		cgoVars    []string
		realICU    bool
		aliasICU   bool
	}{
		{name: "skip advertised missing version", installed: []string{"icu4c@78"}, want: "icu4c@78"},
		{name: "newest installed version", installed: []string{"icu4c@9", "icu4c@78"}, want: "icu4c@78"},
		{name: "unversioned brew", installed: []string{"icu4c"}, want: "icu4c"},
		{name: "pkg config fallback", installed: []string{"pkg-icu"}, pkgConfig: "pkg-icu", want: "pkg-icu"},
		{name: "stale pkg config prefix", pkgConfig: "absent"},
		{name: "nothing installed"},
		{name: "pure Go package without ICU", withoutICU: []string{"test-pkg", "PKG=./internal/shellquote", "EXTRA_TEST_ENV=CGO_ENABLED=1"}},
		{name: "test environment disables CGO", withoutICU: []string{"test-pkg", "PKG=./internal/beads", "RUN=^TestMemStoreCreateUsesSerializableTimestamp$", "EXTRA_TEST_ENV=CGO_ENABLED=0"}},
		{name: "ICU dependency with CGO enabled", makeArgs: []string{"test-pkg", "PKG=./internal/beads", "RUN=^TestMemStoreCreateUsesSerializableTimestamp$", "EXTRA_TEST_ENV=CGO_ENABLED=1"}},
		{name: "vet ICU dependency", makeArgs: []string{"vet", "PKGS=github.com/dolthub/go-icu-regex/internal/icu"}},
		{name: "vet pure Go package", withoutICU: []string{"vet", "PKGS=./internal/shellquote"}},
		{name: "environment wins", installed: []string{"icu4c@78"}, explicit: "environment", want: "custom"},
		{name: "command line wins", installed: []string{"icu4c@78"}, explicit: "command line", want: "custom"},
		{name: "help without ICU", withoutICU: []string{"help"}},
		{name: "CGO disabled without ICU", withoutICU: []string{"print-cgo-env", "CGO_ENABLED=0"}},
		{name: "cross compilation without ICU", withoutICU: []string{"print-cgo-env", "GOOS=js", "GOARCH=wasm"}, defaultCGO: true},
		{name: "persisted CGO setting without ICU", withoutICU: []string{"print-cgo-env"}, goEnv: "CGO_ENABLED=0\n", defaultCGO: true},
		{name: "saved CGO off cannot bypass missing ICU", goEnv: "CGO_ENABLED=0\n"},
		{name: "manual CPP include", installed: []string{"manual"}, cgoInclude: "-I", withoutICU: []string{"print-cgo-env"}},
		{name: "manual separated CPP include", installed: []string{"manual"}, cgoInclude: "-I ", withoutICU: []string{"print-cgo-env"}},
		{name: "CXX-only include cannot supply C headers", installed: []string{"manual"}, cgoInclude: "-I", cgoVars: []string{"CGO_CXXFLAGS"}},
		{name: "C-only include cannot supply CXX headers", installed: []string{"manual"}, cgoInclude: "-I", cgoVars: []string{"CGO_CFLAGS"}},
		{name: "paired C and CXX includes", installed: []string{"manual"}, cgoInclude: "-I", cgoVars: []string{"CGO_CFLAGS", "CGO_CXXFLAGS"}, withoutICU: []string{"print-cgo-env"}},
		{name: "paired includes with trailing slash", installed: []string{"manual"}, cgoInclude: "-I", cgoVars: []string{"CGO_CFLAGS"}, goEnv: "CGO_CXXFLAGS=-I{root}/manual/include/\n", withoutICU: []string{"print-cgo-env"}},
		{name: "paired includes with symlink alias", installed: []string{"manual"}, aliasICU: true, cgoInclude: "-I", cgoVars: []string{"CGO_CFLAGS"}, goEnv: "CGO_CXXFLAGS=-I{root}/manual-alias/include\n", withoutICU: []string{"print-cgo-env"}},
		{name: "persisted CPP include", installed: []string{"manual"}, goEnv: "CGO_CPPFLAGS=-I{root}/manual/include\nCGO_LDFLAGS=-L{root}/manual/lib\n", withoutICU: []string{"print-cgo-env"}},
		{name: "persisted stale CPP include", goEnv: "CGO_CPPFLAGS=-I{root}/absent/include\n"},
		{name: "exported CPP wins over persisted stale include", installed: []string{"manual"}, goEnv: "CGO_CPPFLAGS=-I{root}/absent/include\n", cgoInclude: "-I", withoutICU: []string{"print-cgo-env"}},
		{name: "exported stale CPP overrides persisted valid include", installed: []string{"saved"}, goEnv: "CGO_CPPFLAGS=-I{root}/saved/include\n", cgoInclude: "-I"},
		{name: "persisted paired C and CXX includes", installed: []string{"manual"}, goEnv: "CGO_CFLAGS=-I{root}/manual/include\nCGO_CXXFLAGS=-I{root}/manual/include\n", withoutICU: []string{"print-cgo-env"}},
		{name: "persisted CXX-only include", installed: []string{"manual"}, goEnv: "CGO_CXXFLAGS=-I{root}/manual/include\n"},
		{name: "C and CXX require a common header directory", installed: []string{"manual", "saved"}, cgoInclude: "-I", cgoVars: []string{"CGO_CFLAGS"}, goEnv: "CGO_CXXFLAGS=-I{root}/saved/include\n"},
		{name: "persisted CPP real vet", realICU: true, installed: []string{"manual"}, goEnv: "CGO_CPPFLAGS=-I{root}/manual/include\nCGO_LDFLAGS=-L{root}/manual/lib\n", withoutICU: []string{"vet", "PKGS=github.com/dolthub/go-icu-regex/internal/icu"}},
		{name: "persisted CPP real beads build", realICU: true, installed: []string{"manual"}, goEnv: "CGO_CPPFLAGS=-I{root}/manual/include\nCGO_LDFLAGS=-L{root}/manual/lib\n", withoutICU: []string{"test-pkg", "PKG=./internal/beads", "RUN=^TestMemStore"}},
		{name: "paired C and CXX real beads build", realICU: true, installed: []string{"manual"}, cgoInclude: "-I", cgoVars: []string{"CGO_CFLAGS", "CGO_CXXFLAGS"}, withoutICU: []string{"test-pkg", "PKG=./internal/beads", "RUN=^TestMemStore"}},
		{name: "CPP real beads build", realICU: true, installed: []string{"manual"}, cgoInclude: "-I", withoutICU: []string{"test-pkg", "PKG=./internal/beads", "RUN=^TestMemStore"}},
		{name: "CXX-only real beads build rejected", realICU: true, installed: []string{"manual"}, cgoInclude: "-I", cgoVars: []string{"CGO_CXXFLAGS"}, makeArgs: []string{"test-pkg", "PKG=./internal/beads", "RUN=^TestMemStore"}},
		{name: "stale manual CGO include", cgoInclude: "-I"},
		{name: "native DoltLite without ICU", withoutICU: []string{"-n", "test-native-doltlite-beads"}},
		{name: "pack registry without ICU", withoutICU: []string{"-n", "test-pack-registry-live"}},
		{name: "docs without ICU", withoutICU: []string{"-n", "docs-dev"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var realPrefix string
			if tc.realICU {
				if runtime.GOOS != "darwin" {
					t.Skip("real Darwin ICU build requires macOS")
				}
				probe := testCommand("sh", filepath.Join(repoRoot(t), "scripts/lib/icu-prefix.sh"))
				out, err := probe.CombinedOutput()
				if err != nil {
					var exitErr *exec.ExitError
					if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && len(out) == 0 {
						t.Skip("real Darwin ICU build requires installed ICU")
					}
					t.Fatalf("discover installed ICU for real build: %v\n%s", err, out)
				}
				realPrefix = strings.TrimSpace(string(out))
				if realPrefix == "" {
					t.Fatal("ICU discovery succeeded without an installed prefix")
				}
			}
			tmp := t.TempDir()
			binDir := filepath.Join(tmp, "bin")
			if err := os.Mkdir(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, name := range tc.installed {
				if tc.realICU {
					if err := os.Symlink(realPrefix, filepath.Join(tmp, name)); err != nil {
						t.Fatal(err)
					}
					continue
				}
				header := filepath.Join(tmp, name, "include", "unicode", "regex.h")
				if err := os.MkdirAll(filepath.Dir(header), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(header, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			writeExecutable(t, filepath.Join(binDir, "uname"), "#!/bin/sh\necho Darwin\n")
			if tc.aliasICU {
				if err := os.Symlink(filepath.Join(tmp, "manual"), filepath.Join(tmp, "manual-alias")); err != nil {
					t.Fatal(err)
				}
			}
			writeExecutable(t, filepath.Join(binDir, "brew"), `#!/bin/sh
root=$(dirname "$(dirname "$0")")
case "$*" in
  'list --formula') printf '%s\n' icu4c@79 icu4c@9 icu4c@78 ;;
  '--prefix icu4c')
    if [ -f "$root/icu4c/include/unicode/regex.h" ]; then
      echo "$root/icu4c"
    else
      echo "$root/icu4c@79"
    fi ;;
  '--prefix icu4c@'*) echo "$root/$2" ;;
  *) exit 1 ;;
esac
`)
			writeExecutable(t, filepath.Join(binDir, "pkg-config"), "#!/bin/sh\n[ \"$*\" = '--variable=prefix icu-uc' ] || exit 1\nprintf '%s\\n' \"$TEST_ICU_PKG_PREFIX\"\n")
			args := []string{"-s", "--no-print-directory", "print-cgo-env"}
			if tc.makeArgs != nil {
				args = append([]string{"-s", "--no-print-directory"}, tc.makeArgs...)
			}
			if tc.withoutICU != nil {
				args = append([]string{"-s", "--no-print-directory"}, tc.withoutICU...)
			}
			if tc.explicit == "command line" {
				args = append(args, "ICU_PREFIX="+filepath.Join(tmp, tc.want))
			}
			cmd := makeCommand(args...)
			cmd.Dir = repoRoot(t)
			env := replaceScriptEnv(filteredMakefileCGOTestEnv(t), "PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			env = replaceScriptEnv(env, "TEST_ICU_PKG_PREFIX", filepath.Join(tmp, tc.pkgConfig))
			if tc.cgoInclude != "" {
				flagVars := tc.cgoVars
				if len(flagVars) == 0 {
					flagVars = []string{"CGO_CPPFLAGS"}
				}
				for _, flagVar := range flagVars {
					env = replaceScriptEnv(env, flagVar, "-O2 "+tc.cgoInclude+filepath.Join(tmp, "manual", "include"))
				}
				env = replaceScriptEnv(env, "CGO_LDFLAGS", "-L"+filepath.Join(tmp, "manual", "lib"))
			}
			if tc.defaultCGO {
				env = replaceScriptEnv(env, "CGO_ENABLED", "")
			}
			if tc.goEnv != "" {
				goEnvFile := filepath.Join(tmp, "goenv")
				if err := os.WriteFile(goEnvFile, []byte(strings.ReplaceAll(tc.goEnv, "{root}", tmp)), 0o600); err != nil {
					t.Fatal(err)
				}
				env = replaceScriptEnv(env, "GOENV", goEnvFile)
			}
			// An inherited prefix must not bypass the automatic resolver cases.
			for i := len(env) - 1; i >= 0; i-- {
				if strings.HasPrefix(env[i], "ICU_PREFIX=") {
					env = append(env[:i], env[i+1:]...)
				}
			}
			if tc.explicit != "" {
				env = append(env, "ICU_PREFIX="+filepath.Join(tmp, "custom-env"))
				if tc.explicit == "environment" {
					env = replaceScriptEnv(env, "ICU_PREFIX", filepath.Join(tmp, tc.want))
				}
			}
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if tc.withoutICU != nil {
				if err != nil {
					t.Fatalf("target should not need ICU: %v\n%s", err, out)
				}
				assertNotContains(t, string(out), "ICU headers not found")
				return
			}
			if tc.want == "" {
				if err == nil {
					t.Fatalf("make succeeded without ICU:\n%s", out)
				}
				assertContains(t, string(out), "ICU_PREFIX=")
				assertContains(t, string(out), "brew install")
				assertContains(t, string(out), "CGO_CPPFLAGS=")
				return
			}
			if err != nil {
				t.Fatalf("make print-cgo-env: %v\n%s", err, out)
			}
			prefix := filepath.Join(tmp, tc.want)
			assertContains(t, string(out), "CGO_CPPFLAGS=-I"+prefix+"/include")
			assertContains(t, string(out), "CGO_LDFLAGS=-L"+prefix+"/lib")
			assertContains(t, string(out), "ICU_PREFIX="+prefix)
			assertContains(t, string(out), "ICU_PREFIX_SOURCE=")
		})
	}
}

func TestMakefileDarwinICURealBuildSkipsUnavailable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("real Darwin ICU discovery requires macOS")
	}
	binDir := t.TempDir()
	for _, name := range []string{"brew", "pkg-config"} {
		writeExecutable(t, filepath.Join(binDir, name), "#!/bin/sh\nexit 1\n")
	}
	cmd := testCommand(os.Args[0], "-test.run=^TestMakefileDarwinICUPrefix$/real", "-test.v")
	cmd.Env = replaceScriptEnv(filteredMakefileCGOTestEnv(t), "PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("unavailable optional ICU should skip real builds: %v\n%s", err, out)
	}
	if count := strings.Count(string(out), "--- SKIP: TestMakefileDarwinICUPrefix/"); count != 5 {
		t.Fatalf("expected all five real-build cases to skip, got %d:\n%s", count, out)
	}
}

func TestMakefileDarwinICURealBuildRejectsDiscoveryErrors(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("real Darwin ICU discovery requires macOS")
	}
	for _, tc := range []struct{ name, script string }{
		{"unexpected exit", "exit 2\n"},
		{"error output", "echo discovery-error >&2\nexit 1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binDir := t.TempDir()
			writeExecutable(t, filepath.Join(binDir, "sh"), "#!/bin/sh\n"+tc.script)
			cmd := testCommand(os.Args[0], "-test.run=^TestMakefileDarwinICUPrefix$/persisted_CPP_real_vet$", "-test.v")
			cmd.Env = replaceScriptEnv(filteredMakefileCGOTestEnv(t), "PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("unexpected discovery error was hidden:\n%s", out)
			}
			assertContains(t, string(out), "discover installed ICU for real build:")
			assertNotContains(t, string(out), "--- SKIP:")
		})
	}
}

// TestMakefileICUGateTargets proves that each caller checks its selected load
// before invoking the compiler or linter, without blocking unrelated packages.
func TestMakefileICUGateTargets(t *testing.T) {
	for _, target := range []string{"build", "print-cgo-env", "test", "test-pkg", "vet", "lint-full", "lint-new", "lint-changed", "lint-affected", "test-fast-parallel"} {
		for _, selection := range []string{"icu", "safe", "empty", "dry-run"} {
			if (target == "build" || target == "print-cgo-env" || target == "test" || target == "test-pkg") && selection != "dry-run" {
				continue
			}
			if target == "vet" && selection == "empty" {
				continue
			}
			if target == "test-fast-parallel" && (selection == "safe" || selection == "empty") {
				continue // The fast suite always selects the whole module.
			}
			t.Run(target+"/"+selection, func(t *testing.T) {
				files := map[string]string{
					"icu/icu.go":   "package icu\nimport \"C\"\n",
					"icu/icu.cpp":  "#include <unicode/regex.h>\n",
					"safe/safe.go": "package safe\n",
				}
				// Affected selection intentionally includes every native consumer
				// for any changed path, so a pure-Go closure needs a pure-Go module.
				if target == "lint-affected" && selection == "safe" {
					delete(files, "icu/icu.go")
					delete(files, "icu/icu.cpp")
				}
				fixture := newPRStaticScopeFixture(t, files)
				binDir := t.TempDir()
				writeExecutable(t, filepath.Join(binDir, "uname"), "#!/bin/sh\necho Darwin\n")
				writeExecutable(t, filepath.Join(binDir, "brew"), "#!/bin/sh\nexit 1\n")
				writeExecutable(t, filepath.Join(binDir, "pkg-config"), "#!/bin/sh\nexit 1\n")
				// Only the package-load boundary is real; unrelated parallel jobs
				// must not launch the repository's heavy suite from this fixture.
				for _, path := range []string{
					"scripts/test-local-parallel", "scripts/test-gitconfig-path",
					"scripts/push-gate-lock-lib.sh", "scripts/lib/go-toolchain.sh",
					"scripts/lib/test-slice.sh", "scripts/lib/inner-parallelism.sh",
					"scripts/lib/harness-reap.sh", "scripts/lib/check-icu.sh",
				} {
					content, err := os.ReadFile(filepath.Join(repoRoot(t), path))
					if err != nil {
						t.Fatal(err)
					}
					writeTestFile(t, filepath.Join(fixture.repoRoot, path), string(content))
					if err := os.Chmod(filepath.Join(fixture.repoRoot, path), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				for _, path := range []string{"scripts/test-go-test-shard", "scripts/test-push-gate-lock.sh", "scripts/test-local-concurrency.sh"} {
					writeExecutable(t, filepath.Join(fixture.repoRoot, path), "#!/bin/sh\nexit 0\n")
				}
				pkg := "./" + selection
				if selection == "dry-run" {
					pkg = "./icu"
				}
				if selection == "empty" {
					pkg = ""
				} else {
					file := strings.TrimPrefix(pkg, "./")
					writeTestFile(t, filepath.Join(fixture.repoRoot, file, file+".go"), "package "+file+"\n"+map[string]string{"icu": "import \"C\"\n"}[file]+"// changed\n")
				}
				args := []string{
					"-s", "--no-print-directory", "-f", fixture.productionMakefile, target,
					"GOLANGCI_LINT=" + fixture.fakeLint, "LINT_PKGS=" + pkg, "PKGS=" + pkg, "PKG=" + pkg,
					"LINT_CHANGED_SCOPE=tracked", "LINT_CHANGED_REF=HEAD", "LINT_FLAGS=",
					"LOCAL_TEST_JOBS=1", "CORE_SHARD_TOTAL=1", "CMD_GC_PROCESS_TOTAL=1",
				}
				if selection == "dry-run" {
					args = append(args, "-n")
				}
				cmd := makeCommand(args...)
				cmd.Dir = fixture.repoRoot
				cmd.Env = replaceScriptEnv(filteredMakefileCGOTestEnv(t), "PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
				for _, entry := range []string{
					"GC_PUSH_GATE_NO_CAP=1", "GC_TEST_NO_SLICE=1",
					"STATIC_SCOPE_LINT_LOG=" + fixture.lintLog,
					"LOCAL_TEST_LOG_DIR=" + filepath.Join(t.TempDir(), "logs"),
				} {
					key, value, _ := strings.Cut(entry, "=")
					cmd.Env = replaceScriptEnv(cmd.Env, key, value)
				}
				out, err := cmd.CombinedOutput()
				if selection == "icu" {
					if err == nil {
						t.Fatalf("%s reached its tool without ICU:\n%s", target, out)
					}
					assertContains(t, string(out), "ICU headers not found")
					assertContains(t, string(out), "ICU_PREFIX=")
					assertNotContains(t, string(out), "file not found")
					assertNotContains(t, string(out), "[unit-core-1-of-1] start")
					fixture.requireNoCalls(t)
				} else {
					if err != nil {
						t.Fatalf("%s/%s: %v\n%s", target, selection, err, out)
					}
					if selection == "dry-run" {
						assertContains(t, string(out), "check-icu.sh")
					}
					if selection == "empty" && target != "vet" {
						fixture.requireNoCalls(t)
					}
				}
			})
		}
	}
}

func TestMakefileCGOFixtureIgnoresSavedSettings(t *testing.T) {
	goEnvFile := filepath.Join(t.TempDir(), "goenv")
	if err := os.WriteFile(goEnvFile, []byte("CGO_ENABLED=0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOENV", goEnvFile)
	cmd := testCommand("go", "env", "CGO_ENABLED", "GOENV")
	cmd.Env = filteredMakefileCGOTestEnv(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "1\n\n" {
		t.Fatalf("fixture inherited saved CGO settings: %s", out)
	}
	for _, entry := range cmd.Env {
		if entry == "HOME="+os.Getenv("HOME") {
			t.Fatal("fixture inherited the developer's HOME")
		}
	}
}

func TestMakefileLinuxCGOPathsCanBeDisabled(t *testing.T) {
	repoRoot := repoRoot(t)
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	sysInclude := filepath.Join(tmp, "sysinclude")
	if err := os.MkdirAll(filepath.Join(sysInclude, "unicode"), 0o755); err != nil {
		t.Fatalf("mkdir unicode include: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sysInclude, "unicode", "uregex.h"), []byte(""), 0o644); err != nil {
		t.Fatalf("write ICU header: %v", err)
	}
	sysLib := filepath.Join(tmp, "syslib")
	if err := os.MkdirAll(sysLib, 0o755); err != nil {
		t.Fatalf("mkdir syslib: %v", err)
	}

	writeExecutable(t, filepath.Join(binDir, "uname"), "#!/usr/bin/env sh\necho Linux\n")
	writeExecutable(t, filepath.Join(binDir, "dpkg-architecture"), "#!/usr/bin/env sh\necho x86_64-linux-gnu\n")
	writeExecutable(t, filepath.Join(binDir, "cc"), `#!/usr/bin/env sh
case " $* " in
  *" -print-multiarch "*)
    echo x86_64-linux-gnu
    exit 0
    ;;
esac
printf '%s\n' '#include <...> search starts here:' ' /nix/store/include' 'End of search list.' >&2
`)

	out := runMakefileCGOPrintTarget(t, repoRoot, tmp, binDir,
		"SYS_USR_INCLUDE="+sysInclude,
		"SYS_USR_LIB_ROOT="+sysLib,
		"SYS_USR_LIB64_ROOT="+filepath.Join(tmp, "syslib64"),
		"SYS_USR_CGO_FALLBACK=0",
		"CC=cc",
	)
	if out != "CGO_CPPFLAGS=\nCGO_LDFLAGS=\n" {
		t.Fatalf("CGO flags should stay empty when the Linux system fallback is disabled:\n%s", out)
	}
	assertNotContains(t, out, "Linux system CGO fallback active:")
}

func TestMakefileLinuxCGOPathsStayEmptyWhenCompilerSeesSystemRoot(t *testing.T) {
	repoRoot := repoRoot(t)
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	sysInclude := filepath.Join(tmp, "sysinclude")
	if err := os.MkdirAll(filepath.Join(sysInclude, "unicode"), 0o755); err != nil {
		t.Fatalf("mkdir unicode include: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sysInclude, "unicode", "uregex.h"), []byte(""), 0o644); err != nil {
		t.Fatalf("write ICU header: %v", err)
	}
	sysLib := filepath.Join(tmp, "syslib")
	if err := os.MkdirAll(sysLib, 0o755); err != nil {
		t.Fatalf("mkdir syslib: %v", err)
	}

	writeExecutable(t, filepath.Join(binDir, "uname"), "#!/usr/bin/env sh\necho Linux\n")
	writeExecutable(t, filepath.Join(binDir, "dpkg-architecture"), `#!/usr/bin/env sh
touch "$(dirname "$0")/dpkg.ran"
echo x86_64-linux-gnu
`)
	writeExecutable(t, filepath.Join(binDir, "cc"), `#!/usr/bin/env sh
case " $* " in
  *" -print-multiarch "*)
    touch "$(dirname "$0")/cc-multiarch.ran"
    echo x86_64-linux-gnu
    exit 0
    ;;
esac
printf '%s\n' '#include <...> search starts here:' ' `+sysInclude+`' 'End of search list.' >&2
`)

	out := runMakefileCGOPrintTarget(t, repoRoot, tmp, binDir,
		"SYS_USR_INCLUDE="+sysInclude,
		"SYS_USR_LIB_ROOT="+sysLib,
		"SYS_USR_LIB64_ROOT="+filepath.Join(tmp, "syslib64"),
		"CC=cc",
	)
	if out != "CGO_CPPFLAGS=\nCGO_LDFLAGS=\n" {
		t.Fatalf("CGO flags should stay empty when the compiler already sees the system include root:\n%s", out)
	}
	for _, name := range []string{"dpkg.ran", "cc-multiarch.ran"} {
		if _, err := os.Stat(filepath.Join(binDir, name)); err == nil {
			t.Fatalf("Makefile should not run %s when the compiler already sees the system include root", strings.TrimSuffix(name, ".ran"))
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat %s: %v", name, err)
		}
	}
	assertNotContains(t, out, "Linux system CGO fallback active:")
}

func TestMakefileLinuxCGOPathsDeduplicateSystemLibDirs(t *testing.T) {
	repoRoot := repoRoot(t)
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	sysInclude := filepath.Join(tmp, "sysinclude")
	if err := os.MkdirAll(filepath.Join(sysInclude, "unicode"), 0o755); err != nil {
		t.Fatalf("mkdir unicode include: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sysInclude, "unicode", "uregex.h"), []byte(""), 0o644); err != nil {
		t.Fatalf("write ICU header: %v", err)
	}
	sysLib := filepath.Join(tmp, "syslib")
	multiarchLib := filepath.Join(sysLib, "x86_64-linux-gnu")
	for _, dir := range []string{multiarchLib, sysLib} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	writeExecutable(t, filepath.Join(binDir, "uname"), "#!/usr/bin/env sh\necho Linux\n")
	writeExecutable(t, filepath.Join(binDir, "dpkg-architecture"), "#!/usr/bin/env sh\necho x86_64-linux-gnu\n")
	writeExecutable(t, filepath.Join(binDir, "cc"), `#!/usr/bin/env sh
case " $* " in
  *" -print-multiarch "*)
    echo x86_64-linux-gnu
    exit 0
    ;;
esac
printf '%s\n' '#include <...> search starts here:' ' /nix/store/include' 'End of search list.' >&2
`)

	out := runMakefileCGOPrintTarget(t, repoRoot, tmp, binDir,
		"SYS_USR_INCLUDE="+sysInclude,
		"SYS_USR_LIB_ROOT="+sysLib,
		"SYS_USR_LIB64_ROOT="+sysLib,
		"CC=cc",
	)
	ldflags := strings.TrimPrefix(lineWithPrefix(t, out, "CGO_LDFLAGS="), "CGO_LDFLAGS=")
	if count := countExactField(ldflags, "-L"+multiarchLib); count != 1 {
		t.Fatalf("multiarch lib dir appears %d times, want 1:\n%s", count, out)
	}
	if count := countExactField(ldflags, "-L"+sysLib); count != 1 {
		t.Fatalf("system lib dir appears %d times, want 1:\n%s", count, out)
	}
	assertFieldsInOrder(t, ldflags, "-L"+multiarchLib, "-L"+sysLib)
}

func TestMakefileLinuxCGOPathsTreatNestedIncludeAsMissingSystemRoot(t *testing.T) {
	repoRoot := repoRoot(t)
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	sysInclude := filepath.Join(tmp, "sysinclude")
	if err := os.MkdirAll(filepath.Join(sysInclude, "unicode"), 0o755); err != nil {
		t.Fatalf("mkdir unicode include: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sysInclude, "unicode", "uregex.h"), []byte(""), 0o644); err != nil {
		t.Fatalf("write ICU header: %v", err)
	}
	sysLib := filepath.Join(tmp, "syslib")
	if err := os.MkdirAll(sysLib, 0o755); err != nil {
		t.Fatalf("mkdir syslib: %v", err)
	}

	writeExecutable(t, filepath.Join(binDir, "uname"), "#!/usr/bin/env sh\necho Linux\n")
	writeExecutable(t, filepath.Join(binDir, "dpkg-architecture"), "#!/usr/bin/env sh\nexit 1\n")
	writeExecutable(t, filepath.Join(binDir, "cc"), `#!/usr/bin/env sh
case " $* " in
  *" -print-multiarch "*)
    exit 0
    ;;
esac
printf '%s\n' '#include <...> search starts here:' ' `+sysInclude+`/x86_64-linux-gnu' 'End of search list.' >&2
`)

	out := runMakefileCGOPrintTarget(t, repoRoot, tmp, binDir,
		"SYS_USR_INCLUDE="+sysInclude,
		"SYS_USR_LIB_ROOT="+sysLib,
		"SYS_USR_LIB64_ROOT="+filepath.Join(tmp, "syslib64"),
		"CC=cc",
	)
	assertContains(t, out, "CGO_CPPFLAGS=-I"+sysInclude)
	assertContains(t, out, "-L"+sysLib)
}

func TestMakefileLinuxCGOPathsSkipDetectionWhenICUHeaderMissing(t *testing.T) {
	repoRoot := repoRoot(t)
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	sysInclude := filepath.Join(tmp, "sysinclude")
	if err := os.MkdirAll(sysInclude, 0o755); err != nil {
		t.Fatalf("mkdir sysinclude: %v", err)
	}
	writeExecutable(t, filepath.Join(binDir, "uname"), "#!/usr/bin/env sh\necho Linux\n")
	writeExecutable(t, filepath.Join(binDir, "dpkg-architecture"), `#!/usr/bin/env sh
touch "$(dirname "$0")/dpkg.ran"
echo x86_64-linux-gnu
`)
	writeExecutable(t, filepath.Join(binDir, "cc"), `#!/usr/bin/env sh
touch "$(dirname "$0")/cc.ran"
exit 0
`)

	out := runMakefileCGOPrintTarget(t, repoRoot, tmp, binDir,
		"SYS_USR_INCLUDE="+sysInclude,
		"SYS_USR_LIB_ROOT="+filepath.Join(tmp, "syslib"),
		"SYS_USR_LIB64_ROOT="+filepath.Join(tmp, "syslib64"),
		"CC=cc",
	)
	if out != "CGO_CPPFLAGS=\nCGO_LDFLAGS=\n" {
		t.Fatalf("CGO flags should stay empty when the ICU header is absent:\n%s", out)
	}
	for _, name := range []string{"dpkg.ran", "cc.ran"} {
		if _, err := os.Stat(filepath.Join(binDir, name)); err == nil {
			t.Fatalf("Makefile should not run %s when the ICU header is absent", strings.TrimSuffix(name, ".ran"))
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat %s: %v", name, err)
		}
	}
}

func runMakefileCGOPrintTarget(t *testing.T, repoRoot, tmp, binDir string, args ...string) string {
	t.Helper()
	makefile, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	testMakefile := filepath.Join(tmp, "Makefile")
	makefileContent := string(makefile) + `
.PHONY: print-cgo-flags
print-cgo-flags:
	@printf 'CGO_CPPFLAGS=%s\n' '$(CGO_CPPFLAGS)'
	@printf 'CGO_LDFLAGS=%s\n' '$(CGO_LDFLAGS)'
`
	if err := os.WriteFile(testMakefile, []byte(makefileContent), 0o644); err != nil {
		t.Fatalf("write test Makefile: %v", err)
	}

	// Write a fake gc stub so the Nix/Flox ICU detection block (which runs
	// `ldd $(command -v gc)`) finds a shell script rather than the real binary.
	// ldd on a shell script emits nothing useful, so _NIX_ICU_RT resolves to ""
	// and the Nix block stays inert — letting the SYS_USR_CGO_FALLBACK logic
	// under test run unobstructed.
	writeExecutable(t, filepath.Join(binDir, "gc"), "#!/bin/sh\n")

	cmdArgs := append([]string{"--no-print-directory", "-f", testMakefile, "print-cgo-flags"}, args...)
	cmd := makeCommand(cmdArgs...)
	cmd.Dir = repoRoot
	cmd.Env = append(filteredMakefileCGOTestEnv(t),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+filepath.Join(tmp, "home"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make print-cgo-flags failed: %v\n%s", err, out)
	}
	return string(out)
}

func makeCommand(args ...string) *exec.Cmd {
	return testCommand("make", args...)
}

func testCommand(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}

func filteredMakefileCGOTestEnv(t *testing.T) []string {
	t.Helper()
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, "ICU_PREFIX=") || strings.HasPrefix(entry, "CGO_") || strings.HasPrefix(entry, "GOENV=") || strings.HasPrefix(entry, "HOME=") || strings.HasPrefix(entry, "OBSERVABLE_") {
			continue
		}
		filtered = append(filtered, entry)
	}
	// Isolate persistent Go settings and state the intended CGO mode. Keep
	// toolchain and cache paths on the host so actual package regressions
	// reuse dependencies instead of downloading them into the temp home.
	for _, key := range []string{"GOPATH", "GOMODCACHE", "GOCACHE"} {
		filtered = replaceScriptEnv(filtered, key, goEnvValue(t, key))
	}
	return append(filtered,
		"GOTOOLCHAIN=local",
		"HOME="+t.TempDir(),
		"GOENV=off",
		"CGO_ENABLED=1",
	)
}

func assertContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("output missing %q:\n%s", needle, haystack)
	}
}

func assertNotContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("output contains %q:\n%s", needle, haystack)
	}
}

func lineWithPrefix(t *testing.T, output, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("output missing line prefix %q:\n%s", prefix, output)
	return ""
}

func countExactField(fields, want string) int {
	count := 0
	for _, field := range strings.Fields(fields) {
		if field == want {
			count++
		}
	}
	return count
}

func assertFieldsInOrder(t *testing.T, fields string, want ...string) {
	t.Helper()
	allFields := strings.Fields(fields)
	next := 0
	for _, field := range allFields {
		if field == want[next] {
			next++
			if next == len(want) {
				return
			}
		}
	}
	t.Fatalf("fields are not in required order %v:\n%s", want, fields)
}
