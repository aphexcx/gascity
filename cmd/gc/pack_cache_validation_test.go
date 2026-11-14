package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	beadsexec "github.com/gastownhall/gascity/internal/beads/exec"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	eventsexec "github.com/gastownhall/gascity/internal/events/exec"
	mailexec "github.com/gastownhall/gascity/internal/mail/exec"
	runtimeexec "github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/runtime/rppcheck"
	"github.com/gastownhall/gascity/internal/runtime/runtimecontract"
	"github.com/gastownhall/gascity/internal/usage"
	"github.com/spf13/cobra"
)

const packCacheTestCommit = "1234567890abcdef1234567890abcdef12345678"

type packCacheFixture struct {
	city   string
	caches []string
	gitLog string
	marker string
}

func newPackCacheFixture(t *testing.T, count int) packCacheFixture {
	t.Helper()
	base := t.TempDir()
	f := packCacheFixture{city: filepath.Join(base, "city"), gitLog: filepath.Join(base, "git-calls"), marker: filepath.Join(base, "executed")}
	gcHome := filepath.Join(base, "gchome")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("GC_CITY", f.city)
	t.Setenv("GC_DIR", "")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_NO_API", "1")
	restoreFlag(t, &cityFlag)
	restoreFlag(t, &rigFlag)
	config.ResetRemoteCacheValidationCache()
	t.Cleanup(config.ResetRemoteCacheValidationCache)
	write := func(path, text string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), mode); err != nil {
			t.Fatal(err)
		}
	}
	var city, lock strings.Builder
	city.WriteString("[workspace]\nname = \"cache-city\"\n")
	lock.WriteString("schema = 1\n")
	for i := range count {
		source := fmt.Sprintf("https://example.com/cache%d.git", i)
		cache := filepath.Join(gcHome, "cache", "repos", config.RepoCacheKey(source, packCacheTestCommit))
		f.caches = append(f.caches, cache)
		fmt.Fprintf(&city, "[imports.sample%d]\nsource = %q\n", i, source)
		fmt.Fprintf(&lock, "[packs.%q]\ncommit = %q\n", source, packCacheTestCommit)
		write(filepath.Join(cache, ".git", "HEAD"), packCacheTestCommit+"\n", 0o644)
		write(filepath.Join(cache, ".git", "index"), "fixture", 0o644)
		write(filepath.Join(cache, "pack.toml"), fmt.Sprintf("[pack]\nname = \"sample%d\"\nschema = 1\n", i), 0o644)
		write(filepath.Join(cache, "commands", "report", "run.sh"), fmt.Sprintf("#!/bin/sh\nprintf executed > %q\n", f.marker), 0o755)
	}
	write(filepath.Join(f.city, "city.toml"), city.String(), 0o644)
	write(filepath.Join(f.city, "packs.lock"), lock.String(), 0o644)
	gitPath := filepath.Join(base, "bin", "git")
	write(gitPath, fmt.Sprintf(`#!/bin/sh
printf '%%s %%s\n' "$PWD" "$*" >> %q
case "$*" in
  --version) printf 'git version 2.50.0\n' ;;
  *'rev-parse HEAD') cat .git/HEAD ;;
  *'status --porcelain') if test -f .dirty; then printf ' M commands/report/run.sh\n'; fi ;;
  *) printf 'unexpected git invocation\n' >&2; exit 1 ;;
esac
`, f.gitLog), 0o755)
	t.Setenv("PATH", filepath.Dir(gitPath)+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

func (f packCacheFixture) gitCalls(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.gitLog)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestPackCacheDiscoveryAndOrdinaryLoadDoNotSpawnGit(t *testing.T) {
	f := newPackCacheFixture(t, 1)
	root := &cobra.Command{Use: "gc"}
	registerPackCommands(root, []string{"version"}, io.Discard, io.Discard)
	if findSubcommand(root, "sample0") == nil {
		t.Fatal("healthy cached command was not discovered")
	}
	if _, err := loadCityConfigWithoutBuiltinPackRefresh(f.city, io.Discard); err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	if _, err := loadCityConfig(f.city, &warnings); err != nil {
		t.Fatal(err)
	}
	if calls := f.gitCalls(t); calls != "" {
		t.Fatalf("discovery/ordinary read spawned Git:\n%s", calls)
	}
}

func TestPackCacheShellFlagsRefuseBeforeExecution(t *testing.T) {
	for _, tc := range []struct{ name, command string }{
		{"combined", `sh -ec '"$PACK_SCRIPT"'`},
		{"login", `bash -lc '"$PACK_SCRIPT"'`},
		{"option-operand", `sh -o errexit -c '"$PACK_SCRIPT"'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPackCacheFixture(t, 1)
			if _, err := loadCityConfigWithoutBuiltinPackRefresh(f.city, io.Discard); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := shellExecRunner(context.Background(), tc.command, f.city, []string{"PACK_SCRIPT=" + filepath.Join(f.caches[0], "commands", "report", "run.sh")})
			if err == nil || !strings.Contains(err.Error(), "has local worktree changes") {
				t.Fatalf("dirty shell command was not refused: %v", err)
			}
			if _, err := os.Stat(f.marker); !os.IsNotExist(err) {
				t.Fatalf("dirty script side effect exists: %v", err)
			}
		})
	}
}

func TestPackCacheExecutionCancellationWhileLockHeld(t *testing.T) {
	f := newPackCacheFixture(t, 1)
	if _, err := loadCityConfigWithoutBuiltinPackRefresh(f.city, io.Discard); err != nil {
		t.Fatal(err)
	}
	locked, release := make(chan struct{}), make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		_, err := config.WithRepoCacheWriteLock(filepath.Dir(f.caches[0]), func() (string, error) {
			close(locked)
			<-release
			return "", nil
		})
		writerDone <- err
	}()
	select {
	case <-locked:
	case err := <-writerDone:
		t.Fatalf("cache writer failed: %v", err)
	}
	done := make(chan error, 1)
	finished := false
	defer func() {
		close(release)
		if err := <-writerDone; err != nil {
			t.Error(err)
		}
		if !finished {
			<-done // release the lock before joining a regressed runner
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	go func() {
		_, err := shellExecRunner(ctx, `"$PACK_SCRIPT"`, f.city, []string{"PACK_SCRIPT=" + filepath.Join(f.caches[0], "commands", "report", "run.sh")})
		done <- err
	}()
	select {
	case err := <-done:
		finished = true
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("execution error = %v; want deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("execution deadline could not cancel cache validation while lock remained held")
	}
	if _, err := os.Stat(f.marker); !os.IsNotExist(err) {
		t.Fatalf("canceled script side effect exists: %v", err)
	}
}

func TestPackCacheVersionAndHelpDoNotSpawnGit(t *testing.T) {
	for _, command := range []string{"version", "help", "import"} {
		t.Run(command, func(t *testing.T) {
			f := newPackCacheFixture(t, 1)
			var stdout, stderr bytes.Buffer
			if code := run([]string{command}, &stdout, &stderr); code != 0 {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			if command == "help" && !strings.Contains(stdout.String(), "sample0") {
				t.Fatal("help omitted discovered pack")
			}
			if calls := f.gitCalls(t); calls != "" {
				t.Fatalf("%s spawned Git:\n%s", command, calls)
			}
		})
	}
}

func TestPackCacheWrongCommitRefusesDiscoveryAndOrdinaryLoad(t *testing.T) {
	f := newPackCacheFixture(t, 1)
	wrong := strings.Repeat("f", 40)
	if err := os.WriteFile(filepath.Join(f.caches[0], ".git", "HEAD"), []byte(wrong+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := &cobra.Command{Use: "gc"}
	registerPackCommands(root, nil, io.Discard, io.Discard)
	if findSubcommand(root, "sample0") != nil {
		t.Fatal("discovery accepted the wrong commit")
	}
	_, err := loadCityConfigWithoutBuiltinPackRefresh(f.city, io.Discard)
	want := fmt.Sprintf("cached import https://example.com/cache0.git is checked out at %s, expected %s; run \"gc import install\"", wrong, packCacheTestCommit)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want %s", err, want)
	}
	if calls := f.gitCalls(t); calls != "" {
		t.Fatalf("commit mismatch spawned Git:\n%s", calls)
	}
}

func TestPackCacheExecutionScansOnlyOwningImport(t *testing.T) {
	f := newPackCacheFixture(t, 3)
	root := &cobra.Command{Use: "gc"}
	var stderr bytes.Buffer
	registerPackCommands(root, nil, io.Discard, &stderr)
	root.SetArgs([]string{"sample1", "report"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v; %s", err, stderr.String())
	}
	if _, err := os.Stat(f.marker); err != nil {
		t.Fatalf("script did not execute: %v", err)
	}
	calls := f.gitCalls(t)
	if strings.Count(calls, "status --porcelain") != 1 || !strings.Contains(calls, f.caches[1]+" -c core.fsmonitor=false") {
		t.Fatalf("expected one scan of owning cache, got:\n%s", calls)
	}
	for _, other := range []string{f.caches[0], f.caches[2]} {
		if strings.Contains(calls, other+" ") {
			t.Fatalf("execution checked unrelated import %s:\n%s", other, calls)
		}
	}
}

func TestPackCacheDirtyCommandRefusedBeforeExecution(t *testing.T) {
	f := newPackCacheFixture(t, 1)
	// Make the change after discovery: execution must not reuse a QUICK result.
	root := &cobra.Command{Use: "gc"}
	var stderr bytes.Buffer
	registerPackCommands(root, nil, io.Discard, &stderr)
	if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	root.SetArgs([]string{"sample0", "report"})
	if err := root.Execute(); err == nil {
		t.Fatal("dirty command executed successfully")
	}
	want := "cached import https://example.com/cache0.git has local worktree changes; run \"gc import install\""
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want %s", stderr.String(), want)
	}
	if _, err := os.Stat(f.marker); !os.IsNotExist(err) {
		t.Fatalf("script side effect exists or stat failed: %v", err)
	}
}

func TestPackCacheManifestCannotRedirectExecutionPastValidation(t *testing.T) {
	f := newPackCacheFixture(t, 3)
	localScript := filepath.Join(f.city, "local-runner.sh")
	if err := os.WriteFile(localScript, []byte(fmt.Sprintf("#!/bin/sh\ntouch %q\n", f.marker)), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(f.caches[0], "commands", "report", "command.toml")
	if err := os.WriteFile(manifest, []byte("run = \"{{.CityRoot}}/local-runner.sh\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	root := &cobra.Command{Use: "gc"}
	var stderr bytes.Buffer
	registerPackCommands(root, nil, io.Discard, &stderr)
	root.SetArgs([]string{"sample0", "report"})
	if err := root.Execute(); err == nil || !strings.Contains(stderr.String(), "has local worktree changes") {
		t.Errorf("dirty manifest was not refused: %v, stderr=%s", err, stderr.String())
	}
	if _, err := os.Stat(f.marker); !os.IsNotExist(err) {
		t.Errorf("redirected script executed: %v", err)
	}
}

func TestPackCacheInlineWorkQueryRefusedBeforeExecution(t *testing.T) {
	f := newPackCacheFixture(t, 1)
	if _, err := loadCityConfigWithoutBuiltinPackRefresh(f.city, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := shellWorkQueryWithEnv(fmt.Sprintf("printf dirty > %q; printf '[]'", f.marker), f.city, nil)
	if err == nil || !strings.Contains(err.Error(), "has local worktree changes") {
		t.Errorf("inline configuration instructions were not refused: %v", err)
	}
	if _, err := os.Stat(f.marker); !os.IsNotExist(err) {
		t.Errorf("inline work query executed: %v", err)
	}
}

func TestPackCacheInterpreterArgumentsValidatedBeforeExecution(t *testing.T) {
	f := newPackCacheFixture(t, 2)
	if _, err := loadCityConfigWithoutBuiltinPackRefresh(f.city, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.caches[1], ".dirty"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	entry := config.DiscoveredCommand{
		PackDir: f.caches[0], SourceDir: filepath.Join(f.caches[0], "commands", "report"), RunScript: "/bin/sh",
	}
	var stderr bytes.Buffer
	code := runDiscoveredCommand(entry, f.city, "cache-city", []string{"-c", fmt.Sprintf("%q", filepath.Join(f.caches[1], "commands", "report", "run.sh"))}, nil, io.Discard, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "has local worktree changes") {
		t.Errorf("interpreter payload was not refused: code=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(f.marker); !os.IsNotExist(err) {
		t.Errorf("interpreter payload executed: %v", err)
	}
}

func TestPackCachePromptSynthesisValidatesConfigurationSource(t *testing.T) {
	f := newPackCacheFixture(t, 1)
	if _, err := loadCityConfigWithoutBuiltinPackRefresh(f.city, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	provider := &config.ResolvedProvider{Command: "sh", Args: []string{"-c", fmt.Sprintf("printf dirty > %q", f.marker)}}
	_, err := defaultPromptSynthRunner(t.Context(), provider, "prompt", f.city)
	if err == nil || !strings.Contains(err.Error(), "has local worktree changes") {
		t.Errorf("provider configuration was not refused: %v", err)
	}
	if _, err := os.Stat(f.marker); !os.IsNotExist(err) {
		t.Errorf("provider configuration executed: %v", err)
	}
}

func TestPackCacheDirectBDRunnersRefuseDirtyExecutable(t *testing.T) {
	for _, watch := range []bool{false, true} {
		t.Run(fmt.Sprintf("watch=%t", watch), func(t *testing.T) {
			f := newPackCacheFixture(t, 1)
			if _, err := loadCityConfigWithoutBuiltinPackRefresh(f.city, io.Discard); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(f.caches[0], "commands", "report", "run.sh")
			var stderr bytes.Buffer
			var code int
			if watch {
				runner := &bdWatchRunner{bdPath: path, dir: f.city, env: os.Environ(), cityPath: f.city, scopeRoot: f.city, stderr: &stderr}
				code = runner.run(t.Context(), []string{"show", "sample"}, io.Discard, &stderr)
			} else {
				code = runBdSubprocess(path, []string{"list"}, f.city, execStoreTarget{ScopeRoot: f.city}, os.Environ(), io.Discard, &stderr)
			}
			if code == 0 || !strings.Contains(stderr.String(), "has local worktree changes") {
				t.Errorf("dirty beads executable was not refused: exit=%d stderr=%q", code, stderr.String())
			}
			if _, err := os.Stat(f.marker); !os.IsNotExist(err) {
				t.Errorf("dirty beads executable ran: %v", err)
			}
		})
	}
}

func TestPackCacheConfiguredProviderOperations(t *testing.T) {
	operations := map[string]func(context.Context, string, string) string{
		"runtime": func(_ context.Context, script, _ string) string {
			_, err := runtimeexec.NewProvider(script).ListRunning("")
			return fmt.Sprint(err)
		},
		"beads": func(_ context.Context, script, city string) string {
			store := beadsexec.NewStore(script)
			store.SetEnv(map[string]string{"GC_CITY_PATH": city})
			_, err := store.ListOpen()
			return fmt.Sprint(err)
		},
		"mail": func(_ context.Context, script, _ string) string {
			_, err := mailexec.NewProvider(script).Send("sender", "recipient", "subject", "body")
			return fmt.Sprint(err)
		},
		"events": func(_ context.Context, script, _ string) string {
			_, err := eventsexec.NewProvider(script, io.Discard).List(events.Filter{})
			return fmt.Sprint(err)
		},
		"event-watch": func(ctx context.Context, script, _ string) string {
			watcher, err := eventsexec.NewProvider(script, io.Discard).Watch(ctx, 0)
			if watcher != nil {
				_ = watcher.Close()
			}
			return fmt.Sprint(err)
		},
		"usage": func(ctx context.Context, script, _ string) string {
			return fmt.Sprint(usage.NewExecSink(script).Record(ctx, usage.Fact{}))
		},
		"runtime-check": func(ctx context.Context, script, _ string) string {
			result, err := rppcheck.Run(ctx, script, rppcheck.Options{})
			return fmt.Sprintf("%v %v", result, err)
		},
		"runtime-contract": func(ctx context.Context, script, city string) string {
			result, err := runtimecontract.Run(ctx, script, runtimecontract.Options{WorkDir: city})
			return fmt.Sprintf("%v %v", result, err)
		},
	}
	for name, run := range operations {
		t.Run(name, func(t *testing.T) {
			f := newPackCacheFixture(t, 1)
			t.Setenv("GC_CITY_PATH", f.city)
			if _, err := loadCityConfigWithoutBuiltinPackRefresh(f.city, io.Discard); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(f.city, "local-provider.sh")
			if err := os.WriteFile(script, []byte(fmt.Sprintf("#!/bin/sh\nprintf dirty > %q\n", f.marker)), 0o755); err != nil {
				t.Fatal(err)
			}
			if result := run(t.Context(), script, f.city); !strings.Contains(result, "has local worktree changes") {
				t.Error("provider did not report its dirty cached configuration source")
			}
			if _, err := os.Stat(f.marker); !os.IsNotExist(err) {
				t.Errorf("configuration-selected local provider executed: %v", err)
			}
		})
	}
}

func TestPackCacheProviderFactoriesRetainExplicitCity(t *testing.T) {
	for _, kind := range []string{"mail", "events", "usage", "runtime", "pack-runtime", "runtime-check", "runtime-conformance"} {
		t.Run(kind, func(t *testing.T) {
			f := newPackCacheFixture(t, 1)
			t.Setenv("GC_CITY", "")
			t.Setenv("GC_CITY_PATH", "")
			t.Chdir(t.TempDir())
			cityFlag = f.city
			if _, err := loadCityConfigWithoutBuiltinPackRefresh(f.city, io.Discard); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(f.city, "local-provider.sh")
			if err := os.WriteFile(script, []byte(fmt.Sprintf("#!/bin/sh\nprintf dirty > %q\n", f.marker)), 0o755); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "mail":
				_, err = newCommandMailProviderNamed("exec:"+script, nil).Send("sender", "recipient", "subject", "body")
			case "events":
				provider, openErr := newEventsProviderForName("exec:"+script, "", io.Discard)
				if openErr != nil {
					t.Fatal(openErr)
				}
				_, err = provider.List(events.Filter{})
			case "usage":
				cfg := &config.City{}
				cfg.Usage.Provider = "exec:" + script
				err = usageSinkForCity(cfg, f.city).Record(t.Context(), usage.Fact{})
			case "runtime", "pack-runtime":
				cfg := &config.City{Runtimes: map[string]config.DiscoveredRuntime{
					"sample-runtime": {Name: "sample-runtime", Command: script, PackName: "sample"},
				}}
				reg, registryErr := runtimeRegistryForCity(cfg)
				if registryErr != nil {
					t.Fatal(registryErr)
				}
				name := "exec:" + script
				if kind == "pack-runtime" {
					name = "sample-runtime"
				}
				provider, createErr := reg.New(name, config.SessionConfig{}, "cache-city", f.city)
				if createErr != nil {
					t.Fatal(createErr)
				}
				_, err = provider.ListRunning("")
			case "runtime-check", "runtime-conformance":
				var output bytes.Buffer
				command := newRuntimeCheckCmd(&output, &output)
				args := []string{script}
				if kind == "runtime-conformance" {
					command = newRuntimeConformanceCmd(&output, &output)
					args = append(args, "--env")
				}
				command.SetArgs(args)
				if runErr := command.Execute(); runErr != nil {
					err = fmt.Errorf("%w: %s", runErr, output.String())
				}
			}
			if err == nil || !strings.Contains(err.Error(), "has local worktree changes") {
				t.Errorf("explicit city's dirty source was not refused: %v", err)
			}
			if _, err := os.Stat(f.marker); !os.IsNotExist(err) {
				t.Errorf("explicit city's provider executed: %v", err)
			}
		})
	}
}

func TestPackCacheExecutionUsesSelectedCity(t *testing.T) {
	for _, operation := range []string{"mail", "events", "synthesis", "runtime-payload"} {
		for _, selectDirty := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/selected-dirty=%t", operation, selectDirty), func(t *testing.T) {
				f := newPackCacheFixture(t, 1)
				other, rig := t.TempDir(), t.TempDir()
				script := filepath.Join(other, "local-provider.sh")
				if err := os.WriteFile(script, []byte(fmt.Sprintf("#!/bin/sh\nprintf executed > %q\nprintf '{}\\n'\n", f.marker)), 0o755); err != nil {
					t.Fatal(err)
				}
				mailConfig := fmt.Sprintf("\n[mail]\nprovider = %q\n", "exec:"+script)
				cityConfig, err := os.ReadFile(filepath.Join(f.city, "city.toml"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.city, "city.toml"), append(cityConfig, []byte(mailConfig)...), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(other, "city.toml"), []byte("[workspace]\nname = 'other'\n"+mailConfig), 0o644); err != nil {
					t.Fatal(err)
				}
				for _, city := range []string{f.city, other} {
					if _, err := loadCityConfigWithoutBuiltinPackRefresh(city, io.Discard); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
				selected, ambient := other, f.city
				if selectDirty {
					selected, ambient = f.city, other
				}
				cityFlag, rigFlag = "", ""
				t.Setenv("GC_CITY_PATH", ambient)
				t.Setenv("GC_CITY", ambient)
				t.Setenv("GC_MAIL", "")
				t.Setenv("GC_EVENTS", "")
				var runErr error
				switch operation {
				case "mail":
					_, runErr = defaultMailProvider(selected).Send("sender", "recipient", "subject", "body")
				case "events":
					cfg := &config.City{}
					cfg.Events.Provider = "exec:" + script
					cliFactoryEventsRecorder(selected, cfg).Record(events.Event{})
				case "synthesis":
					runErr = runDirectSynth(t.Context(), promptSynthOpts{}, selected, "worker", "prompt",
						metaPromptCtx{CityPath: selected, ContextType: "rig", RigPath: rig},
						&config.ResolvedProvider{Command: script}, defaultPromptSynthRunner, io.Discard, io.Discard)
				case "runtime-payload":
					_, _, runErr = runtimeexec.NewProviderForCity(script, selected).Exec(t.Context(), "session", []string{"printf", "ok"})
				}
				if !selectDirty && runErr != nil {
					t.Errorf("clean selected city was rejected: %v", runErr)
				}
				if selectDirty && operation != "events" && (runErr == nil || !strings.Contains(runErr.Error(), "has local worktree changes")) {
					t.Errorf("dirty selected city was not refused: %v", runErr)
				}
				_, markerErr := os.Stat(f.marker)
				if selectDirty && !os.IsNotExist(markerErr) {
					t.Errorf("dirty selected city executed: %v", markerErr)
				} else if !selectDirty && markerErr != nil {
					t.Errorf("clean selected city did not execute: %v", markerErr)
				}
			})
		}
	}
}

func TestPackCacheFullLifecycleLoadsRejectDirtyImport(t *testing.T) {
	for name, load := range map[string]func(string) error{
		"start":             func(city string) error { _, _, err := loadStartCityConfig(city); return err },
		"supervisor":        func(city string) error { _, _, err := loadSupervisorCityConfig(city); return err },
		"config-validation": func(city string) error { _, _, err := loadConfigCommandCityConfig(city); return err },
		"doctor":            func(city string) error { _, err := loadCityConfigAllowMissingProviderReferences(city); return err },
		"import":            validateComposedConfigAfterInstall,
	} {
		t.Run(name, func(t *testing.T) {
			f := newPackCacheFixture(t, 1)
			if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), []byte("changed"), 0o644); err != nil {
				t.Fatal(err)
			}
			err := load(f.city)
			if err == nil || !strings.Contains(err.Error(), "has local worktree changes; run \"gc import install\"") {
				t.Fatalf("FULL load error = %v", err)
			}
			if calls := f.gitCalls(t); !strings.Contains(calls, "status --porcelain") {
				t.Fatalf("FULL scan count: %s", calls)
			}
		})
	}
}

func TestPackCacheImportReadCommandsValidateCaches(t *testing.T) {
	for _, args := range [][]string{{"list"}, {"why", "sample0"}} {
		t.Run(args[0], func(t *testing.T) {
			f := newPackCacheFixture(t, 1)
			if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), []byte("changed"), 0o644); err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd := newImportCmd(io.Discard, &stderr)
			cmd.SetArgs(args)
			if err := cmd.Execute(); err == nil {
				t.Fatal("import inspection accepted dirty cache")
			}
			if calls := f.gitCalls(t); !strings.Contains(calls, "status --porcelain") {
				t.Fatalf("import did not scan: %s", calls)
			}
		})
	}
}

func TestPackCacheDiscovery3000FilesTiming(t *testing.T) {
	path := os.Getenv("PATH")
	f := newPackCacheFixture(t, 1)
	t.Setenv("PATH", path)
	cache := f.caches[0]
	if err := os.RemoveAll(filepath.Join(cache, ".git")); err != nil {
		t.Fatal(err)
	}
	for i := range 3000 {
		dir := filepath.Join(cache, "fixture", fmt.Sprintf("%02d", i/100))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%04d.txt", i)), []byte("fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runRegistryPublishGit(t, cache, "init", "-q")
	runRegistryPublishGit(t, cache, "add", ".")
	runRegistryPublishGit(t, cache, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.com", "-c", "commit.gpgsign=false", "commit", "-qm", "fixture")
	commit := runRegistryPublishGit(t, cache, "rev-parse", "HEAD")
	newCache := filepath.Join(filepath.Dir(cache), config.RepoCacheKey("https://example.com/cache0.git", commit))
	if err := os.Rename(cache, newCache); err != nil {
		t.Fatal(err)
	}
	lock := fmt.Sprintf("schema = 1\n[packs.\"https://example.com/cache0.git\"]\ncommit = %q\n", commit)
	if err := os.WriteFile(filepath.Join(f.city, "packs.lock"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	previous := advisoryLoad
	t.Cleanup(func() { advisoryLoad = previous })
	for _, mode := range []config.CacheValidationMode{config.CacheValidationFull, config.CacheValidationQuick} {
		config.ResetRemoteCacheValidationCache()
		advisoryLoad.RemoteCacheValidation = mode
		root := &cobra.Command{Use: "gc"}
		started := time.Now()
		registerPackCommands(root, []string{"version"}, io.Discard, io.Discard)
		elapsed := time.Since(started)
		if findSubcommand(root, "sample0") == nil {
			t.Fatal("fixture command was not discovered")
		}
		t.Logf("discovery fixture_files=3000 mode=%d elapsed=%s (FULL=0 is previous behavior; QUICK=1)", mode, elapsed)
	}
}
