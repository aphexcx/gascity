//go:build integration

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/subprocess"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/test/tmuxtest"
)

func TestCmdStopSubprocessPersistedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, origin, missing string
		force, live           bool
	}{
		{name: "stale manual forced", origin: "manual", force: true},
		{name: "stale manual graceful", origin: "manual"},
		{name: "stale named forced", origin: "named", force: true},
		{name: "stale named graceful", origin: "named"},
		{name: "live socket", origin: "manual", force: true, live: true},
		{name: "partial city", origin: "manual", force: true, missing: "GC_SESSION_ID"},
		{name: "partial session", origin: "manual", force: true, missing: "GC_CITY_PATH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldBound := stopPerTargetTimeoutDefault
			stopPerTargetTimeoutDefault = 100 * time.Millisecond
			t.Cleanup(func() { stopPerTargetTimeoutDefault = oldBound })
			cityDir := t.TempDir()
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test"}, Beads: config.BeadsConfig{Provider: "file"},
				Session: config.SessionConfig{Provider: "subprocess"}, Daemon: config.DaemonConfig{ShutdownTimeout: "1ns"},
			}
			writeStopLifecycleCityConfig(t, cityDir, cfg)
			store, err := openCityStoreAt(cityDir)
			if err != nil {
				t.Fatal(err)
			}
			const name = "persisted-runtime"
			metadata := map[string]string{"session_origin": tc.origin, "session_name": name, "state": "active"}
			if tc.origin == "named" {
				metadata["configured_named_session"] = "true"
				metadata["configured_named_identity"] = "sky"
			}
			b, err := store.Create(beads.Bead{Type: "session", Metadata: metadata})
			if err != nil {
				t.Fatal(err)
			}
			dir := testutil.ShortTempDir(t, "gc-stop-")
			sp := subprocess.NewSeamBackedWithDir(dir)
			for key, value := range map[string]string{"GC_CITY_PATH": cityDir, "GC_SESSION_ID": b.ID} {
				if key != tc.missing {
					if err := sp.SetMeta(name, key, value); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.live {
				// A control peer acknowledges stop but keeps serving the session.
				// Use the provider's supported legacy socket path.
				listener, err := net.Listen("unix", filepath.Join(dir, name+".sock"))
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					for {
						conn, err := listener.Accept()
						if err != nil {
							return
						}
						_ = conn.SetDeadline(time.Now().Add(testutil.ExecRaceTimeout))
						if bufio.NewScanner(conn).Scan() {
							_, _ = io.WriteString(conn, "ok\n")
						}
						_ = conn.Close()
					}
				}()
				t.Cleanup(func() {
					_ = listener.Close()
					<-done
				})
			}
			oldFactory := sessionProviderForStopCity
			t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
			sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return sp, nil }
			overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
			for attempt := 1; attempt <= 2; attempt++ {
				var stdout, stderr lockedBuffer
				started := time.Now()
				code := cmdStopBody(cityDir, cfg, tc.force, &stdout, &stderr)
				t.Logf("attempt=%d exit=%d stdout=%q stderr=%q", attempt, code, stdout.String(), stderr.String())
				if tc.live || tc.missing != "" {
					if code != 1 || strings.Contains(stdout.String(), "City stopped.") {
						t.Errorf("unresolved identity must fail closed: exit=%d", code)
					}
					if tc.live && (time.Since(started) < stopPerTargetTimeoutDefault || !strings.Contains(stderr.String(), "remains after shutdown")) {
						t.Error("live socket must survive until the shutdown bound")
					}
					if tc.missing != "" && !strings.Contains(stderr.String(), "incomplete ownership") {
						t.Error("partial sidecar must report incomplete ownership")
					}
					continue
				}
				if code != 0 || !strings.Contains(stdout.String(), "City stopped.") {
					t.Errorf("stale sidecar must not block shutdown: exit=%d", code)
				}
				owner := sp.(interface {
					SessionOwnership(string) (string, string, error)
				})
				if _, _, err := owner.SessionOwnership(name); !runtime.IsSessionNotFound(err) {
					t.Errorf("stale sidecar ownership error = %v, want typed absence", err)
				}
			}
		})
	}
}

