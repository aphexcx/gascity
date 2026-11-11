package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestSlingHelpersDoNotOpenDiagnosticStores(t *testing.T) {
	for _, batch := range []bool{false, true} {
		name := "single"
		if batch {
			name = "batch"
		}
		t.Run(name, func(t *testing.T) {
			a := config.Agent{Name: "worker", Dir: "rig", MaxActiveSessions: intPtr(1)}
			cfg := &config.City{Agents: []config.Agent{a}, Rigs: []config.Rig{{Name: "rig", Path: t.TempDir(), Prefix: "gc"}}}
			deps, stdout, stderr := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
			deps.StoreRef = "rig:rig"
			store := beads.NewMemStore()
			deps.Store = store
			work, err := store.Create(beads.Bead{Title: "work", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			opened := false
			previous := slingOpenCityStore
			slingOpenCityStore = func(string) (beads.Store, error) {
				opened = true
				return nil, errors.New("unexpected diagnostic store open")
			}
			t.Cleanup(func() { slingOpenCityStore = previous })
			opts := testOpts(a, work.ID)
			opts.SkipPoke = true
			var code int
			if batch {
				code = doSlingBatch(opts, deps, store, stdout, stderr)
			} else {
				code = doSling(opts, deps, store, stdout, stderr)
			}
			if code != 0 || opened {
				t.Fatalf("helper result=%d opened diagnostic store=%v; stderr=%s", code, opened, stderr.String())
			}
		})
	}
}

func TestCmdSlingWarnsAboutClosedPoolSessionName(t *testing.T) {
	for _, tc := range []struct {
		name, target, heldName string
		capacity               int
	}{
		{name: "singleton", target: "worker", heldName: "worker", capacity: 1},
		{name: "pool template", target: "worker", heldName: "worker-1-pool", capacity: 2},
		{name: "pool member", target: "worker-1", heldName: "worker-1-pool", capacity: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configureIsolatedRuntimeEnv(t)
			t.Setenv("GC_BEADS", "file")
			cityDir := t.TempDir()
			if err := ensureScopedFileStoreLayout(cityDir); err != nil {
				t.Fatal(err)
			}
			if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
				t.Fatal(err)
			}
			cityToml := fmt.Sprintf(`[workspace]
name = "test-city"

[[agent]]
name = "worker"
max_active_sessions = %d
`, tc.capacity)
			if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Chdir(cityDir)
			t.Setenv("GC_CITY_PATH", cityDir)
			store, err := openCityStoreAt(cityDir)
			if err != nil {
				t.Fatal(err)
			}
			holder, err := store.Create(beads.Bead{
				Type: session.BeadType, Labels: []string{session.LabelSession},
				Metadata: map[string]string{"session_name": tc.heldName, "session_origin": "unknown", "session_name_explicit": "true"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(holder.ID); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			code := cmdSling([]string{tc.target, "ship feature"}, false, false, false, "", nil, "", true, false, false, "", false, false, false, "", "", &stdout, &stderr)
			if code != 0 {
				t.Fatalf("cmdSling=%d: %s", code, stderr.String())
			}
			want := fmt.Sprintf("warning: pool \"worker\" cannot create its next session: name %q is held by closed session bead %s", tc.heldName, holder.ID)
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("command warning missing: %s", stderr.String())
			}
		})
	}
}

func TestSlingWarnsAboutClosedPoolSessionName(t *testing.T) {
	for _, tc := range []struct {
		name, origin        string
		closed, warn, force bool
	}{
		{name: "closed unknown", origin: "unknown", closed: true, warn: true},
		{name: "closed manual released", origin: "manual", closed: true},
		{name: "asleep manual retained", origin: "manual"},
		{name: "force suppresses warning", origin: "unknown", closed: true, force: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
			cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{a}}
			deps, _, stderr := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
			store := beads.NewMemStore()
			deps.Store = store
			holder, err := store.Create(beads.Bead{
				Type: session.BeadType, Labels: []string{session.LabelSession},
				Metadata: map[string]string{
					"session_name": "worker", "session_name_explicit": "true",
					"session_origin": tc.origin, "state": "asleep",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.closed {
				if err := store.Close(holder.ID); err != nil {
					t.Fatal(err)
				}
			}
			opts := testOpts(a, "")
			opts.Force = tc.force
			printClosedPoolSessionNameWarnings(opts, deps, stderr)
			want := "warning: pool \"worker\" cannot create its next session: name \"worker\" is held by closed session bead " + holder.ID
			if got := strings.Contains(stderr.String(), want); got != tc.warn {
				t.Fatalf("closed-name warning = %v, want %v; stderr = %q", got, tc.warn, stderr.String())
			}
			check := &sessionModelDoctorCheck{
				cfg: cfg, cityPath: deps.CityPath,
				newStore: func(string) (beads.Store, error) { return store, nil },
			}
			result := check.Run(&doctor.CheckContext{})
			found := strings.Contains(strings.Join(result.Details, "\n"), strings.TrimPrefix(want, "warning: "))
			if wantFinding := tc.warn || tc.force; found != wantFinding {
				t.Fatalf("doctor closed-name finding = %v, want %v: %+v", found, wantFinding, result)
			}
		})
	}
}

func TestSlingClosedPoolNameUsesCitySessionStoreForRigWork(t *testing.T) {
	a := config.Agent{Name: "worker", Dir: "rig", MaxActiveSessions: intPtr(1)}
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{a}}
	deps, _, stderr := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.StoreRef = "rig:rig"
	deps.Store = beads.NewMemStore()
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{{
		ID: "city-holder", Type: session.BeadType, Status: "closed",
		Metadata: map[string]string{"session_name": "rig--worker", "session_origin": "unknown"},
	}}, nil)
	previous := slingOpenCityStore
	slingOpenCityStore = func(string) (beads.Store, error) { return cityStore, nil }
	t.Cleanup(func() { slingOpenCityStore = previous })
	printClosedPoolSessionNameWarnings(testOpts(a, ""), deps, stderr)
	if !strings.Contains(stderr.String(), "name \"rig--worker\" is held by closed session bead city-holder") {
		t.Fatalf("city session holder missing from rig sling warning: %q", stderr.String())
	}
}

func TestDoctorClosedPoolNameUsesCrossStoreOccupancy(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := t.TempDir()
	a := config.Agent{Name: "worker", TmuxAlias: "lane", MaxActiveSessions: intPtr(2)}
	cfg := &config.City{Agents: []config.Agent{a}, Rigs: []config.Rig{{Name: "project", Path: rigPath}}}
	primary := beads.NewMemStoreFrom(0, []beads.Bead{{
		ID: "closed-primary", Type: session.BeadType, Status: "closed",
		Metadata: map[string]string{"session_name": "lane-2", "session_origin": "unknown"},
	}}, nil)
	var foreign beads.Store = beads.NewMemStoreFrom(0, []beads.Bead{{
		ID: "foreign-slot", Type: session.BeadType, Status: "open",
		Metadata: map[string]string{
			"session_name": "lane", "session_origin": "ephemeral", "pool_managed": "true",
			"agent_name": "worker-1", "template": "worker", "pool_slot": "1", "state": "active",
		},
	}}, nil)
	check := &sessionModelDoctorCheck{cfg: cfg, cityPath: cityPath, newStore: func(path string) (beads.Store, error) {
		if path == rigPath {
			return foreign, nil
		}
		if path != cityPath {
			t.Fatalf("unexpected store path %q", path)
		}
		return primary, nil
	}}
	result := check.Run(&doctor.CheckContext{})
	if !strings.Contains(strings.Join(result.Details, "\n"), "name \"lane-2\" is held by closed session bead closed-primary") {
		t.Fatalf("cross-store next-slot holder missing: %+v", result)
	}
	foreign = listFailStore{Store: foreign}
	result = check.Run(&doctor.CheckContext{})
	if !strings.Contains(strings.Join(result.Details, "\n"), "list failed") {
		t.Fatalf("foreign census failure hidden: %+v", result)
	}
	cfg.Rigs[0].Suspended = true
	result = check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("suspended foreign store should not participate in the census: %+v", result)
	}
}

