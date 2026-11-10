package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

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
			deps, stdout, stderr := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
			store := beads.NewMemStore()
			deps.Store = store
			b, err := store.Create(beads.Bead{Title: "work", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
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
			opts := testOpts(a, b.ID)
			opts.SkipPoke = true
			opts.Force = tc.force
			if code := doSlingBatch(opts, deps, store, stdout, stderr); code != 0 {
				t.Fatalf("sling = %d: %s", code, stderr.String())
			}
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
	warnings, err := closedPoolSessionNameWarnings(cfg, "", cfg.Agents, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "name \"lane-2\" is held by closed session bead retired-holder") {
		t.Fatalf("warnings = %v, want the next aliased slot's holder", warnings)
	}
	broken := degradedQueryStore{Store: store, err: errors.New("reservation query unavailable")}
	if _, err := closedPoolSessionNameWarnings(cfg, "", cfg.Agents, broken); err == nil || !strings.Contains(err.Error(), "reservation query unavailable") {
		t.Fatalf("reservation query failure was hidden: %v", err)
	}
}
