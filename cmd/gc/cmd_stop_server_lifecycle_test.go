package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/acp"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
	"github.com/gastownhall/gascity/internal/runtime/k8s"
)

// lifecycleOrderProvider wraps runtime.Fake and additionally implements
// runtime.ServerLifecycleProvider. It records the order of ListRunning and
// TeardownServer calls so ordering tests can verify the stop-path sequence.
type lifecycleOrderProvider struct {
	*runtime.Fake
	mu          sync.Mutex
	events      []string
	teardownErr error
	listErr     error // when set, ListRunning returns it (empty slice), modeling a failed enumeration
	unscoped    bool
	scopeCity   string
}

func (p *lifecycleOrderProvider) ListRunning(prefix string) ([]string, error) {
	p.mu.Lock()
	p.events = append(p.events, "ListRunning")
	listErr := p.listErr
	p.mu.Unlock()
	if listErr != nil {
		return nil, listErr
	}
	return p.Fake.ListRunning(prefix)
}

func (p *lifecycleOrderProvider) ConfigureServer() error {
	return nil
}

func (p *lifecycleOrderProvider) IsCityScoped(city string) bool {
	return !p.unscoped && (p.scopeCity == "" || p.scopeCity == city)
}

func (p *lifecycleOrderProvider) TeardownServer() error {
	p.mu.Lock()
	p.events = append(p.events, "TeardownServer")
	p.mu.Unlock()
	return p.teardownErr
}

// Compile-time assertions: lifecycleOrderProvider satisfies both interfaces.
var (
	_ runtime.Provider                = (*lifecycleOrderProvider)(nil)
	_ runtime.ServerLifecycleProvider = (*lifecycleOrderProvider)(nil)
)

func TestStopCompositeProvidersScopeAndTeardown(t *testing.T) {
	for _, factory := range []struct {
		name string
		new  func(runtime.Provider, runtime.Provider) runtime.Provider
	}{
		{"auto", func(a, b runtime.Provider) runtime.Provider { return auto.New(a, b) }},
		{"hybrid", func(a, b runtime.Provider) runtime.Provider {
			return hybrid.New(a, b, func(string) bool { return false })
		}},
	} {
		t.Run(factory.name, func(t *testing.T) {
			for _, scenario := range []string{"scoped", "first unscoped", "second unscoped", "first unknown", "second unknown", "wrong city", "foreign survivor", "partial listing"} {
				t.Run(scenario, func(t *testing.T) {
					first := &lifecycleOrderProvider{Fake: runtime.NewFake(), scopeCity: "test"}
					second := &lifecycleOrderProvider{Fake: runtime.NewFake(), scopeCity: "test"}
					for _, leaf := range []*lifecycleOrderProvider{first, second} {
						if err := leaf.Start(context.Background(), "test-stale", runtime.Config{}); err != nil {
							t.Fatal(err)
						}
					}
					var a, b runtime.Provider = first, second
					city := "test"
					switch scenario {
					case "first unscoped":
						first.unscoped = true
					case "second unscoped":
						second.unscoped = true
					case "first unknown":
						a = first.Fake
					case "second unknown":
						b = second.Fake
					case "wrong city":
						city = "other"
					case "foreign survivor":
						if err := second.SetMeta("test-stale", "GC_CITY_PATH", "/cities/foreign"); err != nil {
							t.Fatal(err)
						}
					case "partial listing":
						second.listErr = errors.New("backend unavailable")
					}
					provider := factory.new(a, b)
					scope := provider.(interface{ IsCityScoped(string) bool })
					wantScope := scenario == "scoped" || scenario == "foreign survivor" || scenario == "partial listing"
					if scope.IsCityScoped(city) != wantScope {
						t.Errorf("composite scope=%v, want %v", scope.IsCityScoped(city), wantScope)
					}
					cfg := &config.City{Workspace: config.Workspace{Name: city, SessionTemplate: "{{.City}}-{{.Agent}}"}, Agents: []config.Agent{{Name: "worker"}}}
					var stderr lockedBuffer
					stopOrphans("/cities/"+city, provider, map[string]bool{city + "-worker": true}, nil, cfg, sessionFrontDoor(beads.NewMemStore()), 0, events.Discard, io.Discard, &stderr)
					teardownEmptyServerForStop(city, provider, &stderr)
					for _, leaf := range []*lifecycleOrderProvider{first, second} {
						teardowns := 0
						for _, event := range leaf.events {
							if event == "TeardownServer" {
								teardowns++
							}
						}
						wantRunning := scenario == "wrong city" || scenario == "partial listing" ||
							(leaf == first && (scenario == "first unscoped" || scenario == "first unknown")) ||
							(leaf == second && (scenario == "second unscoped" || scenario == "second unknown" || scenario == "foreign survivor"))
						wantTeardown := 0
						if !wantRunning {
							wantTeardown = 1
						}
						if teardowns != wantTeardown {
							t.Errorf("teardowns=%d, want %d; stderr=%q", teardowns, wantTeardown, stderr.String())
						}
						if leaf.IsRunning("test-stale") != wantRunning {
							t.Errorf("orphan running=%v, want %v; stderr=%q", leaf.IsRunning("test-stale"), wantRunning, stderr.String())
						}
					}
				})
			}
		})
	}
}

func TestStopCompositeProvidersWithUnscopedNonServerBackends(t *testing.T) {
	for _, kind := range []string{"auto", "hybrid"} {
		t.Run(kind, func(t *testing.T) {
			local := &lifecycleOrderProvider{Fake: runtime.NewFake(), scopeCity: "test"}
			if err := local.Start(context.Background(), "test-stale", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			var provider runtime.Provider
			if kind == "auto" {
				provider = auto.New(local, acp.NewSeamBackedWithDir(t.TempDir(), acp.Config{}))
			} else {
				provider = hybrid.New(local, &ownedStopLeaf{Fake: runtime.NewFake()}, func(string) bool { return false })
			}
			cfg := &config.City{Workspace: config.Workspace{SessionTemplate: "{{.City}}-{{.Agent}}"}, Agents: []config.Agent{{Name: "worker"}}}
			var stderr lockedBuffer
			stopOrphans("/cities/test", provider, map[string]bool{"test-worker": true}, nil, cfg, sessionFrontDoor(beads.NewMemStore()), 0, events.Discard, io.Discard, &stderr)
			teardownEmptyServerForStop("test", provider, &stderr)
			if local.IsRunning("test-stale") || local.CountCalls("Stop", "test-stale") != 1 {
				t.Errorf("scoped leaf was not stopped: %s", stderr.String())
			}
			teardowns := 0
			for _, event := range local.events {
				if event == "TeardownServer" {
					teardowns++
				}
			}
			if teardowns != 1 {
				t.Errorf("teardowns=%d, want 1: %s", teardowns, stderr.String())
			}
		})
	}
}

// ownedStopLeaf exercises the host-ownership contract of ACP and Kubernetes
// without starting an agent process or connecting to a Kubernetes cluster.
type ownedStopLeaf struct {
	*runtime.Fake
	listErr error
}

var _ interface {
	SessionOwnership(string) (string, string, error)
} = (*k8s.Provider)(nil)

func (p *ownedStopLeaf) SessionOwnership(name string) (string, string, error) {
	if !p.IsRunning(name) {
		return "", "", runtime.ErrSessionNotFound
	}
	city, err := p.GetMeta(name, "GC_CITY_PATH")
	if err != nil {
		return "", "", err
	}
	id, err := p.GetMeta(name, "GC_SESSION_ID")
	return city, id, err
}

func (p *ownedStopLeaf) ListRunning(prefix string) ([]string, error) {
	if p.listErr != nil {
		return nil, p.listErr
	}
	return p.Fake.ListRunning(prefix)
}

type sharedStopLeaf struct {
	*ownedStopLeaf
	unmanaged map[string]bool
	err       error
}

func (p *sharedStopLeaf) IsUnmanagedSession(name string) (bool, error) {
	return p.unmanaged[name], p.err
}

func TestStopOrphansPreservesUnmanagedSessions(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%v", partial), func(t *testing.T) {
			sp := &sharedStopLeaf{ownedStopLeaf: &ownedStopLeaf{Fake: runtime.NewFake()}, unmanaged: map[string]bool{"operator": true}}
			startStopTestSession(t, sp.Fake, "operator", nil)
			startStopTestSession(t, sp.Fake, "owned-orphan", map[string]string{"GC_CITY_PATH": "/cities/test", "GC_SESSION_ID": "deleted"})
			if partial {
				startStopTestSession(t, sp.Fake, "partial", map[string]string{"GC_CITY_PATH": "/cities/test"})
			}
			var stdout, stderr lockedBuffer
			stopped := stopOrphans("/cities/test", sp, nil, nil, &config.City{}, sessionFrontDoor(beads.NewMemStore()), 0, events.Discard, &stdout, &stderr)
			if stopped == partial || sp.IsRunning("owned-orphan") || !sp.IsRunning("operator") || sp.CountCalls("Stop", "operator") != 0 || strings.Contains(stderr.String(), "operator") {
				t.Fatalf("stopped=%v stdout=%q stderr=%q", stopped, stdout.String(), stderr.String())
			}
			if partial && (!sp.IsRunning("partial") || !strings.Contains(stderr.String(), "partial")) {
				t.Fatal("partially identified session must be reported and retained")
			}
		})
	}
	t.Run("unreadable identity", func(t *testing.T) {
		wantErr := errors.New("shared identity unavailable")
		sp := &sharedStopLeaf{ownedStopLeaf: &ownedStopLeaf{Fake: runtime.NewFake()}, err: wantErr}
		startStopTestSession(t, sp.Fake, "operator", nil)
		targets, _, err := ownedOrphanStopTargets(sp, "/cities/test", nil)
		if !errors.Is(err, wantErr) || len(targets) != 0 {
			t.Fatalf("unreadable shared identity: targets=%v error=%v", targets, err)
		}
	})
}

