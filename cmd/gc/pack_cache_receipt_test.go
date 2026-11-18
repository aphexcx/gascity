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
	"time"

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

func newPinnedPackCacheExecutionFixture(t *testing.T, pin string) packCacheFixture {
	t.Helper()
	f := newPackCacheExecutionFixture(t)
	cache := filepath.Join(filepath.Dir(f.caches[0]), config.RepoCacheKey("https://example.com/cache0.git", pin))
	if cache != f.caches[0] {
		if err := os.Rename(f.caches[0], cache); err != nil {
			t.Fatal(err)
		}
		f.caches[0] = cache
	}
	lockPath := filepath.Join(f.city, "packs.lock")
	lock, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte(strings.ReplaceAll(string(lock), packCacheTestCommit, pin)), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func writePackCacheLocalScript(t *testing.T, path, marker string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf("#!/bin/sh\nprintf executed > %q\n", marker)), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestPackCacheRuntimeLocalPathSkipsCityLoad(t *testing.T) {
	for _, scenario := range []string{"lock_held", "dirty_import", "no_validation"} {
		for _, location := range []string{"outside_city", "inside_city"} {
			for _, command := range []struct {
				name string
				args []string
			}{
				{"check", []string{"runtime", "check"}},
				{"conformance", []string{"runtime", "conformance"}},
				{"conformance_env", []string{"runtime", "conformance", "--env"}},
			} {
				for _, city := range []string{"env", "flags"} {
					t.Run(strings.Join([]string{scenario, location, command.name, city}, "/"), func(t *testing.T) {
						f := newPackCacheExecutionFixture(t)
						dir := t.TempDir()
						if location == "inside_city" {
							dir = filepath.Join(f.city, "local")
						}
						script, marker := filepath.Join(dir, "rt.sh"), filepath.Join(dir, "executed")
						writePackCacheLocalScript(t, script, marker)
						var args []string
						if city == "flags" {
							t.Setenv("GC_CITY", "")
							t.Setenv("GC_RIG", "")
							args = []string{"--city", f.city, "--rig", "fixture"}
						}
						args = append(append(args, command.args...), script)
						if scenario == "dirty_import" {
							if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), nil, 0o600); err != nil {
								t.Fatal(err)
							}
						}
						var stdout, stderr bytes.Buffer
						if scenario == "lock_held" {
							release := holdExclusiveRepoCacheLock(t, filepath.Dir(f.caches[0]))
							done := make(chan struct{})
							go func() {
								defer close(done)
								_ = run(args, &stdout, &stderr)
							}()
							select {
							case <-done:
								// The lock is still held when execution is observed below.
							case <-time.After(5 * time.Second):
								// Release and join before fixture cleanup, even on failure.
								release()
								<-done
								t.Fatal("local executable check waited on the repository-cache lock")
							}
						} else {
							_ = run(args, &stdout, &stderr)
						}
						// The script proves execution, not RPP conformance or exit status.
						if _, err := os.Stat(marker); err != nil {
							t.Errorf("local executable did not run: %v; %s%s", err, &stdout, &stderr)
						}
						if scenario == "dirty_import" {
							for _, refusal := range []string{"cached executable", "local worktree changes", "historical cache"} {
								if strings.Contains(stdout.String()+stderr.String(), refusal) {
									t.Errorf("unrelated import refused local execution: %s%s", &stdout, &stderr)
								}
							}
						}
						if scenario == "no_validation" {
							if calls := f.gitCalls(t); calls != "" {
								t.Errorf("local executable check invoked Git:\n%s", calls)
							}
							if _, err := os.Stat(filepath.Join(f.caches[0], ".git", "gc-validation-v1.json")); !os.IsNotExist(err) {
								t.Errorf("local executable check wrote a cache receipt: stat=%v", err)
							}
						}
					})
				}
			}
		}
	}
}

func TestPackCacheRuntimePathBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		cached     bool
		modified   bool
	}{
		{"mimic", "mimic", false, false},
		{"symlink_clean", "symlink", true, false},
		{"symlink_modified", "symlink", true, true},
		{"dot_dot_inside_clean", "dot_dot_inside", true, false},
		{"dot_dot_inside_modified", "dot_dot_inside", true, true},
		// Ruling C: a visited cache root counts even if the path then leaves it.
		{"dot_dot_out", "dot_dot_out", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPinnedPackCacheExecutionFixture(t, packCacheTestCommit[:8])
			script := filepath.Join(f.caches[0], "commands", "report", "run.sh")
			switch tc.path {
			case "mimic":
				script = filepath.Join(t.TempDir(), filepath.Base(f.caches[0]), "commands", "report", "run.sh")
				writePackCacheLocalScript(t, script, f.marker)
			case "symlink":
				// Cache root candidates preserve spelling; use the canonical root
				// so this link tests the same boundary on macOS and Linux.
				gcHome, err := filepath.EvalSymlinks(os.Getenv("GC_HOME"))
				if err != nil {
					t.Fatal(err)
				}
				t.Setenv("GC_HOME", gcHome)
				link := filepath.Join(t.TempDir(), "rt.sh")
				if err := os.Symlink(script, link); err != nil {
					t.Fatal(err)
				}
				script = link
			case "dot_dot_inside":
				script = f.caches[0] + "/commands/../commands/report/run.sh"
			case "dot_dot_out":
				script = filepath.Dir(f.caches[0]) + "/../local.sh"
				writePackCacheLocalScript(t, script, f.marker)
			}
			if tc.modified {
				if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			_ = run([]string{"runtime", "check", script}, &stdout, &stderr)
			_, markerErr := os.Stat(f.marker)
			if tc.modified {
				if !os.IsNotExist(markerErr) || !strings.Contains(stdout.String()+stderr.String(), "local worktree changes") {
					t.Errorf("modified cached executable not refused: marker stat=%v; %s%s", markerErr, &stdout, &stderr)
				}
			} else if markerErr != nil {
				t.Errorf("clean executable did not run: %v; %s%s", markerErr, &stdout, &stderr)
			}
			if calls := f.gitCalls(t); tc.cached {
				if !strings.Contains(calls, "status --porcelain") {
					t.Errorf("cached executable checked without full validation: %s", calls)
				}
			} else if calls != "" {
				t.Errorf("local executable check invoked Git:\n%s", calls)
			}
		})
	}
}

func packCacheRuntimeSpelling(t *testing.T, f packCacheFixture, spelling string) string {
	t.Helper()
	script := filepath.Join(f.caches[0], "commands", "report", "run.sh")
	link := func(target string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		return path
	}
	switch spelling {
	case "case_home", "case_cache", "case_repos":
		component := map[string]string{"case_home": "gchome", "case_cache": "cache", "case_repos": "repos"}[spelling]
		variant := map[string]string{"case_home": "GCHOME", "case_cache": "Cache", "case_repos": "Repos"}[spelling]
		changed := strings.Replace(script, string(os.PathSeparator)+component+string(os.PathSeparator), string(os.PathSeparator)+variant+string(os.PathSeparator), 1)
		original, err := os.Stat(script)
		if err != nil {
			t.Fatal(err)
		}
		other, err := os.Stat(changed)
		if os.IsNotExist(err) || (err == nil && !os.SameFile(original, other)) {
			t.Skip("filesystem does not fold this component's letter case")
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Log("case-folding probe: same file")
		return changed
	case "outer_file_link":
		outside := filepath.Join(t.TempDir(), "run.sh")
		writePackCacheLocalScript(t, outside, f.marker)
		if err := os.Remove(script); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, script); err != nil {
			t.Fatal(err)
		}
		return link(script)
	case "outer_directory_link", "cached_directory_link":
		outside := t.TempDir()
		writePackCacheLocalScript(t, filepath.Join(outside, "run.sh"), f.marker)
		if err := os.RemoveAll(filepath.Dir(script)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Dir(script)); err != nil {
			t.Fatal(err)
		}
		if spelling == "outer_directory_link" {
			return link(script)
		}
		return script
	case "linked_home", "linked_home_outer":
		t.Setenv("GC_HOME", link(os.Getenv("GC_HOME")))
		canonical, err := filepath.EvalSymlinks(script)
		if err != nil {
			t.Fatal(err)
		}
		if spelling == "linked_home_outer" {
			return link(canonical)
		}
		return canonical
	case "outer_link":
		return link(script)
	default:
		t.Fatalf("unknown cache spelling %q", spelling)
		return ""
	}
}

