package sling

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestDoSling_SingletonPoolRefusesHumanAssignee(t *testing.T) {
	opts, deps, store, bead := reassignTestSetup(t, "human")
	opts.Target.MaxActiveSessions = intPtr(1)
	opts.Target.MinActiveSessions = intPtr(0)
	router := &fakeBeadRouter{}
	deps.Router = router
	_, err := DoSling(opts, deps, store)
	if err == nil || !strings.Contains(err.Error(), "human") || !strings.Contains(err.Error(), "--reassign") {
		t.Errorf("singleton pool error = %v, want named ownership refusal", err)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, bead) || len(router.routed) != 0 {
		t.Errorf("singleton pool mutated or routed human-held work: before=%+v after=%+v routes=%v", bead, got, router.routed)
	}
}

func TestDoSling_DurablePoolSessionOwner(t *testing.T) {
	for _, tc := range []struct {
		name      string
		template  string
		wantOwn   bool
		split     bool
		readErr   error
		afterRead bool
	}{
		{name: "own", template: "myrig/polecat", wantOwn: true},
		{name: "foreign", template: "myrig/other"},
		{name: "split-own", template: "myrig/polecat", wantOwn: true, split: true},
		{name: "split-foreign", template: "myrig/other", split: true},
		{name: "split-read-error", split: true, readErr: errors.New("session store unavailable")},
		{name: "split-recheck-error", template: "myrig/polecat", split: true, readErr: errors.New("session store became unavailable"), afterRead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, deps, store, bead := reassignTestSetup(t, "")
			ownerStore := store
			if tc.split {
				separate := beads.NewMemStore()
				separate.IDPrefix = "session"
				ownerStore = separate
				reads := 0
				deps.SessionLookup = func(id string) (session.Info, error) {
					reads++
					if tc.readErr != nil && (!tc.afterRead || reads > 1) {
						return session.Info{}, tc.readErr
					}
					info, _, err := session.ResolveSessionRecordByExactID(ownerStore, id)
					return info, err
				}
			}
			owner, err := ownerStore.Create(beads.Bead{Title: "session", Type: "session", Metadata: map[string]string{"template": tc.template}})
			if err != nil {
				t.Fatal(err)
			}
			status := "in_progress"
			if err := store.Update(bead.ID, beads.UpdateOpts{Status: &status, Assignee: &owner.ID, Metadata: map[string]string{
				"gc.routed_to": "myrig/polecat", "gc.session_id": owner.ID,
			}}); err != nil {
				t.Fatal(err)
			}
			before, err := store.Get(bead.ID)
			if err != nil {
				t.Fatal(err)
			}
			opts.NoConvoy = true
			opts.Nudge = true
			router := &fakeBeadRouter{}
			deps.Router = router
			result, err := DoSling(opts, deps, store)
			if tc.readErr != nil && !errors.Is(err, tc.readErr) {
				t.Errorf("lookup error = %v, want preserved session-store failure", err)
			}
			if tc.wantOwn {
				if err != nil || !result.Idempotent || result.NudgeAgent == nil {
					t.Errorf("own durable session sling = %+v, %v; want idempotent nudge", result, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), owner.ID) || result.NudgeAgent != nil {
				t.Errorf("foreign durable session sling = %+v, %v; want named ownership refusal without nudge", result, err)
			}
			got, err := store.Get(bead.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, before) || len(router.routed) != 0 {
				t.Errorf("durable claim mutated or routed: before=%+v after=%+v routes=%v", before, got, router.routed)
			}
		})
	}
}

func TestDoSling_Reassign_RefusesClosedWork(t *testing.T) {
	opts, deps, store, bead := reassignTestSetup(t, "human")
	closed := "closed"
	if err := store.Update(bead.ID, beads.UpdateOpts{Status: &closed, Metadata: map[string]string{
		"gc.work_outcome": "blocked", "gc.work_commit": "abc123",
	}}); err != nil {
		t.Fatal(err)
	}
	before, err := store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	opts.Reassign = true
	router := &fakeBeadRouter{}
	deps.Router = router
	_, err = DoSling(opts, deps, store)
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("DoSling --reassign error = %v, want closed-work refusal", err)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if beads.DependencySatisfied(got.Status, got.Metadata["gc.work_outcome"]) {
		t.Error("reassignment made a blocked closed bead satisfy dependent work")
	}
	if !reflect.DeepEqual(got, before) || len(router.routed) != 0 {
		t.Errorf("closed work was mutated or routed: before=%+v after=%+v routes=%v", before, got, router.routed)
	}
}