func TestManualStopDoesNotTreatPersistedSessionAsUnmanaged(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{"session_origin": "manual", "session_name": "persisted"}}); err != nil {
		t.Fatal(err)
	}
	sp := &sharedStopLeaf{ownedStopLeaf: &ownedStopLeaf{Fake: runtime.NewFake()}, unmanaged: map[string]bool{"persisted": true}}
	startStopTestSession(t, sp.Fake, "persisted", nil)
	targets, err := resolveManualStopSessionTargets(store, &config.City{}, "/cities/test", sp)
	if err == nil || len(targets) != 0 {
		t.Fatalf("persisted session with missing runtime identity must be reported: targets=%v error=%v", targets, err)
	}
}

func TestCmdStopReportsUnownedSessions(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		force, emptyConfig, allOwned bool
	}{
		{name: "forced upgrade", force: true},
		{name: "graceful upgrade"},
		{name: "empty configuration", force: true, emptyConfig: true},
		{name: "forced owned", force: true, allOwned: true},
		{name: "graceful owned", allOwned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cityDir := t.TempDir()
				cfg := &config.City{
					Workspace: config.Workspace{Name: "test"}, Beads: config.BeadsConfig{Provider: "file"},
					Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}},
					Daemon: config.DaemonConfig{ShutdownTimeout: "0s"},
				}
				if tc.emptyConfig {
					cfg.Agents = nil
				}
				writeStopLifecycleCityConfig(t, cityDir, cfg)
				store, err := openCityStoreAt(cityDir)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{
					"session_origin": "manual", "session_name": "legacy-manual", "state": "active",
				}}); err != nil {
					t.Fatal(err)
				}
				sp := &ownedStopLeaf{Fake: runtime.NewFake()}
				startStopTestSession(t, sp.Fake, "removed-gc-9", map[string]string{"GC_CITY_PATH": cityDir, "GC_SESSION_ID": "deleted-bead"})
				startStopTestSession(t, sp.Fake, "foreign-runtime", map[string]string{"GC_CITY_PATH": cityDir + "-foreign", "GC_SESSION_ID": "foreign-id"})
				if !tc.emptyConfig {
					startStopTestSession(t, sp.Fake, "worker", map[string]string{"GC_CITY_PATH": cityDir, "GC_SESSION_ID": "deleted-worker-bead"})
				}
				if !tc.allOwned {
					startStopTestSession(t, sp.Fake, "legacy-manual", nil)
					startStopTestSession(t, sp.Fake, "legacy-orphan-gc-9", nil)
				}
				oldFactory := sessionProviderForStopCity
				t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
				sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return sp, nil }
				overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
				var stdout, stderr lockedBuffer
				code := cmdStopBody(cityDir, cfg, tc.force, &stdout, &stderr)
				t.Logf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				if (code == 0) != tc.allOwned || strings.Contains(stdout.String(), "City stopped.") != tc.allOwned {
					t.Error("unresolved ownership produced the wrong stop result")
				}
				if sp.CountCalls("Stop", "removed-gc-9") != 1 || sp.IsRunning("removed-gc-9") || sp.IsRunning("worker") {
					t.Error("owned or configured session was not stopped once")
				}
				if sp.CountCalls("Stop", "foreign-runtime") != 0 || !sp.IsRunning("foreign-runtime") || strings.Contains(stderr.String(), "foreign-runtime") {
					t.Error("foreign session was stopped or named")
				}
				if !tc.allOwned {
					if strings.Count(stderr.String(), "carry no ownership record") != 1 {
						t.Error("expected one unresolved-ownership line for the leaf")
					}
					if !strings.Contains(stderr.String(), "gc session kill ID-OR-ALIAS") || strings.Contains(stderr.String(), "gc stop SESSION") {
						t.Error("ownership diagnostic must name the session command, not the city stop command")
					}
					for _, name := range []string{"legacy-manual", "legacy-orphan-gc-9"} {
						if !sp.IsRunning(name) || sp.CountCalls("Stop", name) != 0 || !strings.Contains(stderr.String(), name) {
							t.Errorf("unowned session %s was stopped or not named", name)
						}
					}
				}
			})
		})
	}
}

func TestStopOrphansIncludesOwnedUnscopedLeaves(t *testing.T) {
	for _, kind := range []string{"acp", "kubernetes"} {
		for _, failList := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/listFails=%v", kind, failList), func(t *testing.T) {
				local := &lifecycleOrderProvider{Fake: runtime.NewFake()}
				remote := &ownedStopLeaf{Fake: runtime.NewFake()}
				startStopTestSession(t, local.Fake, "test-stale", nil)
				startStopTestSession(t, remote.Fake, "owned-runtime", map[string]string{"GC_CITY_PATH": "/cities/test", "GC_SESSION_ID": "old-session"})
				startStopTestSession(t, remote.Fake, "foreign-runtime", map[string]string{"GC_CITY_PATH": "/cities/foreign", "GC_SESSION_ID": "other-session"})
				if failList {
					remote.listErr = errors.New("remote enumeration unavailable")
				}
				var sp runtime.Provider = auto.New(local, remote)
				if kind == "kubernetes" {
					sp = hybrid.New(local, remote, func(string) bool { return false })
				}
				cfg := &config.City{Workspace: config.Workspace{Name: "test", SessionTemplate: "{{.City}}-{{.Agent}}"}, Agents: []config.Agent{{Name: "worker"}}}
				var stdout, stderr lockedBuffer
				ok := stopOrphans("/cities/test", sp, map[string]bool{"test-worker": true}, nil, cfg, sessionFrontDoor(beads.NewMemStore()), 0, events.Discard, &stdout, &stderr)
				t.Logf("success=%v owned=%v foreign=%v stderr=%q", ok, remote.IsRunning("owned-runtime"), remote.IsRunning("foreign-runtime"), stderr.String())
				if ok == failList || remote.IsRunning("owned-runtime") != failList || local.IsRunning("test-stale") != failList || !remote.IsRunning("foreign-runtime") {
					t.Fatal("leaf enumeration or ownership was lost")
				}
			})
		}
	}
}

func TestStopOrphansShareGraceWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := &ownedStopLeaf{Fake: runtime.NewFake()}
		var names []string
		for i := range 5 {
			name := fmt.Sprintf("orphan-%d", i)
			names = append(names, name)
			startStopTestSession(t, sp.Fake, name, map[string]string{"GC_CITY_PATH": "/cities/test", "GC_SESSION_ID": name})
		}
		cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
		var stdout, stderr lockedBuffer
		const grace = 5 * time.Second
		started := time.Now()
		ok := stopOrphans("/cities/test", sp, map[string]bool{"configured": true}, nil, cfg, sessionFrontDoor(beads.NewMemStore()), grace, events.Discard, &stdout, &stderr)
		elapsed := time.Since(started)
		t.Logf("five orphans: success=%v fake elapsed=%s grace=%s stderr=%q", ok, elapsed, grace, stderr.String())
		if !ok || elapsed != grace {
			t.Errorf("orphan batch consumed %s, want one %s grace window", elapsed, grace)
		}
		for _, name := range names {
			if sp.IsRunning(name) || sp.CountCalls("Stop", name) != 1 || sp.CountCalls("Interrupt", name) != 1 {
				t.Errorf("%s: running=%v stops=%d interrupts=%d", name, sp.IsRunning(name), sp.CountCalls("Stop", name), sp.CountCalls("Interrupt", name))
			}
		}
	})
}

type vanishingOwnedStopLeaf struct {
	*ownedStopLeaf
	beforeKill bool
}

func (p *vanishingOwnedStopLeaf) SessionOwnership(name string) (string, string, error) {
	if !p.IsRunning(name) {
		return "", "", runtime.ErrSessionNotFound
	}
	return p.ownedStopLeaf.SessionOwnership(name)
}

func (p *vanishingOwnedStopLeaf) Interrupt(name string) error {
	return p.Stop(name)
}

func (p *vanishingOwnedStopLeaf) GetMeta(name, key string) (string, error) {
	if p.beforeKill && key == "GC_PROVIDER" {
		if err := p.Stop(name); err != nil {
			return "", err
		}
	}
	return p.Fake.GetMeta(name, key)
}