func TestPackCacheRuntimeModifiedPathIdentity(t *testing.T) {
	for _, spelling := range []string{"case_home", "case_cache", "case_repos", "outer_file_link", "outer_directory_link", "linked_home", "outer_link", "cached_directory_link"} {
		for _, command := range []string{"check", "conformance", "conformance --env"} {
			for _, city := range []string{"env", "flags"} {
				t.Run(strings.Join([]string{spelling, command, city}, "/"), func(t *testing.T) {
					f := newPinnedPackCacheExecutionFixture(t, packCacheTestCommit[:8])
					script := packCacheRuntimeSpelling(t, f, spelling)
					if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), nil, 0o600); err != nil {
						t.Fatal(err)
					}
					var args []string
					if city == "flags" {
						t.Setenv("GC_CITY", "")
						t.Setenv("GC_RIG", "")
						args = []string{"--city", f.city, "--rig", "fixture"}
					}
					args = append(args, "runtime")
					args = append(args, strings.Fields(command)...)
					args = append(args, script)
					var stdout, stderr bytes.Buffer
					code := run(args, &stdout, &stderr)
					output := stdout.String() + stderr.String()
					if code == 0 {
						t.Errorf("modified executable accepted: %s", output)
					}
					if _, err := os.Stat(f.marker); !os.IsNotExist(err) {
						t.Errorf("modified executable ran: marker stat=%v; %s", err, output)
					}
					if !strings.Contains(output, "local worktree changes") && !strings.Contains(output, "cached executable") && !strings.Contains(output, "historical cache") {
						t.Errorf("missing cache refusal: %s", output)
					}
				})
			}
		}
	}
}

func TestPackCacheRuntimeCleanPathIdentity(t *testing.T) {
	for _, spelling := range []string{"linked_home", "linked_home_outer", "shadowed_name"} {
		for _, pin := range []struct{ name, commit string }{
			{"abbreviated_8", packCacheTestCommit[:8]},
			{"uppercase_full", strings.ToUpper(packCacheTestCommit)},
			{"mixed_case_full", strings.ReplaceAll(packCacheTestCommit, "abcdef", "aBcDeF")},
		} {
			if spelling == "shadowed_name" && pin.name == "mixed_case_full" {
				continue
			}
			for _, command := range []string{"check", "conformance", "conformance --env"} {
				for _, city := range []string{"env", "flags"} {
					t.Run(strings.Join([]string{spelling, pin.name, command, city}, "/"), func(t *testing.T) {
						f := newPinnedPackCacheExecutionFixture(t, pin.commit)
						var script, localMarker string
						if spelling == "shadowed_name" {
							cwd := t.TempDir()
							localMarker = filepath.Join(cwd, "local-executed")
							writePackCacheLocalScript(t, filepath.Join(cwd, "run.sh"), localMarker)
							t.Chdir(cwd)
							t.Setenv("PATH", filepath.Join(f.caches[0], "commands", "report")+string(os.PathListSeparator)+os.Getenv("PATH"))
							script = "run.sh"
						} else {
							script = packCacheRuntimeSpelling(t, f, spelling)
						}
						var args []string
						if city == "flags" {
							t.Setenv("GC_CITY", "")
							t.Setenv("GC_RIG", "")
							args = []string{"--city", f.city, "--rig", "fixture"}
						}
						args = append(args, "runtime")
						args = append(args, strings.Fields(command)...)
						args = append(args, script)
						var stdout, stderr bytes.Buffer
						_ = run(args, &stdout, &stderr)
						if _, err := os.Stat(f.marker); err != nil {
							t.Errorf("clean pinned executable did not run: %v; %s%s", err, &stdout, &stderr)
						}
						if !strings.Contains(f.gitCalls(t), "status --porcelain") {
							t.Error("clean executable ran without full validation")
						}
						if localMarker != "" {
							if _, err := os.Stat(localMarker); !os.IsNotExist(err) {
								t.Errorf("working-directory shadow ran: %v", err)
							}
						}
					})
				}
			}
		}
	}
}