func TestDoSling_Reassign_ReopensNormalizedDeferredHold(t *testing.T) {
	opts, deps, _, bead := reassignTestSetup(t, "human")
	bead.IndefinitelyDeferred = true
	store := beads.NewMemStoreFrom(1, []beads.Bead{bead}, nil)
	deps.Store = store
	opts.Reassign = true
	if _, err := DoSling(opts, deps, store); err != nil {
		t.Fatalf("DoSling --reassign: %v", err)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignee != "" || !beads.IsReadyCandidate(got, time.Unix(0, 0)) {
		t.Errorf("normalized deferred hold remains unclaimable: %+v", got)
	}
}

func TestDoSling_Reassign_ScheduledDeferral(t *testing.T) {
	for _, tc := range []struct {
		name     string
		deadline time.Time
		refuse   bool
	}{
		{"future", time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), true},
		{"expired", time.Unix(1, 0), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, deps, _, bead := reassignTestSetup(t, "human")
			bead.DeferUntil = &tc.deadline
			store := beads.NewMemStoreFrom(1, []beads.Bead{bead}, nil)
			deps.Store = store
			router := &fakeBeadRouter{}
			deps.Router = router
			opts.Reassign = true
			_, err := DoSling(opts, deps, store)
			if tc.refuse {
				if err == nil || !strings.Contains(err.Error(), tc.deadline.Format(time.RFC3339)) || !strings.Contains(err.Error(), "clear the scheduled deferral") {
					t.Errorf("reassign error = %v, want deadline and instruction to clear scheduled deferral", err)
				}
			} else if err != nil {
				t.Fatalf("expired deferral refused: %v", err)
			}
			got, err := store.Get(bead.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.refuse {
				if !reflect.DeepEqual(got, bead) || len(router.routed) != 0 {
					t.Errorf("scheduled hold mutated or routed: before=%+v after=%+v routes=%v", bead, got, router.routed)
				}
			} else if got.Assignee != "" || !beads.IsReadyCandidate(got, time.Unix(2, 0)) || len(router.routed) != 1 {
				t.Errorf("expired hold was not released and routed: bead=%+v routes=%v", got, router.routed)
			}
		})
	}
}

// A session can claim a bead after sling reads it but before the release write.
type claimBeforeReassignStore struct {
	*beads.MemStore
	claim func() error
}

func (s *claimBeforeReassignStore) Update(id string, opts beads.UpdateOpts) error {
	if err := s.claim(); err != nil {
		return err
	}
	return s.MemStore.Update(id, opts)
}

func (s *claimBeforeReassignStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	if err := s.claim(); err != nil {
		return err
	}
	return s.MemStore.UpdateIfMatch(id, revision, opts)
}

func TestReopenForReassign_PreservesConcurrentClaim(t *testing.T) {
	mem := beads.NewMemStore()
	bead, err := mem.Create(beads.Bead{Title: "held work", Type: "task", Assignee: "human"})
	if err != nil {
		t.Fatal(err)
	}
	claimed := false
	store := &claimBeforeReassignStore{MemStore: mem, claim: func() error {
		if claimed {
			return nil
		}
		claimed = true
		status, owner := "in_progress", "worker-session-1"
		return mem.Update(bead.ID, beads.UpdateOpts{Status: &status, Assignee: &owner, Metadata: map[string]string{"gc.keep": "yes"}})
	}}
	err = reopenForReassignInStore(store, bead.ID, bead)
	if err == nil {
		t.Error("reassign overwrote a concurrent session claim")
	}
	got, err := mem.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "in_progress" || got.Assignee != "worker-session-1" || len(got.Metadata) != 1 || got.Metadata["gc.keep"] != "yes" {
		t.Errorf("concurrent claim was changed by reassign: %+v", got)
	}
}