func TestStopOwnedOrphanConfirmedAbsenceSucceeds(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(fmt.Sprintf("forced=%v", forced), func(t *testing.T) {
			sp := &vanishingOwnedStopLeaf{ownedStopLeaf: &ownedStopLeaf{Fake: runtime.NewFake()}, beforeKill: forced}
			startStopTestSession(t, sp.Fake, "owned-runtime", map[string]string{"GC_CITY_PATH": "/cities/test", "GC_SESSION_ID": "deleted-record"})
			startStopTestSession(t, sp.Fake, "foreign-runtime", map[string]string{"GC_CITY_PATH": "/cities/foreign", "GC_SESSION_ID": "foreign-record"})
			timeout := time.Second
			if forced {
				timeout = 0
			}
			var stdout, stderr lockedBuffer
			ok := stopOrphans("/cities/test", sp, map[string]bool{"configured": true}, nil, &config.City{}, sessionFrontDoor(beads.NewMemStore()), timeout, events.Discard, &stdout, &stderr)
			t.Logf("success=%v owned=%v foreign=%v stderr=%q", ok, sp.IsRunning("owned-runtime"), sp.IsRunning("foreign-runtime"), stderr.String())
			if !ok || sp.IsRunning("owned-runtime") || !sp.IsRunning("foreign-runtime") || strings.Contains(stderr.String(), "foreign-runtime") {
				t.Fatal("confirmed orphan absence must succeed and preserve foreign sessions")
			}
		})
	}
}

func TestStopOrphanBatchRetainsScopedLifecycleFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := beads.NewMemStore()
		b, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{
			"session_name": "test-stale", "state": "invalid-state",
		}})
		if err != nil {
			t.Fatal(err)
		}
		local := &lifecycleOrderProvider{Fake: runtime.NewFake()}
		remote := &ownedStopLeaf{Fake: runtime.NewFake()}
		startStopTestSession(t, local.Fake, "test-stale", map[string]string{"GC_CITY_PATH": "/cities/test", "GC_SESSION_ID": b.ID})
		startStopTestSession(t, remote.Fake, "remote-orphan", map[string]string{"GC_CITY_PATH": "/cities/test", "GC_SESSION_ID": "deleted-record"})
		sp := hybrid.New(local, remote, func(string) bool { return false })
		cfg := &config.City{Workspace: config.Workspace{Name: "test", SessionTemplate: "{{.City}}-{{.Agent}}"}, Agents: []config.Agent{{Name: "worker"}}}
		var stdout, stderr lockedBuffer
		start := time.Now()
		ok := stopOrphans("/cities/test", sp, map[string]bool{"test-worker": true}, nil, cfg, sessionFrontDoor(store), 5*time.Second, events.Discard, &stdout, &stderr)
		if ok || !local.IsRunning("test-stale") || remote.IsRunning("remote-orphan") || !strings.Contains(stderr.String(), "test-stale") {
			t.Fatalf("success=%v scoped=%v remote=%v stderr=%q", ok, local.IsRunning("test-stale"), remote.IsRunning("remote-orphan"), stderr.String())
		}
		if elapsed := time.Since(start); elapsed != 5*time.Second {
			t.Errorf("mixed backend batch elapsed=%s, want one grace window", elapsed)
		}
	})
}

func TestStopOrphanBatchRefusesAmbiguousNames(t *testing.T) {
	local := &lifecycleOrderProvider{Fake: runtime.NewFake()}
	remote := &ownedStopLeaf{Fake: runtime.NewFake()}
	for _, leaf := range []*runtime.Fake{local.Fake, remote.Fake} {
		startStopTestSession(t, leaf, "test-stale", map[string]string{"GC_CITY_PATH": "/cities/test", "GC_SESSION_ID": "deleted-record"})
	}
	sp := hybrid.New(local, remote, func(string) bool { return false })
	cfg := &config.City{Workspace: config.Workspace{Name: "test", SessionTemplate: "{{.City}}-{{.Agent}}"}, Agents: []config.Agent{{Name: "worker"}}}
	var stdout, stderr lockedBuffer
	ok := stopOrphans("/cities/test", sp, map[string]bool{"test-worker": true}, nil, cfg, sessionFrontDoor(beads.NewMemStore()), 0, events.Discard, &stdout, &stderr)
	if ok || !local.IsRunning("test-stale") || !remote.IsRunning("test-stale") || !strings.Contains(stderr.String(), "test-stale") {
		t.Fatalf("ambiguous backends: success=%v stderr=%q", ok, stderr.String())
	}
}

func TestStopOrphanMetadataStaysOnOwningBackend(t *testing.T) {
	local := &lifecycleOrderProvider{Fake: runtime.NewFake()}
	foreign := &lifecycleOrderProvider{Fake: runtime.NewFake()}
	startStopTestSession(t, local.Fake, "test-stale", map[string]string{"GC_CITY_PATH": "/cities/test"})
	startStopTestSession(t, foreign.Fake, "test-stale", map[string]string{"GC_CITY_PATH": "/cities/foreign", "GC_SESSION_ID": "foreign-session"})
	sp := hybrid.New(local, foreign, func(string) bool { return true })
	cfg := &config.City{Workspace: config.Workspace{Name: "test", SessionTemplate: "{{.City}}-{{.Agent}}"}, Agents: []config.Agent{{Name: "worker"}}}
	var stdout, stderr lockedBuffer
	ok := stopOrphans("/cities/test", sp, map[string]bool{"test-worker": true}, nil, cfg, sessionFrontDoor(beads.NewMemStore()), 0, events.Discard, &stdout, &stderr)
	t.Logf("success=%v owned=%v foreign=%v stderr=%q", ok, local.IsRunning("test-stale"), foreign.IsRunning("test-stale"), stderr.String())
	if !ok || local.IsRunning("test-stale") || !foreign.IsRunning("test-stale") || strings.Contains(stderr.String(), "foreign-session") {
		t.Fatal("orphan handle resolution followed the foreign backend")
	}
}

func TestStopManualAliasedSessions(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty server", true: "foreign survivor"}[foreign], func(t *testing.T) {
			store := beads.NewMemStore()
			manual, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{
				"template": "worker", "agent_name": "sky", "alias": "sky", "session_origin": "manual", "state": "active",
			}})
			if err != nil {
				t.Fatal(err)
			}
			// The persisted identity defaults to s-<beadID>, not the alias or template.
			manualName := "s-" + manual.ID
			sp := &lifecycleOrderProvider{Fake: runtime.NewFake()}
			startStopTestSession(t, sp.Fake, manualName, map[string]string{
				"GC_CITY_PATH": "/cities/test", "GC_AGENT": "sky", "GC_TEMPLATE": "worker", "GC_SESSION_ID": manual.ID,
			})
			if foreign {
				startStopTestSession(t, sp.Fake, "s-foreign", map[string]string{"GC_CITY_PATH": "/cities/foreign"})
			}
			cfg := &config.City{Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(2)}}}
			var stderr lockedBuffer
			names, desired, _, resolveErr := resolveStopSessionNames(store, cfg, "/cities/test", "test", sp, &stderr)
			if resolveErr != nil {
				t.Fatal(resolveErr)
			}
			code := doStopWithoutSuccess(names, sp, cfg, store, 0, events.Discard, io.Discard, &stderr)
			stopOrphans("/cities/test", sp, desired, resolveErr, cfg, sessionFrontDoor(store), 0, events.Discard, io.Discard, &stderr)
			teardownEmptyServerForStop("test", sp, &stderr)
			if code != 0 || sp.IsRunning(manualName) || !desired[manualName] || sp.CountCalls("Stop", manualName) != 1 {
				t.Errorf("manual stop: code=%d running=%v desired=%v stops=%d stderr=%q", code, sp.IsRunning(manualName), desired, sp.CountCalls("Stop", manualName), stderr.String())
			}
			if foreign && (!sp.IsRunning("s-foreign") || sp.CountCalls("Stop", "s-foreign") != 0) {
				t.Error("foreign session was stopped")
			}
			teardowns := 0
			for _, event := range sp.events {
				if event == "TeardownServer" {
					teardowns++
				}
			}
			if (teardowns == 1) == foreign || teardowns > 1 {
				t.Errorf("teardowns=%d foreign=%v; stderr=%q", teardowns, foreign, stderr.String())
			}
		})
	}
}