func TestPackCacheRuntimeModifiedPhysicalParent(t *testing.T) {
	for _, spelling := range []string{"absolute_link", "absolute_link_decoy", "relative_link", "linked_cwd"} {
		for _, command := range []string{"check", "conformance", "conformance --env"} {
			for _, city := range []string{"env", "flags"} {
				t.Run(strings.Join([]string{spelling, command, city}, "/"), func(t *testing.T) {
					f := newPinnedPackCacheExecutionFixture(t, packCacheTestCommit[:8])
					script := filepath.Join(f.caches[0], "commands", "report", "run.sh")
					outside := t.TempDir()
					link := filepath.Join(outside, "link")
					if err := os.Symlink(filepath.Dir(script), link); err != nil {
						t.Fatal(err)
					}
					target := link + "/../report/run.sh"
					decoyMarker := filepath.Join(outside, "decoy-executed")
					switch spelling {
					case "absolute_link_decoy":
						writePackCacheLocalScript(t, filepath.Join(outside, "report", "run.sh"), decoyMarker)
					case "relative_link":
						t.Chdir(f.city)
						rel, err := filepath.Rel(f.city, outside)
						if err != nil {
							t.Fatal(err)
						}
						target = rel + "/link/../report/run.sh"
					case "linked_cwd":
						t.Chdir(link)
						target = "../report/run.sh"
					}
					original, err := os.Stat(script)
					if err != nil {
						t.Fatal(err)
					}
					other, err := os.Stat(target)
					if err != nil || !os.SameFile(original, other) {
						t.Fatalf("target %q does not reach cached script %q: %v", target, script, err)
					}
					if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), nil, 0o600); err != nil {
						t.Fatal(err)
					}
					var args []string
					if city == "flags" {
						t.Setenv("GC_CITY", "")
						t.Setenv("GC_RIG", "")
						args = []string{"--city", f.city, "--rig", "fixture"}
					}
					args = append(args, "runtime")
					args = append(args, strings.Fields(command)...)
					args = append(args, target)
					var stdout, stderr bytes.Buffer
					code := run(args, &stdout, &stderr)
					output := stdout.String() + stderr.String()
					if code == 0 {
						t.Errorf("modified executable accepted: %s", output)
					}
					if _, err := os.Stat(f.marker); !os.IsNotExist(err) {
						t.Errorf("modified executable ran: marker stat=%v; %s", err, output)
					}
					if _, err := os.Stat(decoyMarker); !os.IsNotExist(err) {
						t.Errorf("lexically cleaned decoy ran: marker stat=%v; %s", err, output)
					}
					if !strings.Contains(output, "local worktree changes") && !strings.Contains(output, "cached executable") && !strings.Contains(output, "historical cache") {
						t.Errorf("missing cache refusal: %s", output)
					}
				})
			}
		}
	}
}

func TestPackCacheRuntimeLocalPhysicalParent(t *testing.T) {
	for _, spelling := range []string{"local_directory", "local_link"} {
		for _, command := range []string{"check", "conformance", "conformance --env"} {
			for _, city := range []string{"env", "flags"} {
				t.Run(strings.Join([]string{spelling, command, city}, "/"), func(t *testing.T) {
					f := newPackCacheExecutionFixture(t)
					outside := t.TempDir()
					script, marker := filepath.Join(outside, "script.sh"), filepath.Join(outside, "executed")
					writePackCacheLocalScript(t, script, marker)
					dir := filepath.Join(outside, "sub")
					if err := os.Mkdir(dir, 0o755); err != nil {
						t.Fatal(err)
					}
					if spelling == "local_link" {
						link := filepath.Join(t.TempDir(), "link")
						if err := os.Symlink(dir, link); err != nil {
							t.Fatal(err)
						}
						dir = link
					}
					target := dir + "/../script.sh"
					var args []string
					if city == "flags" {
						t.Setenv("GC_CITY", "")
						t.Setenv("GC_RIG", "")
						args = []string{"--city", f.city, "--rig", "fixture"}
					}
					args = append(args, "runtime")
					args = append(args, strings.Fields(command)...)
					args = append(args, target)
					var stdout, stderr bytes.Buffer
					_ = run(args, &stdout, &stderr)
					if _, err := os.Stat(marker); err != nil {
						t.Errorf("local executable did not run: %v; %s%s", err, &stdout, &stderr)
					}
					if calls := f.gitCalls(t); calls != "" {
						t.Errorf("local executable check invoked Git:\n%s", calls)
					}
					if _, err := os.Stat(filepath.Join(f.caches[0], ".git", "gc-validation-v1.json")); !os.IsNotExist(err) {
						t.Errorf("local executable check wrote a cache receipt: stat=%v", err)
					}
				})
			}
		}
	}
}

