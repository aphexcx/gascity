package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	beadsexec "github.com/gastownhall/gascity/internal/beads/exec"
	"github.com/gastownhall/gascity/internal/cacheexec"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	eventsexec "github.com/gastownhall/gascity/internal/events/exec"
	mailexec "github.com/gastownhall/gascity/internal/mail/exec"
	"github.com/gastownhall/gascity/internal/orders"
	runtimeexec "github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/runtime/rppcheck"
	"github.com/gastownhall/gascity/internal/runtime/runtimecapability"
	"github.com/gastownhall/gascity/internal/runtime/runtimecontract"
	"github.com/gastownhall/gascity/internal/usage"
)

func TestConfiguredTextRunnersExplicitCity(t *testing.T) {
	type runner func(city, dir, command string) error
	runners := map[string]runner{
		"workflow-query": func(city, dir, command string) error {
			_, err := nextWorkflowServeBeads(city, command, dir, map[string]string{})
			return err
		},
		"prompt-synthesis": func(city, dir, command string) error {
			_, err := defaultPromptSynthRunner(cacheexec.WithCity(context.Background(), city), &config.ResolvedProvider{Command: "sh", Args: []string{"-c", command}}, "", dir)
			return err
		},
		"sling": func(city, dir, command string) error {
			_, err := shellSlingRunnerForCity(city, dir, command, map[string]string{})
			return err
		},
		"hook": func(city, dir, command string) error {
			_, err := shellWorkQueryForCity(city, command, dir, []string{})
			return err
		},
		"pool": func(city, dir, command string) error {
			_, err := runShellCommandForCity(city, command, dir, time.Second, map[string]string{}, nil)
			return err
		},
		"order-exec": func(city, dir, command string) error {
			_, err := shellExecRunner(cacheexec.WithCity(context.Background(), city), command, dir, []string{})
			return err
		},
		"agent-script": func(city, _, command string) error {
			e := agentScriptExecutor{cityPath: city, stdout: io.Discard, stderr: io.Discard}
			return e.runShellCommand(command, []string{})
		},
		"condition": func(city, dir, command string) error {
			result := orders.CheckTriggerForCity(city, orders.Order{Trigger: "condition", Check: command}, time.Now(), nil, nil, nil, orders.TriggerOptions{ConditionDir: dir, ConditionEnv: []string{}})
			if result.Due {
				return nil
			}
			return fmt.Errorf("%s", result.Reason)
		},
	}
	providerOperations := map[string]func(context.Context, string, string) string{
		"runtime": func(_ context.Context, script, city string) string {
			_, err := runtimeexec.NewProviderForCity(script, city).ListRunning("")
			return fmt.Sprint(err)
		},
		"beads": func(_ context.Context, script, city string) string {
			store := beadsexec.NewStoreForCity(script, city)
			_, err := store.ListOpen()
			return fmt.Sprint(err)
		},
		"mail": func(_ context.Context, script, city string) string {
			_, err := mailexec.NewProviderForCity(script, city).Send("sender", "recipient", "subject", "body")
			return fmt.Sprint(err)
		},
		"events": func(_ context.Context, script, city string) string {
			_, err := eventsexec.NewProviderForCity(script, io.Discard, city).List(events.Filter{})
			return fmt.Sprint(err)
		},
		"event-watch": func(ctx context.Context, script, city string) string {
			watcher, err := eventsexec.NewProviderForCity(script, io.Discard, city).Watch(ctx, 0)
			if watcher != nil {
				_ = watcher.Close()
			}
			return fmt.Sprint(err)
		},
		"usage": func(ctx context.Context, script, city string) string {
			return fmt.Sprint(usage.NewExecSinkForCity(script, city).Record(ctx, usage.Fact{}))
		},
		"runtime-check": func(ctx context.Context, script, city string) string {
			result, err := rppcheck.Run(ctx, script, rppcheck.Options{CityPath: city})
			return fmt.Sprintf("%v %v", result, err)
		},
		"runtime-contract": func(ctx context.Context, script, city string) string {
			result, err := runtimecontract.Run(ctx, script, runtimecontract.Options{CityPath: city})
			return fmt.Sprintf("%v %v", result, err)
		},
		"runtime-capability": func(ctx context.Context, script, city string) string {
			result, err := runtimecapability.Run(ctx, script, runtimecapability.Options{CityPath: city})
			return fmt.Sprintf("%v %v", result, err)
		},
	}
	for name, run := range providerOperations {
		runners[name] = func(city, dir, command string) error {
			script := filepath.Join(dir, "provider.sh")
			if err := os.WriteFile(script, []byte("#!/bin/sh\n"+command+"\n"), 0o755); err != nil {
				return err
			}
			return fmt.Errorf("%s", run(context.Background(), script, city))
		}
	}
	for name, run := range runners {
		t.Run(name, func(t *testing.T) {
			f := newPackCacheFixture(t, 1)
			if _, err := loadCityConfigWithoutBuiltinPackRefresh(f.city, io.Discard); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GC_CITY", "")
			t.Setenv("GC_CITY_PATH", "")
			if err := run(f.city, t.TempDir(), fmt.Sprintf("printf executed > %q", f.marker)); err == nil || !strings.Contains(err.Error(), "has local worktree changes") {
				t.Errorf("runner lost the caller's city: %v", err)
			}
			if _, err := os.Stat(f.marker); !os.IsNotExist(err) {
				t.Errorf("dirty configured command executed: %v", err)
			}
		})
	}
	for _, shape := range []string{"stdin", "pipe", "variable", "cd", "path", "unparseable"} {
		t.Run("shell/"+shape, func(t *testing.T) {
			f := newPackCacheFixture(t, 1)
			if _, err := loadCityConfigWithoutBuiltinPackRefresh(f.city, io.Discard); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(f.caches[0], "commands", "report", "run.sh")
			commands := map[string]string{
				"stdin":       fmt.Sprintf("sh < %q", script),
				"pipe":        fmt.Sprintf("cat %q | sh", script),
				"variable":    fmt.Sprintf("target=%q; sh \"$target\"", script),
				"cd":          fmt.Sprintf("cd %q; ./run.sh", filepath.Dir(script)),
				"path":        fmt.Sprintf("PATH=%q:$PATH; run.sh", filepath.Dir(script)),
				"unparseable": "sh '",
			}
			err := cacheexec.ValidateCachedShellCommand(cacheexec.WithCity(context.Background(), f.city), commands[shape], t.TempDir(), []string{})
			if err == nil || !strings.Contains(err.Error(), "has local worktree changes") {
				t.Fatalf("shell fallback lost explicit city: %v", err)
			}
		})
	}
}