func TestCmdStopBodyManualSessionKeepsOwningBackend(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"}, Beads: config.BeadsConfig{Provider: "file"},
		Daemon: config.DaemonConfig{ShutdownTimeout: "1ns"},
		Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(2)}},
	}
	writeStopLifecycleCityConfig(t, cityDir, cfg)
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{
		"session_origin": "manual", "session_name": "custom-runtime", "template": "worker", "alias": "sky", "state": "active",
	}})
	if err != nil {
		t.Fatal(err)
	}
	local, remote := runtime.NewFake(), runtime.NewFake()
	startStopTestSession(t, local, "custom-runtime", map[string]string{"GC_CITY_PATH": cityDir, "GC_SESSION_ID": info.ID})
	// A changed remote_match must not redirect shutdown away from the owner.
	provider := hybrid.New(local, remote, func(string) bool { return true })
	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return provider, nil }
	overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
	var stdout, stderr lockedBuffer
	code := cmdStopBody(cityDir, cfg, false, &stdout, &stderr)
	if code != 0 || local.IsRunning("custom-runtime") || local.CountCalls("Stop", "custom-runtime") != 1 || local.CountCalls("Interrupt", "custom-runtime") != 1 {
		t.Errorf("code=%d local running=%v stops=%d interrupts=%d stderr=%s", code, local.IsRunning("custom-runtime"), local.CountCalls("Stop", "custom-runtime"), local.CountCalls("Interrupt", "custom-runtime"), stderr.String())
	}
	if remote.CountCalls("Stop", "custom-runtime") != 0 || remote.CountCalls("Interrupt", "custom-runtime") != 0 {
		t.Error("shutdown reached the newly routed backend instead of the verified owner")
	}
}

func TestCmdStopBodyNamedSessionAndEmptyDesiredSurvivor(t *testing.T) {
	for _, mode := range []string{"named", "named removed template", "named with foreign survivor", "named with owned survivor", "named no agents owned survivor", "empty desired owned survivor", "empty desired foreign survivor", "manual no agents owned survivor", "manual no agents foreign survivor"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cityDir := t.TempDir()
				cfg := &config.City{Workspace: config.Workspace{Name: "test"}, Beads: config.BeadsConfig{Provider: "file"}}
				named := strings.HasPrefix(mode, "named")
				manual := strings.HasPrefix(mode, "manual")
				if named && mode != "named removed template" && !strings.Contains(mode, "no agents") {
					cfg.Agents = []config.Agent{{Name: "worker"}}
				}
				if mode == "named removed template" {
					cfg.Agents = []config.Agent{{Name: "replacement", MaxActiveSessions: intPtr(1)}}
				}
				writeStopLifecycleCityConfig(t, cityDir, cfg)
				store, err := openCityStoreAt(cityDir)
				if err != nil {
					t.Fatal(err)
				}
				sp := &lifecycleOrderProvider{Fake: runtime.NewFake()}
				if named || manual {
					metadata := map[string]string{
						"template": "worker", "agent_name": "sky", "alias": "sky", "session_origin": "named",
						"configured_named_session": "true", "configured_named_identity": "sky",
						"session_name": "persisted-runtime", "state": "active",
					}
					if manual {
						metadata["session_origin"] = "manual"
						delete(metadata, "configured_named_session")
						delete(metadata, "configured_named_identity")
					}
					b, err := store.Create(beads.Bead{Type: "session", Metadata: metadata})
					if err != nil {
						t.Fatal(err)
					}
					startStopTestSession(t, sp.Fake, "persisted-runtime", map[string]string{"GC_CITY_PATH": cityDir, "GC_SESSION_ID": b.ID})
				}
				foreign := strings.Contains(mode, "foreign")
				survivor := strings.Contains(mode, "survivor")
				if survivor {
					owner := cityDir
					if foreign {
						owner = filepath.Join(cityDir, "foreign")
					}
					startStopTestSession(t, sp.Fake, "survivor", map[string]string{"GC_CITY_PATH": owner})
				}
				oldFactory := sessionProviderForStopCity
				t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
				sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return sp, nil }
				overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
				var stdout, stderr lockedBuffer
				code := cmdStopBody(cityDir, cfg, true, &stdout, &stderr)
				t.Logf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				wantFailure := survivor && !foreign
				if (code != 0) != wantFailure || strings.Contains(stdout.String(), "City stopped.") == wantFailure {
					t.Errorf("code=%d stdout=%q stderr=%q; want failure=%v", code, stdout.String(), stderr.String(), wantFailure)
				}
				if (named || manual) && (sp.IsRunning("persisted-runtime") || sp.CountCalls("Stop", "persisted-runtime") != 1) {
					t.Errorf("named session survived stop: running=%v stops=%d", sp.IsRunning("persisted-runtime"), sp.CountCalls("Stop", "persisted-runtime"))
				}
				if survivor {
					if !sp.IsRunning("survivor") || slices.Contains(sp.events, "TeardownServer") {
						t.Error("survivor or its server was destroyed")
					}
				} else if !slices.Contains(sp.events, "TeardownServer") {
					t.Error("empty named-session server was not torn down")
				}
				if wantFailure && !strings.Contains(stderr.String(), "survivor") {
					t.Errorf("missing survivor diagnostic: %q", stderr.String())
				}
			})
		})
	}
}

// terminatingStopLeaf models accepted deletion: the process is no longer
// running, but its runtime remains listed and owned until teardown completes.
type terminatingStopLeaf struct {
	*ownedStopLeaf
	mu          sync.Mutex
	delay       time.Duration
	terminating map[string]time.Time
	keepProcess bool
}

func (p *terminatingStopLeaf) IsRunning(name string) bool {
	if p.keepProcess {
		names, _ := p.ListRunning("")
		return slices.Contains(names, name)
	}
	return p.Fake.IsRunning(name)
}

func (p *terminatingStopLeaf) Stop(name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.terminating[name]; !exists {
		p.terminating[name] = time.Now()
	}
	return p.Fake.Stop(name)
}

func (p *terminatingStopLeaf) ListRunning(prefix string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	names, err := p.Fake.ListRunning(prefix)
	for name, started := range p.terminating {
		if p.delay < 0 || time.Since(started) < p.delay {
			if strings.HasPrefix(name, prefix) {
				names = append(names, name)
			}
		}
	}
	return names, err
}

func (p *terminatingStopLeaf) SessionOwnership(name string) (string, string, error) {
	names, err := p.ListRunning("")
	if err != nil {
		return "", "", err
	}
	if !slices.Contains(names, name) {
		return "", "", runtime.ErrSessionNotFound
	}
	return stopSessionOwnership(p.Fake, name)
}

type pendingTerminatingStopLeaf struct{ *terminatingStopLeaf }

func (*pendingTerminatingStopLeaf) IsRunning(string) bool { return false }

func (p *pendingTerminatingStopLeaf) ListRunning(prefix string) ([]string, error) {
	_, err := p.terminatingStopLeaf.ListRunning(prefix)
	return nil, err
}

func TestCmdStopWaitsForPendingArtifactDeletion(t *testing.T) {
	for _, delay := range []time.Duration{3 * time.Second, -1} {
		t.Run(delay.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				oldBound := stopPerTargetTimeoutDefault
				stopPerTargetTimeoutDefault = 4 * time.Second
				t.Cleanup(func() { stopPerTargetTimeoutDefault = oldBound })
				cityDir := t.TempDir()
				cfg := &config.City{Workspace: config.Workspace{Name: "test"}, Beads: config.BeadsConfig{Provider: "file"}}
				writeStopLifecycleCityConfig(t, cityDir, cfg)
				store, err := openCityStoreAt(cityDir)
				if err != nil {
					t.Fatal(err)
				}
				b, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{"session_origin": "manual", "session_name": "pending", "state": "active"}})
				if err != nil {
					t.Fatal(err)
				}
				sp := &pendingTerminatingStopLeaf{&terminatingStopLeaf{ownedStopLeaf: &ownedStopLeaf{Fake: runtime.NewFake()}, delay: delay, terminating: make(map[string]time.Time)}}
				startStopTestSession(t, sp.Fake, "pending", map[string]string{"GC_CITY_PATH": cityDir, "GC_SESSION_ID": b.ID})
				oldFactory := sessionProviderForStopCity
				t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
				sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return sp, nil }
				overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
				var stdout, stderr lockedBuffer
				code := cmdStopBody(cityDir, cfg, true, &stdout, &stderr)
				wantWait := delay
				if delay < 0 {
					wantWait = stopPerTargetTimeoutDefault
				}
				if elapsed := time.Since(sp.terminating["pending"]); elapsed != wantWait || (code != 0) != (delay < 0) {
					t.Fatalf("pending teardown code=%d elapsed=%s want=%s stdout=%q stderr=%q", code, elapsed, wantWait, stdout.String(), stderr.String())
				}
				if delay < 0 && (!strings.Contains(stderr.String(), "pending") || strings.Contains(stdout.String(), "City stopped.")) {
					t.Fatal("pending artifact with a finalizer was not reported")
				}
			})
		})
	}
}