func TestCmdStopRealTmuxNamedAndIdempotent(t *testing.T) {
	for _, mode := range []string{"named", "removed template", "pool graceful", "pool forced", "start-pending", "drained", "archived", "closed", "drained graceful", "removed template foreign", "start-pending foreign", "drained foreign", "archived foreign", "closed foreign"} {
		t.Run(mode, func(t *testing.T) { testCmdStopRealTmuxLifecycle(t, mode) })
	}
}

func TestCmdStopRealTmuxWithoutConfiguredAgents(t *testing.T) {
	for _, origin := range []string{"manual", "named"} {
		t.Run(origin, func(t *testing.T) { testCmdStopRealTmuxWithoutConfiguredAgents(t, origin) })
	}
}

func testCmdStopRealTmuxWithoutConfiguredAgents(t *testing.T, origin string) {
	t.Helper()
	guard := tmuxtest.NewGuard(t)
	tmuxCfg := tmux.DefaultConfig()
	tmuxCfg.SocketName = guard.SocketName()
	tm := tmux.NewTmuxWithConfig(tmuxCfg)
	cityPath := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: guard.SocketName()}, Beads: config.BeadsConfig{Provider: "file"}}
	writeStopLifecycleCityConfig(t, cityPath, cfg)
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	metadata := map[string]string{"session_origin": origin, "session_name": "manual-runtime", "state": "active", "template": "worker"}
	orphanName := "owned-survivor"
	if origin == "named" {
		metadata["configured_named_session"] = "true"
		metadata["configured_named_identity"] = "sky"
		orphanName = "worker-gc-9"
	}
	b, err := store.Create(beads.Bead{Type: "session", Metadata: metadata})
	if err != nil {
		t.Fatal(err)
	}
	foreign := guard.SocketName() + "-foreign"
	for name, owner := range map[string]string{"manual-runtime": cityPath, orphanName: cityPath, foreign: cityPath + "-other"} {
		env := map[string]string{"GC_CITY_PATH": owner, "GC_SESSION_ID": b.ID}
		if name == orphanName {
			env["GC_SESSION_ID"] = "gc-9"
			env["GC_TEMPLATE"] = "worker"
		}
		if err := tm.NewSessionWithCommandAndEnv(name, cityPath, "exec sleep 300", env); err != nil {
			t.Fatal(err)
		}
	}
	provider := tmux.NewSeamBackedWithConfig(tmuxCfg)
	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return provider, nil }
	overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
	var stdout, stderr lockedBuffer
	code := cmdStopBody(cityPath, cfg, true, &stdout, &stderr)
	t.Logf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	if code != 1 || strings.Contains(stdout.String(), "City stopped.") || !strings.Contains(stderr.String(), orphanName) {
		t.Error("shutdown failed to report the owned survivor")
	}
	if alive, err := tm.HasSession("manual-runtime"); err != nil || alive {
		t.Errorf("manual runtime was not stopped: alive=%v error=%v", alive, err)
	}
	if alive, err := tm.HasSession(foreign); err != nil || !alive || strings.Contains(stderr.String(), foreign) {
		t.Errorf("foreign session was stopped or named: alive=%v error=%v", alive, err)
	}
}

func TestCmdStopRealSubprocessGracefulManual(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}, Beads: config.BeadsConfig{Provider: "file"}, Daemon: config.DaemonConfig{ShutdownTimeout: "3s"}}
	writeStopLifecycleCityConfig(t, cityPath, cfg)
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{
		"session_origin": "manual", "session_name": "manual-runtime", "state": "active",
	}})
	if err != nil {
		t.Fatal(err)
	}
	provider := subprocess.NewSeamBackedWithDir(t.TempDir())
	if err := provider.Start(context.Background(), "manual-runtime", runtime.Config{Command: "exec sleep 300", Env: map[string]string{"GC_CITY_PATH": cityPath, "GC_SESSION_ID": b.ID}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Stop("manual-runtime") })
	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return provider, nil }
	overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
	var stdout, stderr lockedBuffer
	code := cmdStopBody(cityPath, cfg, false, &stdout, &stderr)
	t.Logf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	if code != 0 || provider.IsRunning("manual-runtime") || !strings.Contains(stdout.String(), "City stopped.") {
		t.Error("completed subprocess shutdown reported failure")
	}
}

