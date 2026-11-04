//go:build integration

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/acp"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
	"github.com/gastownhall/gascity/internal/runtime/subprocess"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/test/tmuxtest"
)

func TestCmdStopSubprocessChildHelper(t *testing.T) {
	ready := os.Getenv("GASCITY_STOP_CHILD_READY")
	if ready == "" {
		t.Skip("subprocess child helper")
	}
	if os.Getenv("GASCITY_STOP_CHILD_ACP") == "1" {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			var request struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
				t.Fatal(err)
			}
			if request.Method == "initialize" || request.Method == "session/new" {
				response := struct {
					JSONRPC string `json:"jsonrpc"`
					ID      int64  `json:"id"`
					Result  struct {
						SessionID string `json:"sessionId"`
					} `json:"result"`
				}{JSONRPC: "2.0", ID: request.ID}
				response.Result.SessionID = "stop-test"
				if err := json.NewEncoder(os.Stdout).Encode(response); err != nil {
					t.Fatal(err)
				}
			}
			if request.Method == "session/new" {
				break
			}
		}
		if err := scanner.Err(); err != nil {
			t.Fatal(err)
		}
	}
	if os.Getenv("GASCITY_STOP_CHILD_DESCENDANT") == "1" {
		pid := startSigtermIgnoringProcess(t, filepath.Dir(ready))
		if err := os.WriteFile(ready+".descendant", []byte(strconv.Itoa(pid)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(ready, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("GASCITY_STOP_CHILD_OPAQUE") == "1" {
		if err := syscall.Exec("/bin/sleep", []string{"sleep", "300"}, os.Environ()); err != nil {
			t.Fatal(err)
		}
	}
	// The parent reaps this fixture through its original provider handle.
	<-time.NewTimer(time.Hour).C
}

func TestCmdStopSubprocessPersistedIdentity(t *testing.T) {
	for _, leaf := range []string{"subprocess", "acp"} {
		t.Run(leaf, func(t *testing.T) {
			for _, tc := range []struct {
				name, origin, missing      string
				force, live, child, opaque bool
				legacy                     bool
				descendant                 bool
			}{
				{name: "stale manual forced", origin: "manual", force: true},
				{name: "stale manual graceful", origin: "manual"},
				{name: "stale named forced", origin: "named", force: true},
				{name: "stale named graceful", origin: "named"},
				{name: "live socket", origin: "manual", force: true, live: true},
				{name: "child without socket", origin: "manual", force: true, child: true},
				{name: "opaque child without socket", origin: "manual", force: true, child: true, opaque: true},
				{name: "partial city", origin: "manual", force: true, missing: "GC_SESSION_ID"},
				{name: "partial session", origin: "manual", force: true, missing: "GC_CITY_PATH"},
				{name: "stale desired", origin: "desired", force: true},
				{name: "desired child", origin: "desired", force: true, child: true},
				{name: "desired opaque child", origin: "desired", force: true, child: true, opaque: true},
				{name: "desired partial city", origin: "desired", force: true, child: true, missing: "GC_SESSION_ID"},
				{name: "desired partial session", origin: "desired", force: true, child: true, missing: "GC_CITY_PATH"},
				{name: "desired no ownership", origin: "desired", force: true, child: true, missing: "both"},
				{name: "orphan child", origin: "orphan", force: true, child: true},
				{name: "orphan opaque child", origin: "orphan", force: true, child: true, opaque: true},
				{name: "legacy manual", origin: "manual", force: true, legacy: true},
				{name: "legacy named", origin: "named", legacy: true},
				{name: "leader exits descendant survives", origin: "manual", force: true, child: true, descendant: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					oldBound := stopPerTargetTimeoutDefault
					// Only intentional survivors use the shortened bound. Real
					// children retain the production budget for kernel reaping.
					if !tc.child || tc.missing != "" || tc.descendant {
						stopPerTargetTimeoutDefault = 100 * time.Millisecond
					}
					t.Cleanup(func() { stopPerTargetTimeoutDefault = oldBound })
					cityDir := t.TempDir()
					cfg := &config.City{
						Workspace: config.Workspace{Name: "test"}, Beads: config.BeadsConfig{Provider: "file"},
						Session: config.SessionConfig{Provider: leaf}, Daemon: config.DaemonConfig{ShutdownTimeout: "1ns"},
					}
					if tc.origin == "desired" {
						one := 1
						cfg.Agents = []config.Agent{{Name: "worker", MaxActiveSessions: &one}}
					}
					writeStopLifecycleCityConfig(t, cityDir, cfg)
					store, err := openCityStoreAt(cityDir)
					if err != nil {
						t.Fatal(err)
					}
					name := "persisted-runtime"
					if tc.origin == "desired" {
						name, err = lookupSessionNameOrLegacyChecked(store, "test", "worker", cfg.Workspace.SessionTemplate)
						if err != nil {
							t.Fatal(err)
						}
					}
					metadata := map[string]string{"session_origin": tc.origin, "session_name": name, "state": "active"}
					if tc.origin == "named" {
						metadata["configured_named_session"] = "true"
						metadata["configured_named_identity"] = "sky"
					}
					b, err := store.Create(beads.Bead{Type: "session", Metadata: metadata})
					if err != nil {
						t.Fatal(err)
					}
					if tc.origin == "orphan" || tc.origin == "desired" {
						if err := store.Delete(b.ID); err != nil {
							t.Fatal(err)
						}
					}
					dir := testutil.ShortTempDir(t, "gc-stop-")
					newProvider := func() runtime.Provider {
						if leaf == "acp" {
							return acp.NewSeamBackedWithDir(dir, acp.Config{})
						}
						return subprocess.NewSeamBackedWithDir(dir)
					}
					sp := newProvider()
					identity := struct {
						Name  string `json:"name"`
						PID   int    `json:"pid"`
						Start string `json:"start"`
					}{Name: name, PID: os.Getpid(), Start: "stale-start-token"}
					var descendantPID int
					var descendantIdentity string
					if tc.child {
						executable, err := os.Executable()
						if err != nil {
							t.Fatal(err)
						}
						ready := filepath.Join(dir, "child-ready")
						command := "exec " + shellQuotePath(executable) + " -test.run=^TestCmdStopSubprocessChildHelper$"
						env := map[string]string{"GC_CITY_PATH": cityDir, "GC_SESSION_ID": b.ID, "GASCITY_STOP_CHILD_READY": ready}
						if tc.opaque {
							env["GASCITY_STOP_CHILD_OPAQUE"] = "1"
						}
						if tc.descendant {
							env["GASCITY_STOP_CHILD_DESCENDANT"] = "1"
						}
						if leaf == "acp" {
							env["GASCITY_STOP_CHILD_ACP"] = "1"
						}
						owner := sp
						if err := owner.Start(context.Background(), name, runtime.Config{
							Command: command,
							Env:     env,
						}); err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() {
							if (tc.missing != "" || tc.descendant) && identity.PID > 1 && identity.PID != os.Getpid() {
								_ = syscall.Kill(-identity.PID, syscall.SIGKILL)
								if err := waitForProcessGroupExit(identity.PID, testutil.ExecRaceTimeout); err != nil {
									t.Error(err)
								}
							}
							if err := owner.Stop(name); err != nil && tc.missing == "" {
								t.Errorf("reaping test child: %v", err)
							}
						})
						pidText := waitForNonEmptyFileContent(t, ready, testutil.ExecRaceTimeout)
						identity.PID, err = strconv.Atoi(strings.TrimSpace(pidText))
						if err != nil {
							t.Fatal(err)
						}
						identity.Start, err = proctable.ProcessIdentity(identity.PID)
						if err != nil {
							t.Fatal(err)
						}
						if tc.descendant {
							pidText := waitForNonEmptyFileContent(t, ready+".descendant", testutil.ExecRaceTimeout)
							descendantPID, err = strconv.Atoi(strings.TrimSpace(pidText))
							if err != nil {
								t.Fatal(err)
							}
							descendantIdentity, err = proctable.ProcessIdentity(descendantPID)
							if err != nil {
								t.Fatal(err)
							}
						}
						key := "gc_process_identity"
						if leaf == "acp" {
							key = "gc_session_owner"
						}
						raw, _ := owner.GetMeta(name, key)
						var recorded struct {
							PID   int    `json:"pid"`
							Start string `json:"start"`
						}
						if json.Unmarshal([]byte(raw), &recorded) != nil || recorded.PID != identity.PID || recorded.Start != identity.Start {
							t.Errorf("spawn did not persist child identity: pid=%d start=%q", recorded.PID, recorded.Start)
						}
						// Reproduce the lost controller's observable state: a detached
						// child, no reachable socket, and a fresh provider with no map.
						sockets, err := filepath.Glob(filepath.Join(dir, "*.sock"))
						if err != nil || len(sockets) != 1 {
							t.Fatalf("child control sockets = %v, %v", sockets, err)
						}
						if err := os.Remove(sockets[0]); err != nil {
							t.Fatal(err)
						}
						if !owner.IsRunning(name) {
							t.Fatal("child exited before its socket was lost")
						}
						sp = newProvider()
					}
					for key, value := range map[string]string{"GC_CITY_PATH": cityDir, "GC_SESSION_ID": b.ID} {
						if key != tc.missing && tc.missing != "both" {
							if err := sp.SetMeta(name, key, value); err != nil {
								t.Fatal(err)
							}
						} else if tc.child {
							if err := sp.RemoveMeta(name, key); err != nil {
								t.Fatal(err)
							}
						}
					}
					var record []byte
					if leaf == "acp" {
						owner := struct {
							City    string `json:"city_path"`
							Session string `json:"session_id"`
							Name    string `json:"name,omitempty"`
							PID     int    `json:"pid,omitempty"`
							Start   string `json:"start,omitempty"`
						}{City: cityDir, Session: b.ID}
						if tc.missing == "GC_CITY_PATH" || tc.missing == "both" {
							owner.City = ""
						}
						if tc.missing == "GC_SESSION_ID" || tc.missing == "both" {
							owner.Session = ""
						}
						if !tc.legacy {
							owner.Name, owner.PID, owner.Start = identity.Name, identity.PID, identity.Start
						}
						record, err = json.Marshal(owner)
						if err == nil {
							err = sp.SetMeta(name, "gc_session_owner", string(record))
						}
					} else if !tc.legacy {
						record, err = json.Marshal(identity)
						if err == nil {
							err = sp.SetMeta(name, "gc_process_identity", string(record))
						}
					}
					if err != nil {
						t.Fatal(err)
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
					if tc.origin == "desired" {
						_, desired, _, err := resolveStopSessionNames(store, cfg, cityDir, "test", sp, io.Discard)
						if err != nil || !desired[name] {
							t.Fatalf("fixture is not a desired singleton: desired=%v, error=%v", desired[name], err)
						}
					}
					t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
					sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return sp, nil }
					overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
					for attempt := 1; attempt <= 2; attempt++ {
						var stdout, stderr lockedBuffer
						started := time.Now()
						code := cmdStopBody(cityDir, cfg, tc.force, &stdout, &stderr)
						t.Logf("attempt=%d exit=%d stdout=%q stderr=%q", attempt, code, stdout.String(), stderr.String())
						if tc.live || tc.legacy || tc.missing != "" || tc.descendant {
							if code != 1 || strings.Contains(stdout.String(), "City stopped.") {
								t.Errorf("unresolved identity must fail closed: exit=%d", code)
							}
							if (tc.live || tc.legacy) && (time.Since(started) < stopPerTargetTimeoutDefault || !strings.Contains(stderr.String(), "remains after shutdown")) {
								t.Error("live runtime must survive until the shutdown bound")
							}
							if tc.missing != "" && !strings.Contains(stderr.String(), "incomplete ownership") &&
								(tc.origin != "desired" || !strings.Contains(stderr.String(), "carry no ownership record")) {
								t.Error("partial sidecar must report incomplete ownership")
							}
							if tc.missing != "" && tc.child {
								if current, err := proctable.ProcessIdentity(identity.PID); err != nil || current != identity.Start {
									t.Errorf("partial identity child was signaled: %q, %v", current, err)
								}
								key := "gc_process_identity"
								if leaf == "acp" {
									key = "gc_session_owner"
								}
								if got, err := sp.GetMeta(name, key); err != nil || got != string(record) {
									t.Errorf("refused target lost its process record: %q, %v", got, err)
								}
							}
							if tc.descendant {
								if current, err := proctable.ProcessIdentity(descendantPID); err != nil || current != descendantIdentity {
									t.Errorf("survivor descendant changed: %q, %v", current, err)
								}
								if !sp.IsRunning(name) {
									t.Error("surviving group disappeared from runtime state")
								}
							}
							if tc.legacy && (!strings.Contains(stderr.String(), name) || !strings.Contains(stderr.String(), "restart")) {
								t.Error("legacy record must name the session and restart remedy")
							}
							continue
						}
						if code != 0 || !strings.Contains(stdout.String(), "City stopped.") {
							t.Errorf("stale sidecar must not block shutdown: exit=%d", code)
						}
						if tc.child {
							if _, err := proctable.ProcessIdentity(identity.PID); !errors.Is(err, proctable.ErrProcessGone) {
								t.Errorf("child PID %d survived shutdown: %v", identity.PID, err)
							}
						}
						if tc.origin == "desired" && !tc.child {
							key := "gc_process_identity"
							if leaf == "acp" {
								key = "gc_session_owner"
							}
							got, err := sp.GetMeta(name, key)
							if err != nil || got != string(record) {
								t.Errorf("stale configured sidecar changed: %v", err)
							}
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