func TestPackCacheRuntimePinInvocation(t *testing.T) {
	for _, state := range []string{"clean", "modified"} {
		for _, pin := range []struct{ name, commit string }{
			{"full_lowercase", packCacheTestCommit},
			{"abbreviated_7", packCacheTestCommit[:7]},
			{"abbreviated_8", packCacheTestCommit[:8]},
			{"abbreviated_12", packCacheTestCommit[:12]},
			{"uppercase_full", strings.ToUpper(packCacheTestCommit)},
			{"mixed_case_full", strings.ReplaceAll(packCacheTestCommit, "abcdef", "aBcDeF")},
		} {
			for _, command := range []struct {
				name string
				args []string
			}{
				{"check", []string{"runtime", "check"}},
				{"conformance", []string{"runtime", "conformance"}},
				{"conformance_env", []string{"runtime", "conformance", "--env"}},
			} {
				for _, target := range []string{"name", "path"} {
					for _, city := range []string{"env", "flags", "none"} {
						if target == "name" && city == "none" {
							continue // Runtime names need a city configuration.
						}
						t.Run(strings.Join([]string{state, pin.name, command.name, target, city}, "/"), func(t *testing.T) {
							f := newPinnedPackCacheExecutionFixture(t, pin.commit)
							packPath := filepath.Join(f.caches[0], "pack.toml")
							pack, err := os.ReadFile(packPath)
							if err != nil {
								t.Fatal(err)
							}
							pack = append(pack, []byte("\n[runtimes.sample]\ncommand = \"commands/report/run.sh\"\n")...)
							if err := os.WriteFile(packPath, pack, 0o644); err != nil {
								t.Fatal(err)
							}
							if state == "modified" {
								if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), nil, 0o600); err != nil {
									t.Fatal(err)
								}
							}
							var args []string
							if city != "env" {
								t.Setenv("GC_CITY", "")
								t.Setenv("GC_RIG", "")
								if city == "flags" {
									args = []string{"--city", f.city, "--rig", "fixture"}
								} else {
									t.Chdir(t.TempDir())
								}
							}
							args = append(args, command.args...)
							executable := "sample"
							if target == "path" {
								executable = filepath.Join(f.caches[0], "commands", "report", "run.sh")
							}
							args = append(args, executable)
							var stdout, stderr bytes.Buffer
							code := run(args, &stdout, &stderr)
							output := stdout.String() + stderr.String()
							_, markerErr := os.Stat(f.marker)
							citylessNoncanonical := city == "none" && pin.commit != packCacheTestCommit
							if state == "modified" || citylessNoncanonical {
								if code == 0 {
									t.Errorf("unchecked cached executable accepted: %s", output)
								}
								if !os.IsNotExist(markerErr) {
									t.Errorf("refused executable ran: marker stat=%v; %s", markerErr, output)
								}
								if !strings.Contains(output, "cached executable") && !strings.Contains(output, "local worktree changes") && !strings.Contains(output, "historical cache") {
									t.Errorf("missing cache refusal: %s", output)
								}
								if state == "clean" && citylessNoncanonical && !strings.Contains(output, "does not match its source and commit address") {
									t.Errorf("cityless refusal changed: %s", output)
								}
								return
							}
							// This fixture proves execution, not a complete RPP handshake.
							if markerErr != nil {
								t.Fatalf("clean pinned executable did not run: %v; %s", markerErr, output)
							}
							if !strings.Contains(f.gitCalls(t), "status --porcelain") {
								t.Fatal("clean pinned executable ran without a full validation")
							}
						})
					}
				}
			}
		}
	}
}

