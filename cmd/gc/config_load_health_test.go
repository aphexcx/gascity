package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/supervisor"
)

func TestConfigLoadHealthDeduplicatesUntilRecovery(t *testing.T) {
	health := &configLoadHealth{}
	start := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	first := errors.New("import first: dirty; run gc import install")
	second := errors.New("import second: dirty; run gc import install")
	if !health.record(first, start) || health.record(first, start.Add(time.Minute)) {
		t.Fatal("repeated failure should only be logged once")
	}
	if got := health.snapshot(); got.Error != first.Error() || !got.Since.Equal(start) {
		t.Fatalf("failure = %+v", got)
	}
	if !strings.Contains(configLoadFailureMessage(health.snapshot()), "config reload") {
		t.Fatal("failure diagnostic lost the established config reload marker")
	}
	if !health.record(second, start.Add(2*time.Minute)) {
		t.Fatal("new failure should be logged")
	}
	if got := health.snapshot(); !got.Since.Equal(start) {
		t.Fatalf("changed error reset incident start: %+v", got)
	}
	if health.record(first, start.Add(3*time.Minute)) {
		t.Fatal("distinct failures should only be logged once per incident")
	}
	if got := health.snapshot(); !got.Since.Equal(start) {
		t.Fatalf("repeated failure lost first-seen time: %+v", got)
	}
	health.clear()
	if health.snapshot() != nil || !health.record(first, start.Add(4*time.Minute)) {
		t.Fatal("recovery should clear health and allow a new incident")
	}
}

func TestConfigReloadHealthLogsDistinctWarnings(t *testing.T) {
	var stderr bytes.Buffer
	cr := &CityRuntime{stderr: &stderr, logPrefix: "gc reload", configLoadHealth: &configLoadHealth{}}
	reply := reloadControlReply{Outcome: reloadOutcomeFailed, Error: "strict mode: 1 collision", Warnings: []string{"first collision"}}
	cr.finishConfigReloadHealth(reply)
	since := cr.configLoadHealth.snapshot().Since
	reply.Warnings = []string{"second collision"}
	cr.finishConfigReloadHealth(reply)
	cr.finishConfigReloadHealth(reply)
	if strings.Count(stderr.String(), "strict mode: 1 collision") != 1 || strings.Count(stderr.String(), "first collision") != 1 || strings.Count(stderr.String(), "second collision") != 1 {
		t.Fatalf("distinct diagnostics were lost or repeated: %s", stderr.String())
	}
	if !cr.configLoadHealth.snapshot().Since.Equal(since) {
		t.Fatal("new warning changed the failure's first-seen time")
	}
	cr.finishConfigReloadHealth(reloadControlReply{Outcome: reloadOutcomeNoChange})
	cr.finishConfigReloadHealth(reply)
	if strings.Count(stderr.String(), "strict mode: 1 collision") != 2 || strings.Count(stderr.String(), "second collision") != 2 {
		t.Fatalf("recovery did not reset diagnostic deduplication: %s", stderr.String())
	}
}

func TestControllerConfigLoadStatusWithoutReconcile(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	health := &configLoadHealth{}
	health.record(errors.New("import second: dirty"), time.Unix(123, 0).UTC())
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleControllerConn(server, "unused", controllerHostingSupervisor, func() {}, nil, nil, nil, nil, nil, nil, health)
	}()
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte(configLoadStatusCommand + "\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(client).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var reply configLoadStatusReply
	if err := json.Unmarshal(line, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Failure == nil || reply.Failure.Error != "import second: dirty" || reply.Failure.Since.Unix() != 123 {
		t.Fatalf("reply = %+v", reply)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("controller did not finish")
	}
}

func TestCityStatusConfigLoadFailureIncludesSince(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "test"); err != nil {
		t.Fatal(err)
	}
	oldAlive, oldRunning := supervisorAliveHook, supervisorCityRunningHook
	supervisorAliveHook = func() int { return 4321 }
	supervisorCityRunningHook = func(string) (bool, string, bool) { return true, "", true }
	t.Cleanup(func() { supervisorAliveHook, supervisorCityRunningHook = oldAlive, oldRunning })
	old := readConfigLoadFailureHook
	t.Cleanup(func() { readConfigLoadFailureHook = old })
	failureHook := func(string) (*configLoadFailure, error) {
		return &configLoadFailure{Error: "city import second: dirty; run gc import install", Since: time.Unix(123, 0).UTC()}, nil
	}
	for _, content := range []string{"[workspace\n", "[workspace]\nname = \"test\"\n[beads]\nprovider = \"file\"\n[session]\nprovider = \"fake\"\n"} {
		writeCityToml(t, cityPath, content)
		for _, jsonOutput := range []bool{false, true} {
			readConfigLoadFailureHook = func(string) (*configLoadFailure, error) { return nil, nil }
			var baseline, baselineErr bytes.Buffer
			baseCode := cmdCityStatus([]string{cityPath}, jsonOutput, &baseline, &baselineErr)
			readConfigLoadFailureHook = failureHook
			var stdout, stderr bytes.Buffer
			code := cmdCityStatus([]string{cityPath}, jsonOutput, &stdout, &stderr)
			output := stdout.String() + stderr.String()
			if code != baseCode || !strings.Contains(output, "1970-01-01T00:02:03Z") || !strings.Contains(output, "city import second") || !strings.Contains(output, "keeping last good config") {
				t.Fatalf("json=%v code=%d output=%s", jsonOutput, code, output)
			}
			if strings.Contains(content, "provider = \"fake\"") {
				if code != 0 || stdout.String() != baseline.String() {
					t.Fatalf("health replaced normal running-city status: code %d, got %s, want %s", code, stdout.String(), baseline.String())
				}
			}
		}
	}
}

func TestCityRuntimeStartupKeepsAcceptedConfigAfterReloadFailure(t *testing.T) {
	clearGCEnv(t)
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	cfg, revision := loadCityRuntimeControllerConfig(t, cityPath)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	builds := 0
	ready := false
	waiting := false
	var stdout, stderr bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath: cityPath, CityName: "test-city", TomlPath: tomlPath,
		ConfigRev: revision, Cfg: cfg, SP: runtime.NewFake(), Rec: events.Discard,
		Stdout: &stdout, Stderr: &stderr,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			builds++
			return DesiredStateResult{}
		},
		OnStarted: func() { ready = true; cancel() },
		OnStatus: func(status string) {
			if status == "loading_config" {
				waiting = true
				cancel()
			}
		},
	})
	od := &recordingOrderDispatcher{}
	cr.od = od
	if err := os.WriteFile(tomlPath, []byte("[[[ bad toml"), 0o644); err != nil {
		t.Fatal(err)
	}
	cr.run(ctx)
	if waiting || !ready || builds == 0 || od.calls.Load() == 0 {
		t.Fatalf("rejected reload stopped startup: waiting=%v ready=%v builds=%d orders=%d", waiting, ready, builds, od.calls.Load())
	}
}