func TestCmdStopWaitsForAcceptedTeardown(t *testing.T) {
	for _, tc := range []struct {
		name              string
		force             bool
		delay             time.Duration
		pool, keepProcess bool
	}{
		{name: "forced", force: true, delay: 3 * time.Second},
		{name: "graceful", delay: 3 * time.Second},
		{name: "survivor", force: true, delay: -1},
		{name: "synchronous", force: true},
		{name: "pool process gone", delay: 5 * time.Second, pool: true},
		{name: "pool process alive", delay: 5 * time.Second, pool: true, keepProcess: true},
		{name: "pool survivor", delay: -1, pool: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				oldBound := stopPerTargetTimeoutDefault
				stopPerTargetTimeoutDefault = 4 * time.Second
				if tc.pool {
					stopPerTargetTimeoutDefault = oldBound
				}
				t.Cleanup(func() { stopPerTargetTimeoutDefault = oldBound })
				cityDir := t.TempDir()
				cfg := &config.City{
					Workspace: config.Workspace{Name: "test"}, Beads: config.BeadsConfig{Provider: "file"},
					Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}},
					Daemon: config.DaemonConfig{ShutdownTimeout: "1s"},
				}
				if tc.pool {
					cfg.Agents[0].MaxActiveSessions = intPtr(2)
				}
				writeStopLifecycleCityConfig(t, cityDir, cfg)
				store, err := openCityStoreAt(cityDir)
				if err != nil {
					t.Fatal(err)
				}
				metadata := map[string]string{
					"session_origin": "manual", "session_name": "persisted-runtime", "state": "active",
				}
				if tc.pool {
					metadata["session_origin"], metadata["template"], metadata["pool_slot"] = "ephemeral", "worker", "1"
				}
				b, err := store.Create(beads.Bead{Type: "session", Labels: []string{"gc:session"}, Metadata: metadata})
				if err != nil {
					t.Fatal(err)
				}
				local := &lifecycleOrderProvider{Fake: runtime.NewFake()}
				remote := &terminatingStopLeaf{ownedStopLeaf: &ownedStopLeaf{Fake: runtime.NewFake()}, delay: tc.delay, terminating: make(map[string]time.Time)}
				remote.keepProcess = tc.keepProcess
				startStopTestSession(t, remote.Fake, "persisted-runtime", map[string]string{"GC_CITY_PATH": cityDir, "GC_SESSION_ID": b.ID})
				if tc.pool {
					_, desired, _, err := resolveStopSessionNames(store, cfg, cityDir, "test", remote, io.Discard)
					if err != nil || !desired["persisted-runtime"] {
						t.Fatalf("pool target not discovered: %v %v", desired, err)
					}
				}
				startStopTestSession(t, remote.Fake, "foreign-runtime", map[string]string{"GC_CITY_PATH": cityDir + "-foreign", "GC_SESSION_ID": "foreign-record"})
				sp := hybrid.New(local, remote, func(string) bool { return true })
				oldFactory := sessionProviderForStopCity
				t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
				sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return sp, nil }
				overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
				var stdout, stderr lockedBuffer
				code := cmdStopBody(cityDir, cfg, tc.force, &stdout, &stderr)
				elapsed := time.Since(remote.terminating["persisted-runtime"])
				wantFailure := tc.delay < 0
				wantWait := tc.delay
				if wantFailure {
					wantWait = stopPerTargetTimeoutDefault
					if tc.pool {
						wantWait += time.Second
					}
				}
				t.Logf("code=%d teardown elapsed=%s stdout=%q stderr=%q", code, elapsed, stdout.String(), stderr.String())
				if (code != 0) != wantFailure || strings.Contains(stdout.String(), "City stopped.") == wantFailure {
					t.Error("accepted teardown produced the wrong stop result")
				}
				if elapsed != wantWait {
					t.Errorf("teardown wait = %s, want %s", elapsed, wantWait)
				}
				if tc.pool && remote.CountCalls("Stop", "persisted-runtime") != 1 {
					t.Errorf("pool stop count = %d; want one", remote.CountCalls("Stop", "persisted-runtime"))
				}
				if wantFailure && !strings.Contains(stderr.String(), `city-owned session "persisted-runtime" remains after shutdown`) {
					t.Error("survivor was not named")
				}
				if !remote.IsRunning("foreign-runtime") || strings.Contains(stderr.String(), "foreign-runtime") || remote.CountCalls("Stop", "foreign-runtime") != 0 {
					t.Error("foreign runtime was stopped, waited on, or named")
				}
			})
		})
	}
}

type vanishedOrphanLeaf struct {
	*ownedStopLeaf
	ownershipErr error
}

func (p *vanishedOrphanLeaf) SessionOwnership(name string) (string, string, error) {
	if name == "vanished" {
		return "", "", p.ownershipErr
	}
	if !p.IsRunning(name) {
		return "", "", runtime.ErrSessionNotFound
	}
	return p.ownedStopLeaf.SessionOwnership(name)
}

func TestStopOrphansContinuePastVanishedOrphan(t *testing.T) {
	for _, mode := range []string{"sweep", "command", "unreadable", "joined error"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cityDir := t.TempDir()
				cfg := &config.City{Workspace: config.Workspace{Name: "test"}, Beads: config.BeadsConfig{Provider: "file"}, Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}}}
				sp := &vanishedOrphanLeaf{ownedStopLeaf: &ownedStopLeaf{Fake: runtime.NewFake()}, ownershipErr: runtime.ErrSessionNotFound}
				refused := mode == "unreadable" || mode == "joined error"
				if refused {
					sp.ownershipErr = errors.New("ownership unreadable")
					if mode == "joined error" {
						sp.ownershipErr = errors.Join(runtime.ErrSessionNotFound, sp.ownershipErr)
					}
				}
				for _, name := range []string{"orphan-a", "vanished", "orphan-z", "foreign-runtime"} {
					owner := cityDir
					if name == "foreign-runtime" {
						owner += "-foreign"
					}
					startStopTestSession(t, sp.Fake, name, map[string]string{"GC_CITY_PATH": owner, "GC_SESSION_ID": "deleted-" + name})
				}
				var stdout, stderr lockedBuffer
				var ok bool
				if mode == "command" {
					writeStopLifecycleCityConfig(t, cityDir, cfg)
					oldFactory := sessionProviderForStopCity
					t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
					sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return sp, nil }
					overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
					code := cmdStopBody(cityDir, cfg, true, &stdout, &stderr)
					ok = code == 0 && strings.Contains(stdout.String(), "City stopped.")
				} else {
					ok = stopOrphans(cityDir, sp, map[string]bool{"configured": true}, nil, cfg, sessionFrontDoor(beads.NewMemStore()), 0, events.Discard, &stdout, &stderr)
				}
				t.Logf("success=%v stdout=%q stderr=%q", ok, stdout.String(), stderr.String())
				if ok == refused || strings.Contains(stderr.String(), "vanished") != refused {
					t.Error("ownership absence/error produced the wrong sweep result")
				}
				for _, name := range []string{"orphan-a", "orphan-z"} {
					wantStops := 1
					if refused {
						wantStops = 0
					}
					if sp.IsRunning(name) != refused || sp.CountCalls("Stop", name) != wantStops {
						t.Errorf("%s running=%v stops=%d", name, sp.IsRunning(name), sp.CountCalls("Stop", name))
					}
				}
				if sp.CountCalls("Stop", "vanished") != 0 || sp.CountCalls("Stop", "foreign-runtime") != 0 || !sp.IsRunning("foreign-runtime") || strings.Contains(stderr.String(), "foreign-runtime") {
					t.Error("vanished or foreign runtime was targeted")
				}
			})
		})
	}
}

type unreadableOrphanProvider struct {
	*lifecycleOrderProvider
	cityPath string
}

func (p unreadableOrphanProvider) GetAllEnvironment(name string) (map[string]string, error) {
	if name == "test-unreadable" {
		return nil, errors.New("ownership record unreadable")
	}
	return map[string]string{"GC_CITY_PATH": p.cityPath}, nil
}

func TestCmdStopBodyRefusedOrphanSweepFails(t *testing.T) {
	synctest.Test(t, testCmdStopBodyRefusedOrphanSweepFails)
}

func testCmdStopBodyRefusedOrphanSweepFails(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", SessionTemplate: "{{.City}}-{{.Agent}}"},
		Beads:     config.BeadsConfig{Provider: "file"}, Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}},
	}
	writeStopLifecycleCityConfig(t, cityDir, cfg)
	sp := unreadableOrphanProvider{&lifecycleOrderProvider{Fake: runtime.NewFake()}, cityDir}
	for _, name := range []string{"test-stale", "test-unreadable"} {
		startStopTestSession(t, sp.Fake, name, nil)
	}
	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return sp, nil }
	overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
	var stdout, stderr lockedBuffer
	code := cmdStopBody(cityDir, cfg, true, &stdout, &stderr)
	t.Logf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	if code != 1 || strings.Contains(stdout.String(), "City stopped.") || !strings.Contains(stderr.String(), "test-unreadable") {
		t.Fatal("refused sweep must fail and name the unreadable runtime")
	}
	if !strings.Contains(stderr.String(), `city-owned session "test-stale" remains`) {
		t.Fatal("refused sweep must also verify survivors with a nonempty desired set")
	}
	for _, name := range []string{"test-stale", "test-unreadable"} {
		if !sp.IsRunning(name) || sp.CountCalls("Stop", name) != 0 {
			t.Errorf("refused sweep touched %s", name)
		}
	}
}