func TestCmdStopRealSubprocessRemovedTemplateOrphan(t *testing.T) {
	for _, force := range []bool{true, false} {
		t.Run(fmt.Sprintf("force=%v", force), func(t *testing.T) {
			cityPath := t.TempDir()
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test"}, Beads: config.BeadsConfig{Provider: "file"},
				Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}},
				Daemon: config.DaemonConfig{ShutdownTimeout: "3s"},
			}
			writeStopLifecycleCityConfig(t, cityPath, cfg)
			providerDir := testutil.ShortTempDir(t, "gc-orphan-")
			provider := subprocess.NewSeamBackedWithDir(providerDir)
			const orphan = "removed-gc-9"
			var orphanArtifacts []string
			for _, name := range []string{orphan, "foreign-runtime"} {
				owner := cityPath
				if name != orphan {
					owner += "-foreign"
				}
				if err := provider.Start(context.Background(), name, runtime.Config{Command: "exec sleep 300", Env: map[string]string{
					"GC_CITY_PATH": owner, "GC_SESSION_ID": "deleted-record", "GC_TEMPLATE": "removed",
				}}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = provider.Stop(name) })
				if name == orphan {
					entries, err := os.ReadDir(providerDir)
					if err != nil {
						t.Fatal(err)
					}
					var hasSocket, hasName bool
					for _, entry := range entries {
						orphanArtifacts = append(orphanArtifacts, filepath.Join(providerDir, entry.Name()))
						hasSocket = hasSocket || entry.Type()&os.ModeSocket != 0
						hasName = hasName || strings.HasSuffix(entry.Name(), ".name")
					}
					if hasSocket != hasName || len(orphanArtifacts) < 3 {
						t.Fatal("orphan fixture must expose its metadata and a complete socket/name pair when stored here")
					}
					t.Logf("captured %d orphan artifacts; socket in provider directory=%v", len(orphanArtifacts), hasSocket)
				}
			}
			oldFactory := sessionProviderForStopCity
			t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
			sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return provider, nil }
			overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
			for attempt := range 2 {
				var stdout, stderr lockedBuffer
				code := cmdStopBody(cityPath, cfg, force, &stdout, &stderr)
				t.Logf("attempt=%d code=%d owned=%v foreign=%v stdout=%q stderr=%q", attempt+1, code, provider.IsRunning(orphan), provider.IsRunning("foreign-runtime"), stdout.String(), stderr.String())
				if code != 0 || provider.IsRunning(orphan) || !strings.Contains(stdout.String(), "City stopped.") {
					t.Error("removed-template subprocess survived shutdown")
				}
				if !provider.IsRunning("foreign-runtime") || strings.Contains(stderr.String(), "foreign-runtime") {
					t.Error("foreign process was stopped or named")
				}
			}
			for _, path := range orphanArtifacts {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Errorf("orphan artifact %s remains: %v", path, err)
				}
			}
			for _, key := range []string{"GC_CITY_PATH", "GC_SESSION_ID"} {
				if _, err := provider.GetMeta(orphan, key); !runtime.IsSessionNotFound(err) {
					t.Errorf("orphan %s artifacts survived: %v", key, err)
				}
			}
		})
	}
}