func TestPackCacheRuntimeExistingFilenameLoadsLock(t *testing.T) {
	f := newPinnedPackCacheExecutionFixture(t, packCacheTestCommit[:8])
	dir := filepath.Join(f.caches[0], "commands", "report")
	t.Chdir(dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	_ = run([]string{"runtime", "check", "run.sh"}, &stdout, &stderr)
	if _, err := os.Stat(f.marker); err != nil {
		t.Fatalf("clean pinned executable did not run: %v; %s%s", err, &stdout, &stderr)
	}
	if !strings.Contains(f.gitCalls(t), "status --porcelain") {
		t.Fatal("clean pinned executable ran without a full validation")
	}
}

func TestPackCacheRuntimePathInvalidCityPreservesHistoricalRecovery(t *testing.T) {
	for _, pin := range []string{packCacheTestCommit, packCacheTestCommit[:8]} {
		t.Run(pin, func(t *testing.T) {
			f := newPinnedPackCacheExecutionFixture(t, pin)
			if err := os.WriteFile(filepath.Join(f.city, "city.toml"), []byte("[invalid"), 0o644); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			script := filepath.Join(f.caches[0], "commands", "report", "run.sh")
			code := run([]string{"runtime", "check", script}, &stdout, &stderr)
			output := stdout.String() + stderr.String()
			if strings.Contains(output, "city config not loaded") || strings.Contains(output, "warning") {
				t.Errorf("path target gained a config warning: %s", output)
			}
			_, err := os.Stat(f.marker)
			if pin == packCacheTestCommit {
				if err != nil || !strings.Contains(f.gitCalls(t), "status --porcelain") {
					t.Errorf("clean canonical cache lost validated execution: marker stat=%v; %s", err, output)
				}
			} else if code == 0 || !os.IsNotExist(err) || !strings.Contains(output, "does not match its source and commit address") {
				t.Errorf("non-canonical cache lost historical refusal: code=%d marker stat=%v; %s", code, err, output)
			}
		})
	}
}

func TestPackCacheRuntimePathRetainsPinnedExecution(t *testing.T) {
	for _, pin := range []struct{ name, commit string }{
		{"full_lowercase", packCacheTestCommit},
		{"abbreviated", packCacheTestCommit[:8]},
		{"uppercase", strings.ToUpper(packCacheTestCommit)},
	} {
		for _, command := range []struct {
			name string
			args []string
		}{
			{"check", []string{"runtime", "check"}},
			{"conformance", []string{"runtime", "conformance"}},
			{"conformance_env", []string{"runtime", "conformance", "--env"}},
		} {
			t.Run(pin.name+"/"+command.name, func(t *testing.T) {
				f := newPinnedPackCacheExecutionFixture(t, pin.commit)
				script := filepath.Join(f.caches[0], "commands", "report", "run.sh")
				var stdout, stderr bytes.Buffer
				_ = run(append(command.args, script), &stdout, &stderr)
				if _, err := os.Stat(f.marker); err != nil {
					t.Fatalf("clean pinned executable did not run: %v; %s%s", err, &stdout, &stderr)
				}
				if !strings.Contains(f.gitCalls(t), "status --porcelain") {
					t.Fatal("clean pinned executable ran without a full validation")
				}
			})
		}
	}
}

func TestPackCacheRuntimePathRefusesModifiedAbbreviatedPin(t *testing.T) {
	f := newPinnedPackCacheExecutionFixture(t, packCacheTestCommit[:8])
	if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(f.caches[0], "commands", "report", "run.sh")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"runtime", "check", script}, &stdout, &stderr); code == 0 {
		t.Errorf("modified abbreviated-pin executable accepted: %s%s", &stdout, &stderr)
	}
	if _, err := os.Stat(f.marker); !os.IsNotExist(err) {
		t.Errorf("modified abbreviated-pin executable ran: marker stat=%v; %s%s", err, &stdout, &stderr)
	}
	if output := stdout.String() + stderr.String(); !strings.Contains(output, "historical cache") && !strings.Contains(output, "local worktree changes") {
		t.Errorf("missing cache refusal: %s", output)
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