func TestCmdStopBodyPendingManualArtifact(t *testing.T) {
	for _, mode := range []string{"owned", "unowned", "delete refused"} {
		t.Run(mode, func(t *testing.T) {
			cityDir := t.TempDir()
			cfg := &config.City{Workspace: config.Workspace{Name: "test"}, Beads: config.BeadsConfig{Provider: "file"}}
			writeStopLifecycleCityConfig(t, cityDir, cfg)
			store, err := openCityStoreAt(cityDir)
			if err != nil {
				t.Fatal(err)
			}
			b, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{
				"session_origin": "manual", "session_name": "pending-runtime", "state": "active",
			}})
			if err != nil {
				t.Fatal(err)
			}
			// Pending Kubernetes pods retain identity but are absent from both
			// ListRunning and IsRunning. No controller is started in this city.
			sp := &stopUnlistedRuntimeProvider{Fake: runtime.NewFake(), hidden: true}
			env := map[string]string{"GC_CITY_PATH": cityDir, "GC_SESSION_ID": b.ID}
			if mode == "unowned" {
				env = nil
			}
			startStopTestSession(t, sp.Fake, "pending-runtime", env)
			if mode == "delete refused" {
				sp.StopErrors["pending-runtime"] = errors.New("pending pod deletion refused")
			}
			oldFactory := sessionProviderForStopCity
			t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
			sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return sp, nil }
			overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
			var stdout, stderr lockedBuffer
			code := cmdStopBody(cityDir, cfg, true, &stdout, &stderr)
			wantFailure := mode != "owned"
			if (code != 0) != wantFailure || sp.Fake.IsRunning("pending-runtime") != wantFailure {
				t.Fatalf("code=%d artifact=%v stderr=%q; want failure=%v", code, sp.Fake.IsRunning("pending-runtime"), stderr.String(), wantFailure)
			}
			if mode == "unowned" && (sp.CountCalls("Stop", "pending-runtime") != 0 || !strings.Contains(stderr.String(), "ownership")) {
				t.Fatalf("unowned artifact must be reported and untouched: stderr=%q", stderr.String())
			}
			if mode == "delete refused" && !strings.Contains(stderr.String(), "pending pod deletion refused") {
				t.Fatalf("deletion failure lost: %q", stderr.String())
			}
		})
	}
}

func TestCmdStopManualWithEmptyACPSibling(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}, Beads: config.BeadsConfig{Provider: "file"}}
	writeStopLifecycleCityConfig(t, cityDir, cfg)
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{
		"session_origin": "manual", "session_name": "manual-runtime", "state": "active",
	}})
	if err != nil {
		t.Fatal(err)
	}
	local := &ownedStopLeaf{Fake: runtime.NewFake()}
	startStopTestSession(t, local.Fake, "manual-runtime", map[string]string{"GC_CITY_PATH": cityDir, "GC_SESSION_ID": b.ID})
	provider := auto.New(local, acp.NewSeamBackedWithDir(t.TempDir(), acp.Config{}))
	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return provider, nil }
	overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
	for range 2 {
		var stdout, stderr lockedBuffer
		if code := cmdStopBody(cityDir, cfg, true, &stdout, &stderr); code != 0 || local.IsRunning("manual-runtime") {
			t.Fatalf("stop with empty ACP sibling: code=%d running=%v stderr=%q", code, local.IsRunning("manual-runtime"), stderr.String())
		}
	}
}

func TestCmdStopReportsPendingManualBackendConflict(t *testing.T) {
	for _, identity := range []string{"owned", "unknown", "foreign"} {
		for _, pendingFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/pending-first=%v", identity, pendingFirst), func(t *testing.T) {
				cityDir := t.TempDir()
				cfg := &config.City{Workspace: config.Workspace{Name: "test"}, Beads: config.BeadsConfig{Provider: "file"}}
				writeStopLifecycleCityConfig(t, cityDir, cfg)
				store, err := openCityStoreAt(cityDir)
				if err != nil {
					t.Fatal(err)
				}
				b, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{
					"session_origin": "manual", "session_name": "pending-runtime", "state": "active",
				}})
				if err != nil {
					t.Fatal(err)
				}
				pending := &stopUnlistedRuntimeProvider{Fake: runtime.NewFake(), hidden: true}
				env := map[string]string{"GC_CITY_PATH": cityDir, "GC_SESSION_ID": b.ID}
				switch identity {
				case "unknown":
					env = nil
				case "foreign":
					env["GC_CITY_PATH"] = t.TempDir()
				}
				startStopTestSession(t, pending.Fake, "pending-runtime", env)
				foreign := &ownedStopLeaf{Fake: runtime.NewFake()}
				startStopTestSession(t, foreign.Fake, "pending-runtime", map[string]string{
					"GC_CITY_PATH": t.TempDir(), "GC_SESSION_ID": "foreign-session",
				})
				provider := auto.New(foreign, pending)
				if pendingFirst {
					provider = auto.New(pending, foreign)
				}
				oldFactory := sessionProviderForStopCity
				t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
				sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return provider, nil }
				overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
				var stdout, stderr lockedBuffer
				code := cmdStopBody(cityDir, cfg, true, &stdout, &stderr)
				if code != 1 || strings.Contains(stdout.String(), "City stopped.") || !strings.Contains(stderr.String(), "pending-runtime") {
					t.Fatalf("hidden conflicting artifact must be reported: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				for _, leaf := range []*runtime.Fake{pending.Fake, foreign.Fake} {
					if !leaf.IsRunning("pending-runtime") || leaf.CountCalls("Stop", "pending-runtime") != 0 {
						t.Fatal("ambiguous runtime was stopped")
					}
				}
			})
		}
	}
}

func TestCmdStopBodyManualDiscoveryOutcome(t *testing.T) {
	for _, mode := range []string{"absent sibling", "unreadable owner", "unverified owner", "healthy terminal"} {
		t.Run(mode, func(t *testing.T) {
			cityDir := t.TempDir()
			cfg := &config.City{Workspace: config.Workspace{Name: "test"}, Beads: config.BeadsConfig{Provider: "file"}, Agents: []config.Agent{{Name: "worker"}}}
			writeStopLifecycleCityConfig(t, cityDir, cfg)
			store, err := openCityStoreAt(cityDir)
			if err != nil {
				t.Fatal(err)
			}
			b, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{
				"session_origin": "manual", "session_name": "manual-runtime", "state": "active",
			}})
			if err != nil {
				t.Fatal(err)
			}
			live := runtime.NewFake()
			env := map[string]string{"GC_CITY_PATH": cityDir, "GC_SESSION_ID": b.ID}
			startStopTestSession(t, live, "manual-runtime", env)
			owner := &stopOwnershipBatchProvider{Provider: live, env: env}
			var provider runtime.Provider = owner
			wantFailure := false
			switch mode {
			case "absent sibling", "unverified owner":
				absent := &stopOwnershipBatchProvider{
					Provider: runtime.NewFake(), err: runtime.ErrSessionNotFound,
					listErr: &runtime.PartialListError{Err: errors.New("no tmux server running"), ServerAbsent: true},
				}
				provider = auto.New(absent, owner)
				if mode == "unverified owner" {
					owner.err = errors.New("ownership unavailable")
					wantFailure = true
				}
			case "unreadable owner":
				owner.listErr = errors.New("listing unavailable")
				wantFailure = true
			}
			oldFactory := sessionProviderForStopCity
			t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
			sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return provider, nil }
			overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
			var stdout, stderr lockedBuffer
			code := cmdStopBody(cityDir, cfg, true, &stdout, &stderr)
			if (code != 0) != wantFailure || live.IsRunning("manual-runtime") != wantFailure || strings.Contains(stdout.String(), "City stopped.") == wantFailure {
				t.Errorf("code=%d running=%v stdout=%q stderr=%q; want failure=%v", code, live.IsRunning("manual-runtime"), stdout.String(), stderr.String(), wantFailure)
			}
			if wantFailure && (!strings.Contains(stderr.String(), "orphan sweep skipped") || !strings.Contains(stderr.String(), "manual")) {
				t.Errorf("unresolved discovery must name the skipped cleanup and manual target: %q", stderr.String())
			}
		})
	}
}