func testCmdStopRealTmuxLifecycle(t *testing.T, mode string) {
	t.Helper()
	foreign := strings.HasSuffix(mode, " foreign")
	mode = strings.TrimSuffix(mode, " foreign")
	guard := tmuxtest.NewGuard(t)
	tmuxCfg := tmux.DefaultConfig()
	tmuxCfg.SocketName = guard.SocketName()
	tm := tmux.NewTmuxWithConfig(tmuxCfg)
	cityPath := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: guard.SocketName()}, Beads: config.BeadsConfig{Provider: "file"}, Agents: []config.Agent{{Name: "worker"}}}
	cfg.Daemon.ShutdownTimeout = "3s"
	pool := strings.HasPrefix(mode, "pool")
	if pool {
		cfg.Agents[0].MaxActiveSessions = intPtr(1)
		cfg.Agents[0].MinActiveSessions = intPtr(1)
	}
	if mode == "removed template" {
		cfg.Agents = []config.Agent{{Name: "replacement", MaxActiveSessions: intPtr(1)}}
	}
	writeStopLifecycleCityConfig(t, cityPath, cfg)
	provider := tmux.NewSeamBackedWithConfig(tmuxCfg)
	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return provider, nil }
	overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
	stop := func(stage string) {
		t.Helper()
		var stdout, stderr lockedBuffer
		code := cmdStopBody(cityPath, cfg, !strings.Contains(mode, "graceful"), &stdout, &stderr)
		t.Logf("%s: code=%d stdout=%q stderr=%q", stage, code, stdout.String(), stderr.String())
		if code != 0 || !strings.Contains(stdout.String(), "City stopped.") {
			t.Errorf("%s: code=%d stdout=%q stderr=%q", stage, code, stdout.String(), stderr.String())
		}
	}
	stop("before first start")
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	record := beads.Bead{Type: "session", Metadata: map[string]string{
		"template": "worker", "session_origin": "named", "configured_named_session": "true", "configured_named_identity": "sky",
		"session_name": "persisted-runtime", "state": "active",
	}}
	if pool {
		record.Labels = []string{"gc:session"}
		record.Metadata = map[string]string{"template": "worker", "agent_name": "worker-1", "session_name": "persisted-runtime", "state": "active", "pool_slot": "1"}
	}
	switch mode {
	case "start-pending", "drained", "archived":
		record.Metadata["state"] = mode
	case "drained graceful":
		record.Metadata["state"] = "drained"
	}
	b, err := store.Create(record)
	if err != nil {
		t.Fatal(err)
	}
	if mode == "closed" {
		if err := store.Close(b.ID); err != nil {
			t.Fatal(err)
		}
	}
	ownerCity := cityPath
	if foreign {
		ownerCity = filepath.Join(cityPath, "foreign")
	}
	if err := tm.NewSessionWithCommandAndEnv("persisted-runtime", cityPath, "exec sleep 300", map[string]string{"GC_CITY_PATH": ownerCity, "GC_SESSION_ID": b.ID}); err != nil {
		t.Fatal(err)
	}
	if err := provider.(runtime.ServerLifecycleProvider).ConfigureServer(); err != nil {
		t.Fatal(err)
	}
	stop("named session")
	if alive, err := tm.HasSession("persisted-runtime"); alive != foreign || (err != nil && !errors.Is(err, tmux.ErrNoServer)) {
		t.Errorf("named session after stop: alive=%v error=%v", alive, err)
	}
	if !foreign {
		if _, err := provider.ListRunning(""); !runtime.IsRuntimeServerAbsent(err) {
			t.Errorf("empty configured server survived stop: %v", err)
		}
	}
	stop("second stop")
}

