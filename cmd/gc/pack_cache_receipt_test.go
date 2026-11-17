package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/clientcontext"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

func newPackCacheExecutionFixture(t *testing.T) packCacheFixture {
	t.Helper()
	f := newPackCacheFixture(t, 1)
	t.Setenv("GC_RIG", "fixture")
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	// Historical ownership reads the origin in addition to the validation calls.
	wrapper := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in\n  *'config --get remote.origin.url') printf 'https://example.com/cache0.git\\n'; exit 0 ;;\n  *'status --porcelain') if test -f commands/report/dirty; then printf ' M commands/report/run.sh\\n'; exit 0; fi ;;\nesac\nexec %q \"$@\"\n", git)
	if err := os.WriteFile(filepath.Join(f.city, "git"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", f.city+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

func TestPackCacheBuiltinsRefuseUnresolvedCachedExecutables(t *testing.T) {
	for _, name := range []string{
		"runtime_check", "runtime_conformance", "runtime_conformance_env", "registry_credentials", "remote_grant", "agent_script",
		"remote:beads list", "remote:beads show fixture-1", "remote:session list",
		"remote:convoy list", "remote:convoy status fixture-1", "remote:mail peek fixture-1",
		"remote:mail inbox fixture", "remote:mail send fixture -m test", "remote:sling fixture fixture-1",
	} {
		t.Run(name, func(t *testing.T) {
			f := newPackCacheExecutionFixture(t)
			script := filepath.Join(f.caches[0], "commands", "report", "run.sh")
			if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			var args []string
			switch name {
			case "runtime_check":
				args = []string{"runtime", "check", script}
			case "runtime_conformance", "runtime_conformance_env":
				args = []string{"runtime", "conformance", script}
				if name == "runtime_conformance_env" {
					args = append(args, "--env")
				}
			case "remote_grant":
				t.Setenv("GC_NO_API", "")
				if code := doContextAdd(clientcontext.Context{Name: "fixture", URL: "https://example.invalid", City: "fixture", GrantCommand: script}, io.Discard, io.Discard); code != 0 {
					t.Fatal("create remote context")
				}
				args = []string{"--context", "fixture", "rig", "add", "--git-url", "https://example.invalid/repo.git", "--name", "fixture"}
			case "agent_script":
				t.Setenv("GC_CITY", "")
				t.Setenv("GC_RIG", "")
				t.Chdir(t.TempDir())
				yaml := filepath.Join(f.city, "script.yaml")
				if err := os.WriteFile(yaml, []byte(fmt.Sprintf("schema_version: v1\nsetup:\n  - shell: %q\n  - exit: 1\nturns:\n  - when: {hook: empty}\n    do: [{exit: 1}]\n", script)), 0o600); err != nil {
					t.Fatal(err)
				}
				args = []string{"agent-script", "--script", yaml}
			case "registry_credentials":
				t.Setenv(registryCredentialProviderEnv, fmt.Sprintf("[%q]", script))
				t.Setenv("GC_REGISTRY_TOKEN", "")
				args = []string{"pack", "registry", "whoami"}
			default:
				t.Setenv("GC_NO_API", "")
				if code := doContextAdd(clientcontext.Context{Name: "fixture", URL: "https://example.invalid", City: "fixture", CredentialCommand: script}, io.Discard, io.Discard); code != 0 {
					t.Fatal("create remote context")
				}
				args = append([]string{"--context", "fixture"}, strings.Fields(strings.TrimPrefix(name, "remote:"))...)
			}
			config.ResetRemoteCacheValidationCache()
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code == 0 {
				t.Errorf("dirty cached executable accepted: %s%s", &stdout, &stderr)
			}
			if _, err := os.Stat(f.marker); !os.IsNotExist(err) {
				t.Errorf("unchecked cached executable ran: marker stat=%v; %s%s", err, &stdout, &stderr)
			}
			// The credential protocol intentionally sanitizes subprocess errors;
			// absence of the execution marker proves refusal for that boundary.
			if output := stdout.String() + stderr.String(); name != "registry_credentials" && !strings.Contains(output, "cached executable") && !strings.Contains(output, "local worktree changes") {
				t.Errorf("missing cache refusal: %s", output)
			}
		})
	}
}

func TestPackCacheRuntimePathRetainsCleanExecution(t *testing.T) {
	for _, relative := range []bool{false, true} {
		t.Run(fmt.Sprintf("relative_%t", relative), func(t *testing.T) {
			f := newPackCacheExecutionFixture(t)
			script := filepath.Join(f.caches[0], "commands", "report", "run.sh")
			if relative {
				t.Chdir(f.caches[0])
				script = "./commands/report/run.sh"
			}
			var stdout, stderr bytes.Buffer
			_ = run([]string{"runtime", "check", script}, &stdout, &stderr)
			if _, err := os.Stat(f.marker); err != nil {
				t.Fatalf("clean cached executable did not run: %v; %s%s", err, &stdout, &stderr)
			}
			if !strings.Contains(f.gitCalls(t), "status --porcelain") {
				t.Fatal("clean executable ran without a full validation")
			}
		})
	}
}

func TestPackCacheInspectionDoesNotReuseSuccess(t *testing.T) {
	for _, args := range [][]string{{"import", "list"}, {"import", "why", "sample0"}, {"import", "status"}, {"doctor", "--check", "expanded-config-load"}} {
		for _, freshProcess := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/fresh_%t", strings.Join(args, "_"), freshProcess), func(t *testing.T) {
				f := newPackCacheExecutionFixture(t)
				t.Setenv("GC_DOLT", "skip")
				if _, _, err := loadStartCityConfig(f.city); err != nil {
					t.Fatal(err)
				}
				if scans := strings.Count(f.gitCalls(t), "status --porcelain"); scans != 1 {
					t.Fatalf("initial FULL scans=%d, want 1", scans)
				}
				nested := filepath.Join(f.caches[0], "commands", "report")
				if err := os.WriteFile(filepath.Join(nested, "run.sh"), []byte("#!/bin/sh\nprintf changed\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(nested, "dirty"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if freshProcess {
					config.ResetRemoteCacheValidationCache()
				}
				var stdout, stderr bytes.Buffer
				if code := run(args, &stdout, &stderr); code == 0 {
					t.Error("inspection reused success after a nested edit")
				}
				if !strings.Contains(stdout.String()+stderr.String(), `has local worktree changes; run "gc import install"`) {
					t.Errorf("inspection lost the cache diagnostic: %s%s", &stdout, &stderr)
				}
			})
		}
	}
}