func TestCmdStopBodyManualCodexInterruptContract(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("escape_timeout=%v", fallback), func(t *testing.T) {
			cityDir := t.TempDir()
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test"}, Beads: config.BeadsConfig{Provider: "file"},
				Daemon: config.DaemonConfig{ShutdownTimeout: "1ns"},
				Agents: []config.Agent{{Name: "db", MaxActiveSessions: intPtr(1)}, {Name: "worker", MaxActiveSessions: intPtr(2)}},
			}
			writeStopLifecycleCityConfig(t, cityDir, cfg)
			store, err := openCityStoreAt(cityDir)
			if err != nil {
				t.Fatal(err)
			}
			local, remote := runtime.NewFake(), runtime.NewFake()
			for _, target := range []struct{ name, template, origin string }{
				{"db", "db", "named"}, {"custom-runtime", "worker", "manual"},
			} {
				info, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{
					"session_origin": target.origin, "session_name": target.name, "template": target.template,
					"state": "active", "provider": "codex",
				}})
				if err != nil {
					t.Fatal(err)
				}
				startStopTestSession(t, local, target.name, map[string]string{"GC_CITY_PATH": cityDir, "GC_SESSION_ID": info.ID})
				if fallback {
					local.WaitForIdleSequence[target.name] = []error{context.DeadlineExceeded, nil}
				}
			}
			provider := hybrid.New(local, remote, func(name string) bool { return name == "custom-runtime" })
			oldFactory := sessionProviderForStopCity
			t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
			sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return provider, nil }
			overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
			var stdout, stderr lockedBuffer
			if code := cmdStopBody(cityDir, cfg, false, &stdout, &stderr); code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			waits, interrupts := 1, 0
			if fallback {
				waits, interrupts = 2, 1
			}
			for _, name := range []string{"db", "custom-runtime"} {
				for method, count := range map[string]int{"SendKeys": 1, "WaitForIdle": waits, "WaitForInterruptBoundary": 1, "Interrupt": interrupts, "Stop": 1} {
					if got := local.CountCalls(method, name); got != count {
						t.Errorf("owner %s(%s) calls=%d, want %d; stderr=%s", method, name, got, count, stderr.String())
					}
					if got := remote.CountCalls(method, name); got != 0 {
						t.Errorf("unverified backend received %s(%s) %d times", method, name, got)
					}
				}
			}
		})
	}
}

func TestCmdStopBodyManualWrapperPreservesLegacyACPRoute(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"}, Beads: config.BeadsConfig{Provider: "file"},
		Daemon: config.DaemonConfig{ShutdownTimeout: "1ns"},
		Agents: []config.Agent{{Name: "db", MaxActiveSessions: intPtr(1)}, {Name: "worker", MaxActiveSessions: intPtr(2)}},
	}
	writeStopLifecycleCityConfig(t, cityDir, cfg)
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatal(err)
	}
	local, acpBackend := runtime.NewFake(), runtime.NewFake()
	for _, target := range []struct {
		name, template, origin string
		sp                     *runtime.Fake
	}{
		{"db", "db", "named", acpBackend}, {"custom-runtime", "worker", "manual", local},
	} {
		info, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{
			"session_origin": target.origin, "session_name": target.name, "template": target.template,
			"state": "active", "provider": "claude",
		}})
		if err != nil {
			t.Fatal(err)
		}
		startStopTestSession(t, target.sp, target.name, map[string]string{"GC_CITY_PATH": cityDir, "GC_SESSION_ID": info.ID})
	}
	// The current default is local, while the legacy configured runtime is ACP.
	provider := auto.New(local, acpBackend)
	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return provider, nil }
	overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
	var stdout, stderr lockedBuffer
	if code := cmdStopBody(cityDir, cfg, false, &stdout, &stderr); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if acpBackend.CountCalls("Interrupt", "db") != 1 || local.CountCalls("Interrupt", "db") != 0 || acpBackend.IsRunning("db") {
		t.Fatalf("ACP interrupts=%d local interrupts=%d running=%v; stderr=%s", acpBackend.CountCalls("Interrupt", "db"), local.CountCalls("Interrupt", "db"), acpBackend.IsRunning("db"), stderr.String())
	}
}

type explicitShutdownProvider struct {
	*runtime.Fake
	shutdownCalls int
}

func (p *explicitShutdownProvider) Stop(string) error { return nil } //nolint:unparam // models a provider that deliberately preserves ordinary stops

func (p *explicitShutdownProvider) StopForShutdown(name string) error {
	p.shutdownCalls++
	return p.Fake.Stop(name)
}

func TestCmdStopBodyManualUsesExplicitShutdown(t *testing.T) {
	for _, state := range []string{"active", "suspended", "creating"} {
		for _, force := range []bool{false, true} {
			t.Run(fmt.Sprintf("state=%s/force=%v", state, force), func(t *testing.T) {
				cityDir := t.TempDir()
				cfg := &config.City{
					Workspace: config.Workspace{Name: "test"}, Beads: config.BeadsConfig{Provider: "file"},
					Daemon: config.DaemonConfig{ShutdownTimeout: "1ns"},
					Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(2)}},
				}
				writeStopLifecycleCityConfig(t, cityDir, cfg)
				store, err := openCityStoreAt(cityDir)
				if err != nil {
					t.Fatal(err)
				}
				info, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{
					"session_origin": "manual", "session_name": "manual-runtime", "template": "worker", "state": state,
				}})
				if err != nil {
					t.Fatal(err)
				}
				provider := &explicitShutdownProvider{Fake: runtime.NewFake()}
				startStopTestSession(t, provider.Fake, "manual-runtime", map[string]string{"GC_CITY_PATH": cityDir, "GC_SESSION_ID": info.ID})
				oldFactory := sessionProviderForStopCity
				t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
				sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return provider, nil }
				overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
				var stdout, stderr lockedBuffer
				if code := cmdStopBody(cityDir, cfg, force, &stdout, &stderr); code != 0 {
					t.Fatalf("code=%d stderr=%s", code, stderr.String())
				}
				if provider.IsRunning("manual-runtime") || provider.shutdownCalls != 1 {
					t.Fatalf("manual running=%v shutdown calls=%d; stderr=%s", provider.IsRunning("manual-runtime"), provider.shutdownCalls, stderr.String())
				}
				var operations []string
				for _, call := range provider.SnapshotCalls() {
					if call.Name == "manual-runtime" && (call.Method == "Interrupt" || call.Method == "Stop") {
						operations = append(operations, call.Method)
					}
				}
				wantOperations := "[Interrupt Stop]"
				if force {
					wantOperations = "[Stop]"
				}
				if got := fmt.Sprint(operations); got != wantOperations {
					t.Fatalf("shutdown operations=%s, want %s", got, wantOperations)
				}
			})
		}
	}
}

type dependencyStopProvider struct {
	*runtime.Fake
	record func(string)
}

func (p dependencyStopProvider) Stop(name string) error {
	p.record("stop " + name)
	return p.Fake.Stop(name)
}

func (p dependencyStopProvider) Interrupt(name string) error {
	p.record("interrupt " + name)
	return p.Fake.Interrupt(name)
}

func TestCmdStopBodyManualDependencyOrder(t *testing.T) {
	for _, tc := range []struct {
		name            string
		manualDB        bool
		legacyDB        bool
		removedTemplate bool
		projectedDB     bool
	}{
		{name: "configured dependency"},
		{name: "manual dependency", manualDB: true},
		{name: "legacy manual dependency", manualDB: true, legacyDB: true},
		{name: "removed manual template", removedTemplate: true},
		{name: "projected manual name", manualDB: true, projectedDB: true},
	} {
		for _, force := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/force=%v", tc.name, force), func(t *testing.T) {
				cityDir := t.TempDir()
				cfg := &config.City{
					Workspace: config.Workspace{Name: "test"}, Beads: config.BeadsConfig{Provider: "file"},
					Daemon: config.DaemonConfig{ShutdownTimeout: "1ns"},
					Agents: []config.Agent{{Name: "db", MaxActiveSessions: intPtr(1)}, {Name: "worker", MaxActiveSessions: intPtr(1), DependsOn: []string{"db"}}},
				}
				if tc.legacyDB || tc.projectedDB {
					cfg.Agents[0].MaxActiveSessions = intPtr(2)
				}
				writeStopLifecycleCityConfig(t, cityDir, cfg)
				store, err := openCityStoreAt(cityDir)
				if err != nil {
					t.Fatal(err)
				}
				var mu sync.Mutex
				var calls []string
				record := func(call string) { mu.Lock(); defer mu.Unlock(); calls = append(calls, call) }
				local := dependencyStopProvider{Fake: runtime.NewFake(), record: record}
				remote := dependencyStopProvider{Fake: runtime.NewFake(), record: record}
				dbName := "db"
				targets := []struct {
					name     string
					manual   bool
					provider *runtime.Fake
				}{{"db", tc.manualDB, local.Fake}, {"worker", true, remote.Fake}, {"retired", true, local.Fake}}
				if !tc.removedTemplate {
					targets = targets[:2]
				}
				for _, target := range targets {
					runtimeName := target.name
					env := map[string]string{"GC_CITY_PATH": cityDir}
					if target.manual {
						origin := "manual"
						if tc.legacyDB && target.name == "db" {
							origin = "ephemeral"
						}
						metadata := map[string]string{
							"session_origin": origin, "session_name": target.name, "template": target.name, "state": "active",
						}
						if tc.projectedDB && target.name == "db" {
							delete(metadata, "session_name")
						}
						info, err := store.Create(beads.Bead{Type: "session", Metadata: metadata})
						if err != nil {
							t.Fatal(err)
						}
						env["GC_SESSION_ID"] = info.ID
						if tc.projectedDB && target.name == "db" {
							runtimeName = sessionNameFromBeadID(info.ID)
							dbName = runtimeName
						}
					} else {
						env["GC_SESSION_ID"] = "deleted-" + target.name
					}
					startStopTestSession(t, target.provider, runtimeName, env)
				}
				provider := hybrid.New(local, remote, func(name string) bool { return name == "worker" })
				oldFactory := sessionProviderForStopCity
				t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
				sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return provider, nil }
				overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
				var stdout, stderr lockedBuffer
				if code := cmdStopBody(cityDir, cfg, force, &stdout, &stderr); code != 0 {
					t.Fatalf("code=%d stderr=%s", code, stderr.String())
				}
				mu.Lock()
				defer mu.Unlock()
				workerStop, dbStop := slices.Index(calls, "stop worker"), slices.Index(calls, "stop "+dbName)
				if workerStop < 0 || dbStop < 0 || workerStop >= dbStop {
					t.Fatalf("shutdown must stop dependent worker before db: %v", calls)
				}
				if !force && (slices.Index(calls, "interrupt worker") < 0 || slices.Index(calls, "interrupt worker") >= dbStop || slices.Index(calls, "interrupt "+dbName) < 0) {
					t.Fatalf("all sessions must receive the shared grace window before dependencies stop: %v", calls)
				}
			})
		}
	}
}