func TestManualStopRealTmuxSessionVanishes(t *testing.T) {
	guard := tmuxtest.NewGuard(t)
	cfg := tmux.DefaultConfig()
	cfg.SocketName = guard.SocketName()
	tm := tmux.NewTmuxWithConfig(cfg)
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{
		"session_origin": "manual", "session_name": "manual-runtime", "state": "active",
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"keep-server-alive", "manual-runtime"} {
		if err := tm.NewSessionWithCommandAndEnv(name, cityPath, "exec sleep 300", map[string]string{
			"GC_CITY_PATH": cityPath, "GC_SESSION_ID": b.ID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	provider := tmux.NewSeamBackedWithConfig(cfg)
	targets, err := resolveManualStopSessionTargets(store, &config.City{}, cityPath, provider)
	if err != nil || len(targets) != 1 {
		t.Fatalf("discovery targets=%v error=%v", targets, err)
	}
	if err := tm.KillSession("manual-runtime"); err != nil {
		t.Fatal(err)
	}
	if err := providerForManualStopTargets(provider, targets).Stop("manual-runtime"); err != nil {
		t.Fatalf("vanished manual session must be confirmed absent: %v", err)
	}
	if alive, err := tm.HasSession("keep-server-alive"); err != nil || !alive {
		t.Fatalf("sibling must survive: alive=%v error=%v", alive, err)
	}
	// A later same-prefix sibling must not supply ownership for the vanished
	// exact name or become the target of a retried stop.
	if err := tm.NewSessionWithCommandAndEnv("manual-runtime-sibling", cityPath, "exec sleep 300", map[string]string{
		"GC_CITY_PATH": cityPath, "GC_SESSION_ID": b.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := providerForManualStopTargets(provider, targets).Stop("manual-runtime"); err != nil {
		t.Fatalf("same-prefix sibling must not change confirmed absence: %v", err)
	}
	if alive, err := tm.HasSession("manual-runtime-sibling"); err != nil || !alive {
		t.Fatalf("same-prefix sibling must survive: alive=%v error=%v", alive, err)
	}
}

func TestCmdStopRealTmuxPendingCreation(t *testing.T) {
	guard := tmuxtest.NewGuard(t)
	tmuxCfg := tmux.DefaultConfig()
	tmuxCfg.SocketName = guard.SocketName()
	tm := tmux.NewTmuxWithConfig(tmuxCfg)
	cityPath := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: guard.SocketName()}, Beads: config.BeadsConfig{Provider: "file"}}
	writeStopLifecycleCityConfig(t, cityPath, cfg)
	provider := tmux.NewSeamBackedWithConfig(tmuxCfg)
	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return provider, nil }
	overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{
		"session_origin": "manual", "session_name": "pending-runtime", "state": "creating", "pending_create_claim": "true",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := tm.NewSessionWithCommandAndEnv("pending-runtime", cityPath, "exec sleep 300", map[string]string{"GC_CITY_PATH": cityPath, "GC_SESSION_ID": b.ID}); err != nil {
		t.Fatal(err)
	}
	if err := tm.NewSessionWithCommandAndEnv("foreign-runtime", cityPath, "exec sleep 300", map[string]string{"GC_CITY_PATH": t.TempDir(), "GC_SESSION_ID": "foreign"}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr lockedBuffer
	code := cmdStopBody(cityPath, cfg, true, &stdout, &stderr)
	after, err := store.Get(b.ID)
	pending, pendingErr := tm.HasSession("pending-runtime")
	foreign, foreignErr := tm.HasSession("foreign-runtime")
	t.Logf("code=%d pending=%v state=%s foreign=%v stdout=%q stderr=%q", code, pending, after.Metadata["state"], foreign, stdout.String(), stderr.String())
	if code != 0 || err != nil || pending || pendingErr != nil || !foreign || foreignErr != nil || after.Metadata["state"] != "failed-create" {
		t.Fatalf("pending shutdown failed: record=%v pending=%v foreign=%v", err, pendingErr, foreignErr)
	}
}

func TestCmdStopRealTmuxUnreachableServer(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("cannot test permission-denied socket access as root")
	}
	guard := tmuxtest.NewGuard(t)
	tmuxCfg := tmux.DefaultConfig()
	tmuxCfg.SocketName = guard.SocketName()
	tm := tmux.NewTmuxWithConfig(tmuxCfg)
	cityPath := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: guard.SocketName()}, Beads: config.BeadsConfig{Provider: "file"}, Agents: []config.Agent{{Name: "worker"}}}
	writeStopLifecycleCityConfig(t, cityPath, cfg)
	provider := tmux.NewSeamBackedWithConfig(tmuxCfg)
	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return provider, nil }
	overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
	for _, name := range []string{"worker-1", "worker-2"} {
		if err := tm.NewSessionWithCommandAndEnv(name, cityPath, "exec sleep 300", map[string]string{"GC_CITY_PATH": cityPath}); err != nil {
			t.Fatal(err)
		}
	}
	socketRoot := os.Getenv("TMUX_TMPDIR")
	if socketRoot == "" {
		socketRoot = "/tmp"
	}
	socketPath := filepath.Join(socketRoot, fmt.Sprintf("tmux-%d", os.Getuid()), guard.SocketName())
	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(socketPath, info.Mode().Perm()); err != nil {
			t.Errorf("restore socket permissions: %v", err)
		}
	})
	if err := os.Chmod(socketPath, 0); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr lockedBuffer
	code := cmdStopBody(cityPath, cfg, true, &stdout, &stderr)
	if err := os.Chmod(socketPath, info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"worker-1", "worker-2"} {
		if alive, err := tm.HasSession(name); err != nil || !alive {
			t.Fatalf("unreachable server must preserve %s: alive=%v error=%v", name, alive, err)
		}
	}
	t.Logf("code=%d both sessions alive stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	if code != 1 || strings.Contains(stdout.String(), "City stopped.") || !strings.Contains(stderr.String(), "Permission denied") {
		t.Fatalf("unreachable server reported success or lost its diagnostic")
	}
}