func TestClosedPoolSessionNameWarningsNextAliasedSlot(t *testing.T) {
	a := config.Agent{Name: "worker", TmuxAlias: "lane", MaxActiveSessions: intPtr(2)}
	cfg := &config.City{Agents: []config.Agent{a}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "active-slot", Type: session.BeadType, Status: "open", Metadata: map[string]string{
			"session_name": "lane", "session_origin": "ephemeral", "pool_managed": "true",
			"agent_name": "worker-1", "template": "worker", "pool_slot": "1", "state": "active",
		}},
		{ID: "retired-holder", Type: session.BeadType, Status: "closed", Metadata: map[string]string{
			"session_name": "lane-2", "session_origin": "unknown",
		}},
	}, nil)
	warnings, err := closedPoolSessionNameWarnings(cfg, "", cfg.Agents, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "name \"lane-2\" is held by closed session bead retired-holder") {
		t.Fatalf("warnings = %v, want the next aliased slot's holder", warnings)
	}
	broken := degradedQueryStore{Store: store, err: errors.New("reservation query unavailable")}
	if _, err := closedPoolSessionNameWarnings(cfg, "", cfg.Agents, broken, nil); err == nil || !strings.Contains(err.Error(), "reservation query unavailable") {
		t.Fatalf("reservation query failure was hidden: %v", err)
	}
}