func TestCmdStopBodyPreservesForeignSessionsThroughTeardown(t *testing.T) {
	for _, listFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreign survivor", true: "unreadable server"}[listFails], func(t *testing.T) {
			cityDir := t.TempDir()
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test"},
				Beads:     config.BeadsConfig{Provider: "file"},
				Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
				Agents:    []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}},
			}
			writeStopLifecycleCityConfig(t, cityDir, cfg)
			store, err := openCityStoreAt(cityDir)
			if err != nil {
				t.Fatal(err)
			}
			workerRecord, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{
				"session_name": "worker", "template": "worker", "state": "active",
			}})
			if err != nil {
				t.Fatal(err)
			}
			sp := &lifecycleOrderProvider{Fake: runtime.NewFake()}
			for _, name := range []string{"worker", "foreign"} {
				if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
					t.Fatal(err)
				}
			}
			for key, value := range map[string]string{"GC_CITY_PATH": cityDir, "GC_SESSION_ID": workerRecord.ID} {
				if err := sp.SetMeta("worker", key, value); err != nil {
					t.Fatal(err)
				}
			}
			if err := sp.SetMeta("foreign", "GC_CITY_PATH", "/cities/foreign"); err != nil {
				t.Fatal(err)
			}
			if listFails {
				sp.listErr = errors.New("server listing unavailable")
			}
			oldFactory := sessionProviderForStopCity
			t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
			sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return sp, nil }
			overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
			var stdout, stderr lockedBuffer
			code := cmdStopBody(cityDir, cfg, false, &stdout, &stderr)
			if (code != 0) != listFails || sp.IsRunning("worker") || !sp.IsRunning("foreign") {
				t.Fatalf("stop code=%d worker=%v foreign=%v stderr=%q", code, sp.IsRunning("worker"), sp.IsRunning("foreign"), stderr.String())
			}
			sp.mu.Lock()
			defer sp.mu.Unlock()
			for _, event := range sp.events {
				if event == "TeardownServer" {
					t.Fatal("server teardown would kill the preserved foreign session")
				}
			}
		})
	}
}

func TestTeardownEmptyServerForStopRequiresScope(t *testing.T) {
	sp := &lifecycleOrderProvider{Fake: runtime.NewFake(), unscoped: true}
	var stderr lockedBuffer
	teardownEmptyServerForStop("test", sp, &stderr)
	if !strings.Contains(stderr.String(), "city-scoped socket") {
		t.Fatalf("stderr=%q, want scope refusal", stderr.String())
	}
	for _, event := range sp.events {
		if event == "TeardownServer" {
			t.Fatal("unscoped server was torn down")
		}
	}
}

// TestCmdStopBodyTeardownRunsAfterStopOrphansBeforeBeadsShutdown is a
// coordination test that verifies the stop-path ordering contract in
// cmdStopBody: TeardownServer is called after stopOrphans has run its
// ListRunning sweep AND before the bead-provider shutdown.
//
// Expected sequence: doStop -> stopOrphans (ListRunning) -> TeardownServer -> shutdownBeadsProvider
func TestCmdStopBodyTeardownRunsAfterStopOrphansBeforeBeadsShutdown(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "lifecycle-order-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
	}
	writeStopLifecycleCityConfig(t, cityDir, cfg)

	sp := &lifecycleOrderProvider{Fake: runtime.NewFake()}

	var orderMu sync.Mutex
	var shutdownCalled bool
	var eventsAtShutdown []string
	overrideShutdownBeadsProviderForStop(t, func(string) error {
		sp.mu.Lock()
		snapshot := make([]string, len(sp.events))
		copy(snapshot, sp.events)
		sp.mu.Unlock()

		orderMu.Lock()
		shutdownCalled = true
		eventsAtShutdown = snapshot
		orderMu.Unlock()
		return nil
	})

	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return sp, nil }

	var stdout, stderr lockedBuffer
	code := cmdStopBody(cityDir, cfg, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdStopBody() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	sp.mu.Lock()
	allProviderEvents := make([]string, len(sp.events))
	copy(allProviderEvents, sp.events)
	sp.mu.Unlock()

	orderMu.Lock()
	called := shutdownCalled
	snapshot := eventsAtShutdown
	orderMu.Unlock()

	// TeardownServer must be present.
	teardownIdx := -1
	for i, e := range allProviderEvents {
		if e == "TeardownServer" {
			teardownIdx = i
			break
		}
	}
	if teardownIdx < 0 {
		t.Fatalf("TeardownServer was never called; provider events = %v", allProviderEvents)
	}

	// TeardownServer must precede the bead-provider shutdown.
	if called {
		found := false
		for _, e := range snapshot {
			if e == "TeardownServer" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("TeardownServer must occur before bead-provider shutdown; events at shutdown = %v, all provider events = %v",
				snapshot, allProviderEvents)
		}
	}

	// TeardownServer must follow at least one ListRunning (the orphan sweep).
	listRunningBeforeTeardown := false
	for _, e := range allProviderEvents[:teardownIdx] {
		if e == "ListRunning" {
			listRunningBeforeTeardown = true
			break
		}
	}
	if !listRunningBeforeTeardown {
		t.Fatalf("TeardownServer called before any ListRunning (orphan sweep must precede teardown); events = %v", allProviderEvents)
	}
}

// TestCmdStopBodySkipsTeardownForNonLifecycleProvider verifies that cmdStopBody
// completes successfully when the session provider does not implement
// runtime.ServerLifecycleProvider. The type assertion must yield ok=false and
// skip teardown silently: no panic, no error in stderr.
//
// This is the normal case for non-tmux providers (subprocess, exec, K8s, Fake).
func TestCmdStopBodySkipsTeardownForNonLifecycleProvider(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "no-lifecycle-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
	}
	writeStopLifecycleCityConfig(t, cityDir, cfg)

	// Verify that runtime.Fake does NOT implement ServerLifecycleProvider.
	// This guards against a future change that accidentally adds the interface
	// to Fake (which would defeat the skip path and change non-tmux behavior).
	var baseProvider runtime.Provider = runtime.NewFake()
	if _, ok := baseProvider.(runtime.ServerLifecycleProvider); ok {
		t.Fatal("runtime.Fake must not implement runtime.ServerLifecycleProvider; " +
			"non-lifecycle providers must skip teardown via ok=false type assertion")
	}

	overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })

	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) {
		return runtime.NewFake(), nil
	}

	var stdout, stderr lockedBuffer
	code := cmdStopBody(cityDir, cfg, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdStopBody() = %d, want 0 for non-lifecycle provider; stdout=%q stderr=%q",
			code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout = %q, want City stopped.", stdout.String())
	}
	if strings.Contains(stderr.String(), "teardown server") {
		t.Fatalf("stderr = %q, unexpected teardown error for non-lifecycle provider", stderr.String())
	}
}

// TestCmdStopBodyReportsTeardownErrorWithoutFailing verifies that tmux server
// teardown is best-effort: a provider error is visible to the operator but does
// not change the stop exit code.
func TestCmdStopBodyReportsTeardownErrorWithoutFailing(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "lifecycle-error-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
	}
	writeStopLifecycleCityConfig(t, cityDir, cfg)

	sp := &lifecycleOrderProvider{
		Fake:        runtime.NewFake(),
		teardownErr: errors.New("provider-stop-failed"),
	}

	overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })

	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return sp, nil }

	var stdout, stderr lockedBuffer
	code := cmdStopBody(cityDir, cfg, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdStopBody() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout = %q, want City stopped.", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gc stop: teardown server: provider-stop-failed") {
		t.Fatalf("stderr = %q, want teardown server warning", stderr.String())
	}
}

func writeStopLifecycleCityConfig(t *testing.T, cityDir string, cfg *config.City) {
	t.Helper()
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}
