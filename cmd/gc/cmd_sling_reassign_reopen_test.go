package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

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
