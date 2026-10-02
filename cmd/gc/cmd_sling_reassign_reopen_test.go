package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSlingRetainsCitySessionOwnerOfRigWork(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "city session", true: "city unavailable"}[unavailable], func(t *testing.T) {
			a := config.Agent{Name: "builder", Dir: "project", MaxActiveSessions: intPtr(2)}
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{a},
				Rigs: []config.Rig{{Name: "project", Path: "/project", Prefix: "work"}},
			}
			deps, stdout, stderr := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
			deps.StoreRef = "rig:project"
			work := beads.Bead{
				ID: "work-1", Type: "task", Status: "in_progress", Assignee: "session-1",
				Metadata: map[string]string{"gc.routed_to": a.QualifiedName()},
			}
			deps.Store = beads.NewMemStoreFrom(1, []beads.Bead{work}, nil)
			cityStore := beads.NewMemStoreFrom(1, []beads.Bead{{
				ID: "session-1", Type: "session", Status: "open",
				Metadata: map[string]string{"template": a.QualifiedName(), "state": "active"},
			}}, nil)
			previous := slingOpenCityStore
			slingOpenCityStore = func(path string) (beads.Store, error) {
				if path != deps.CityPath {
					t.Fatalf("city path = %q, want %q", path, deps.CityPath)
				}
				if unavailable {
					return nil, errors.New("session ledger unavailable")
				}
				return cityStore, nil
			}
			t.Cleanup(func() { slingOpenCityStore = previous })
			before, err := deps.Store.Get(work.ID)
			if err != nil {
				t.Fatal(err)
			}
			opts := testOpts(a, work.ID)
			opts.NoFormula, opts.NoConvoy = true, true
			code := doSling(opts, deps, deps.Store, stdout, stderr)
			if unavailable {
				if code == 0 || !strings.Contains(stderr.String(), "session ledger unavailable") {
					t.Fatalf("missing session-ledger failure: code=%d stderr=%s", code, stderr.String())
				}
			} else if code != 0 || !strings.Contains(stdout.String(), "already routed") {
				t.Fatalf("existing session-owned route was not preserved: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			after, err := deps.Store.Get(work.ID)
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("session claim changed: before=%+v after=%+v err=%v", before, after, err)
			}
		})
	}
}

// The CLI forwards explicit reassignment into formula-backed held-work routing.
func TestOnFormulaReassignReopensHeldBead(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "polecat", MaxActiveSessions: intPtr(2)}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	deps.Store = beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "BL-42", Title: "hotspot work", Type: "task", Status: "deferred", Assignee: "human"},
	}, nil)
	deps.Store = wrapStoreWithBeadPolicies(deps.Store, cfg)

	opts := testOpts(a, "BL-42")
	opts.OnFormula = "mol-polecat-work"
	opts.NoConvoy = true
	opts.Reassign = true

	code := doSling(opts, deps, deps.Store, stdout, stderr)
	if code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}

	source, err := deps.Store.Get("BL-42")
	if err != nil {
		t.Fatalf("store.Get(BL-42): %v", err)
	}
	if got := source.Metadata["gc.routed_to"]; got != "polecat" {
		t.Errorf("gc.routed_to = %q, want polecat", got)
	}
	if source.Assignee != "" {
		t.Errorf("Assignee = %q, want empty after --reassign (the human hold must be released)", source.Assignee)
	}
	if source.Status != "open" {
		t.Errorf("Status = %q, want open after --reassign so the pool can claim it", source.Status)
	}
}
