package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/clientcontext"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

func TestPackCacheBuiltinsRefuseUnresolvedCachedExecutables(t *testing.T) {
	for _, name := range []string{
		"runtime_check", "runtime_conformance", "runtime_conformance_env", "registry_credentials", "remote_grant", "agent_script",
		"remote:beads list", "remote:beads show fixture-1", "remote:session list",
		"remote:convoy list", "remote:convoy status fixture-1", "remote:mail peek fixture-1",
		"remote:mail inbox fixture", "remote:mail send fixture -m test", "remote:sling fixture fixture-1",
	} {
		t.Run(name, func(t *testing.T) {
			f := newPackCacheFixture(t, 1)
			t.Setenv("GC_RIG", "fixture")
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
			if output := stdout.String() + stderr.String(); !strings.Contains(output, "cached executable") && !strings.Contains(output, "local worktree changes") {
				t.Errorf("missing cache refusal: %s", output)
			}
		})
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