func TestReopenForReassign_RequiresConditionalWriter(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{Title: "held work", Type: "task", Assignee: "human"})
	if err != nil {
		t.Fatal(err)
	}
	// Expose only the base Store contract, as a backend without atomic writes.
	legacy := struct{ beads.Store }{store}
	err = reopenForReassignInStore(legacy, bead.ID, bead)
	if !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
		t.Errorf("reassign error = %v, want explicit conditional-write refusal", err)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, bead) {
		t.Errorf("unsupported release mutated bead: before=%+v after=%+v", bead, got)
	}
}

func TestDoSlingBatch_Reassign_ReopensHeldChildren(t *testing.T) {
	opts, deps, store, _ := reassignTestSetup(t, "")
	convoy, err := store.Create(beads.Bead{Title: "batch", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	var heldIDs []string
	for _, status := range []string{"deferred", "blocked"} {
		child, err := store.Create(beads.Bead{Title: status, Type: "task", ParentID: convoy.ID, Assignee: "human"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Update(child.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatal(err)
		}
		heldIDs = append(heldIDs, child.ID)
	}
	opts.BeadOrFormula = convoy.ID
	opts.Reassign = true
	result, err := DoSlingBatch(opts, deps, store)
	if err != nil {
		t.Fatalf("DoSlingBatch --reassign: %v", err)
	}
	if result.Routed != 2 {
		t.Errorf("routed = %d, want both held children", result.Routed)
	}
	for _, id := range heldIDs {
		got, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "open" || got.Assignee != "" {
			t.Errorf("held child %s was not released: status=%s assignee=%q", id, got.Status, got.Assignee)
		}
	}
}

func TestDoSlingBatch_RefusesHumanAssigneeWithoutFlag(t *testing.T) {
	opts, deps, store, bead := reassignTestSetup(t, "human")
	convoy, err := store.Create(beads.Bead{Title: "batch", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(bead.ID, beads.UpdateOpts{ParentID: &convoy.ID}); err != nil {
		t.Fatal(err)
	}
	before, err := store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	router := &fakeBeadRouter{}
	deps.Router = router
	opts.BeadOrFormula = convoy.ID
	_, err = DoSlingBatch(opts, deps, store)
	if err == nil || !strings.Contains(err.Error(), "human") || !strings.Contains(err.Error(), "--reassign") {
		t.Errorf("batch error = %v, want ownership refusal", err)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, before) || len(router.routed) != 0 {
		t.Errorf("refused child mutated or routed: before=%+v after=%+v routes=%v", before, got, router.routed)
	}
}

func TestDoSling_ActivePoolOwnerIsIdempotent(t *testing.T) {
	opts, deps, store, bead := reassignTestSetup(t, "myrig/polecat-session-1")
	status := "in_progress"
	if err := store.Update(bead.ID, beads.UpdateOpts{Status: &status, Metadata: map[string]string{"gc.routed_to": "myrig/polecat"}}); err != nil {
		t.Fatal(err)
	}
	before, err := store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	opts.NoConvoy = true
	result, err := DoSling(opts, deps, store)
	if err != nil || !result.Idempotent {
		t.Fatalf("DoSling = %+v, %v; want idempotent current-owner sling", result, err)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, before) {
		t.Errorf("idempotent sling mutated the active claim: before=%+v after=%+v", before, got)
	}
}

func TestDoSling_RefusesHumanHoldInSourceStore(t *testing.T) {
	opts, deps, rigStore, bead := reassignTestSetup(t, "human")
	deps.Store = beads.NewMemStore()
	deps.ValidationQuerier = rigStore
	deps.SourceWorkflowStores = func() ([]SourceWorkflowStore, error) {
		return []SourceWorkflowStore{{Store: rigStore, StoreRef: "rig:myrig"}}, nil
	}
	_, err := DoSling(opts, deps, nil)
	if err == nil || !strings.Contains(err.Error(), "human") || !strings.Contains(err.Error(), "--reassign") {
		t.Errorf("DoSling error = %v, want refusal naming the rig bead's assignee", err)
	}
	got, err := rigStore.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, bead) {
		t.Errorf("refused rig bead changed: before=%+v after=%+v", bead, got)
	}
}

// Explicit reassignment releases held work without stealing active claims.
func TestDoSling_Reassign_ReopensHeldBead(t *testing.T) {
	for _, status := range []string{"deferred", "blocked"} {
		t.Run(status, func(t *testing.T) {
			opts, deps, store, bead := reassignTestSetup(t, "human")
			if err := store.Update(bead.ID, beads.UpdateOpts{Status: &status, Metadata: map[string]string{
				"gc.work_outcome": "shipped", "gc.work_commit": "abc123", "gc.keep": "yes",
			}}); err != nil {
				t.Fatal(err)
			}
			opts.Reassign = true
			if _, err := DoSling(opts, deps, store); err != nil {
				t.Fatalf("DoSling --reassign: %v", err)
			}
			got, err := store.Get(bead.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Assignee != "" || got.Status != "open" {
				t.Errorf("bead = %s / %q, want open / unassigned", got.Status, got.Assignee)
			}
			if got.Metadata["gc.work_outcome"] != "" || got.Metadata["gc.work_commit"] != "" || got.Metadata["gc.keep"] != "yes" {
				t.Errorf("reassign did not clear the previous work outcome independently of unrelated metadata: %v", got.Metadata)
			}
		})
	}
}

func TestDoSling_Reassign_RefusesActiveClaim(t *testing.T) {
	for _, assignee := range []string{"human", "order:batch", "myrig/polecat-session-1"} {
		t.Run(assignee, func(t *testing.T) {
			opts, deps, store, bead := reassignTestSetup(t, assignee)
			status := "in_progress"
			if err := store.Update(bead.ID, beads.UpdateOpts{Status: &status, Metadata: map[string]string{
				"gc.routed_to": "myrig/polecat", "gc.keep": "yes", "gc.work_outcome": "shipped",
			}}); err != nil {
				t.Fatal(err)
			}
			before, err := store.Get(bead.ID)
			if err != nil {
				t.Fatal(err)
			}
			opts.Reassign = true
			router := &fakeBeadRouter{}
			deps.Router = router
			_, err = DoSling(opts, deps, store)
			if err == nil || !strings.Contains(err.Error(), assignee) || !strings.Contains(err.Error(), "in_progress") {
				t.Errorf("DoSling --reassign error = %v, want active claim refusal naming %q", err, assignee)
			}
			got, err := store.Get(bead.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, before) || len(router.routed) != 0 {
				t.Errorf("refused claim was mutated or routed: before=%+v after=%+v routes=%v", before, got, router.routed)
			}
		})
	}
}

// TestDoSling_ReassignFormula_DoesNotReopenCollidingBead is the regression
// guard for the standalone formula + --reassign hazard. LaunchFormula forwards
// Reassign and sets BeadOrFormula to the formula NAME (not a bead ID), and
// pre-flight runs the reassign reopen before the IsFormula dispatch. Without
// the shouldReopenForReassign guard, reopenForReassign was called on that name,
// so a bead whose ID happened to equal the formula name was silently
// cleared/reopened — disrupting work another actor had already claimed. A
// standalone formula launch must never touch a same-named bead.
func TestDoSling_ReassignFormula_DoesNotReopenCollidingBead(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	deps := testDeps(cfg, sp, runner.run)
	// Seed a bead whose ID collides with the "code-review" formula name and put
	// it in the order-claimed state (status=in_progress, assignee set) that
	// reopenForReassign would otherwise clear.
	deps.Store = seededStore("code-review")
	inProgress, orderActor := "in_progress", "order:mol-dog-jsonl"
	if err := deps.Store.Update("code-review", beads.UpdateOpts{Status: &inProgress, Assignee: &orderActor}); err != nil {
		t.Fatalf("Update colliding bead to order-claimed state: %v", err)
	}

	result, err := DoSling(SlingOpts{
		Target:        a,
		BeadOrFormula: "code-review",
		IsFormula:     true,
		Reassign:      true,
	}, deps, nil)
	if err != nil {
		t.Fatalf("DoSling formula launch with --reassign: %v", err)
	}
	if result.Method != "formula" {
		t.Errorf("Method = %q, want formula (standalone formula launch)", result.Method)
	}

	got, err := deps.Store.Get("code-review")
	if err != nil {
		t.Fatalf("store.Get(code-review): %v", err)
	}
	if got.Assignee != orderActor {
		t.Errorf("Assignee = %q, want %q — a standalone formula launch must not reopen a bead sharing the formula name", got.Assignee, orderActor)
	}
	if got.Status != "in_progress" {
		t.Errorf("Status = %q, want in_progress — a standalone formula launch must not reopen a bead sharing the formula name", got.Status)
	}
}