func TestPackCacheReceiptServesFreshProcessFullLoad(t *testing.T) {
	f := newPackCacheFixture(t, 2)
	if _, _, err := loadStartCityConfig(f.city); err != nil {
		t.Fatal(err)
	}
	if scans := strings.Count(f.gitCalls(t), "status --porcelain"); scans != 2 {
		t.Fatalf("cold load scans=%d, want 2", scans)
	}
	config.ResetRemoteCacheValidationCache()
	if _, _, err := loadStartCityConfig(f.city); err != nil {
		t.Fatal(err)
	}
	if scans := strings.Count(f.gitCalls(t), "status --porcelain"); scans != 2 {
		t.Fatalf("fresh-process load repeated scans: %d", scans)
	}
	root := &cobra.Command{Use: "gc"}
	var stderr bytes.Buffer
	registerPackCommands(root, nil, io.Discard, &stderr)
	root.SetArgs([]string{"sample0", "report"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v; %s", err, stderr.String())
	}
	if scans := strings.Count(f.gitCalls(t), "status --porcelain"); scans != 2 {
		t.Fatalf("execution repeated full scan: %d", scans)
	}
	if _, err := os.Stat(f.marker); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(f.caches[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f.caches[0], info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	config.InvalidateRemoteCacheValidations()
	if _, _, err := loadStartCityConfig(f.city); err == nil || !strings.Contains(err.Error(), "local worktree changes") {
		t.Fatalf("reload accepted nested edit: %v", err)
	}
}
