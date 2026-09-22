package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func newPoolCensusGapEnv(t *testing.T) (*reconcilerTestEnv, beads.Bead) {
	t.Helper()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{
		Name: "worker", Dir: "repo", WorkDir: "{{.CityRoot}}/.worktrees/{{.Rig}}/worker",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1),
	}}}
	b := env.createSessionBead("repo--worker", "repo/worker")
	env.setSessionMetadata(&b, map[string]string{
		"alias": "repo/worker", "agent_name": "repo/worker", "pool_managed": "true",
		"state": "awake", "gc.trigger_bead_id": "work-trigger",
	})
	if err := env.sp.Start(context.Background(), "repo--worker", runtime.Config{Command: "test-cmd"}); err != nil {
		t.Fatal(err)
	}
	return env, b
}

func TestReconcileSessionBeads_PoolClaimSurvivesCensusGap(t *testing.T) {
	for _, identity := range []string{"qualified-pool", "session-name", "session-id-metadata"} {
		t.Run(identity, func(t *testing.T) {
			env, seat := newPoolCensusGapEnv(t)
			assignee := "repo/worker"
			switch identity {
			case "session-name":
				assignee = "repo--worker"
			case "session-id-metadata":
				assignee = "legacy-worker"
			}
			work := mustCreateInProgressWork(t, env.store, assignee)
			if err := env.store.SetMetadata(work.ID, beadmeta.SessionIDMetadataKey, seat.ID); err != nil {
				t.Fatal(err)
			}
			env.setSessionMetadata(&seat, map[string]string{"gc.trigger_bead_id": work.ID})
			// Empty desiredState models the tick whose demand census omitted the
			// live claim. Repeating it proves the live guard, not just a delay.
			for tick := 0; tick < 3; tick++ {
				env.reconcile([]beads.Bead{seat})
				if ds := env.dt.get(seat.ID); ds != nil {
					t.Fatalf("live claimed pool session drained on census gap tick %d: reason=%q", tick+1, ds.reason)
				}
			}
			if !env.sp.IsRunning("repo--worker") || env.sessionInfo(seat.ID).Closed {
				t.Fatal("live claimed session must remain running and open")
			}
			if !strings.Contains(env.stdout.String(), "live assigned work found") {
				t.Fatalf("claim was not recognized by the live guard: %s", env.stdout.String())
			}
		})
	}
}

func TestReconcileSessionBeads_PoolTriggerOrClaimRequiresConsecutiveGapTicks(t *testing.T) {
	for _, key := range []string{"gc.trigger_bead_id", beadmeta.CurrentClaimBeadIDMetadataKey} {
		t.Run(key, func(t *testing.T) {
			env, seat := newPoolCensusGapEnv(t)
			env.setSessionMetadata(&seat, map[string]string{"gc.trigger_bead_id": ""})
			env.setSessionMetadata(&seat, map[string]string{key: "completed-work"})
			env.reconcile([]beads.Bead{seat})
			if ds := env.dt.get(seat.ID); ds != nil {
				t.Fatalf("pool session drained on first gap tick: reason=%q", ds.reason)
			}
			env.reconcile([]beads.Bead{seat})
			if ds := env.dt.get(seat.ID); ds == nil || ds.reason != "orphaned" {
				t.Fatalf("pool without live work must drain after two confirming ticks: %#v", ds)
			}
		})
	}
}

func TestCollectAssignedWorkBeads_ExternalClaimInsideCacheCadence(t *testing.T) {
	backing := beads.NewMemStore()
	work, err := backing.Create(beads.Bead{Title: "externally claimed lane work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The claiming process writes the live store. The controller's cache has
	// not reconciled or received the event yet, so its row stays open/unassigned.
	if err := backing.Update(work.ID, beads.UpdateOpts{
		Status: stringPtr("in_progress"), Assignee: stringPtr("repo/worker"),
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "repo/worker"},
	}); err != nil {
		t.Fatal(err)
	}
	stale, err := beads.HandlesFor(cache).Cached.Get(work.ID)
	if err != nil || stale.Status != "open" || stale.Assignee != "" {
		t.Fatalf("expected coherent stale cached row before cadence: %#v, %v", stale, err)
	}
	got, partial := collectAssignedWorkBeads(&config.City{Agents: []config.Agent{{
		Name: "worker", Dir: "repo", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1),
	}}}, cache)
	if partial || len(got) != 1 || got[0].ID != work.ID || got[0].Status != "in_progress" || got[0].Assignee != "repo/worker" {
		t.Fatalf("census lost the live claim inside cache cadence: beads=%#v partial=%v", got, partial)
	}
}

func TestSessionAssignedWorkGuardPoolIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, assignee, sessionID, alias, agentName, status, kind string
		want                                                      bool
	}{
		{name: "alias", assignee: "repo/worker", alias: "repo/worker", status: "in_progress", want: true},
		{name: "agent-name", assignee: "repo/worker", agentName: "repo/worker", status: "in_progress", want: true},
		{name: "session-id", assignee: "legacy", sessionID: "self", status: "in_progress", want: true},
		{name: "open-session-id", assignee: "legacy", sessionID: "self", status: "open", want: true},
		{name: "closed-session-id", assignee: "legacy", sessionID: "self", status: "closed"},
		{name: "other-session", assignee: "legacy", sessionID: "another-session", status: "in_progress"},
		{name: "rebound-lane", assignee: "repo/worker", alias: "repo/worker", sessionID: "another-session", status: "in_progress"},
		{name: "mail", assignee: "legacy", sessionID: "self", status: "open", kind: "message"},
		{name: "session-bead", assignee: "legacy", sessionID: "self", status: "open", kind: session.BeadType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, seat := newPoolCensusGapEnv(t)
			env.setSessionMetadata(&seat, map[string]string{"alias": tc.alias, "agent_name": tc.agentName})
			sessionID := tc.sessionID
			if sessionID == "self" {
				sessionID = seat.ID
			}
			work, err := env.store.Create(beads.Bead{
				Title: "work", Type: tc.kind, Assignee: tc.assignee,
				Metadata: map[string]string{beadmeta.SessionIDMetadataKey: sessionID},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := env.store.Update(work.ID, beads.UpdateOpts{Status: &tc.status}); err != nil {
				t.Fatal(err)
			}
			for _, probe := range []struct {
				name string
				run  func() (bool, error)
			}{
				{"raw", func() (bool, error) {
					return sessionHasOpenAssignedWorkBeforeDrainForConfig("", env.cfg, env.store, nil, seat)
				}},
				{"info", func() (bool, error) {
					return sessionHasOpenAssignedWorkBeforeDrainForConfigInfo("", env.cfg, env.store, nil, env.sessionInfo(seat.ID))
				}},
			} {
				has, err := probe.run()
				if err != nil || has != tc.want {
					t.Errorf("%s guard = %v, %v; want %v", probe.name, has, err, tc.want)
				}
			}
		})
	}
}

func TestReconcileSessionBeads_PoolGapConfirmationResetsOnAssignedWork(t *testing.T) {
	env, seat := newPoolCensusGapEnv(t)
	env.reconcile([]beads.Bead{seat})
	work := mustCreateInProgressWork(t, env.store, "repo--worker")
	env.reconcile([]beads.Bead{seat})
	if err := env.store.Close(work.ID); err != nil {
		t.Fatal(err)
	}
	env.reconcile([]beads.Bead{seat})
	if ds := env.dt.get(seat.ID); ds != nil {
		t.Fatalf("work recovery must reset consecutive gap window: %#v", ds)
	}
	env.reconcile([]beads.Bead{seat})
	if ds := env.dt.get(seat.ID); ds == nil {
		t.Fatal("confirmed absence must eventually drain")
	}
}

func TestReconcileSessionBeads_PoolWithoutTriggerOrClaimDrainsImmediately(t *testing.T) {
	env, seat := newPoolCensusGapEnv(t)
	env.setSessionMetadata(&seat, map[string]string{"gc.trigger_bead_id": ""})
	env.reconcile([]beads.Bead{seat})
	if ds := env.dt.get(seat.ID); ds == nil || ds.reason != "orphaned" {
		t.Fatalf("idle orphan should drain immediately: %#v", ds)
	}
}

type poolClaimQueryErrorStore struct{ beads.Store }

func (s poolClaimQueryErrorStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Metadata[beadmeta.SessionIDMetadataKey] != "" {
		return nil, errors.New("claim query unavailable")
	}
	return s.Store.List(q)
}

func TestReconcileSessionBeads_PoolGapConfirmationResetsOnClaimQueryError(t *testing.T) {
	env, seat := newPoolCensusGapEnv(t)
	env.reconcile([]beads.Bead{seat})
	backing := env.store
	env.store = poolClaimQueryErrorStore{backing}
	env.reconcile([]beads.Bead{seat})
	if ds := env.dt.get(seat.ID); ds != nil {
		t.Fatal("query error must prevent a drain")
	}
	if !strings.Contains(env.stderr.String(), "claim query unavailable") {
		t.Fatal("query error must be reported")
	}
	env.store = backing
	env.reconcile([]beads.Bead{seat})
	if ds := env.dt.get(seat.ID); ds != nil {
		t.Fatal("query error must reset consecutive confirmation window")
	}
}

func TestReconcileSessionBeads_PoolGapConfirmationResetsOnLivenessError(t *testing.T) {
	env, seat := newPoolCensusGapEnv(t)
	env.reconcile([]beads.Bead{seat})
	sp := &sequencedRuntimeObservationProvider{Fake: env.sp, livenessUnavailableAt: map[int]bool{1: true}}
	reconcileWithSequencedRuntimeObservation(env, []beads.Bead{seat}, sp, nil)
	if ds := env.dt.get(seat.ID); ds != nil {
		t.Fatal("liveness error must prevent a drain")
	}
	env.reconcile([]beads.Bead{seat})
	if ds := env.dt.get(seat.ID); ds != nil {
		t.Fatal("liveness error must reset consecutive confirmation window")
	}
	if !strings.Contains(env.stderr.String(), "liveness observation failed") {
		t.Fatal("liveness error must be reported")
	}
}

func TestReconcileSessionBeads_PoolCurrentClaimInsideCacheCadence(t *testing.T) {
	env, seat := newPoolCensusGapEnv(t)
	env.setSessionMetadata(&seat, map[string]string{"gc.trigger_bead_id": ""})
	backing := env.store
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	env.store = cache
	// The claim pointer is written from the worker process. Its work may be
	// temporarily unreadable by the demand pass, so the pointer earns a buffer.
	if err := backing.SetMetadata(seat.ID, beadmeta.CurrentClaimBeadIDMetadataKey, "work-claim"); err != nil {
		t.Fatal(err)
	}
	env.reconcile([]beads.Bead{seat})
	if ds := env.dt.get(seat.ID); ds != nil {
		t.Fatal("fresh external current-claim pointer must defer first drain")
	}
}

func TestReconcileSessionBeads_PoolCensusGapTraceReportsLiveAssignedWork(t *testing.T) {
	env, seat := newPoolCensusGapEnv(t)
	mustCreateInProgressWork(t, env.store, "repo/worker")
	cityDir := t.TempDir()
	tracer := newSessionReconcilerTracer(cityDir, "test-city", io.Discard)
	t.Cleanup(func() { _ = tracer.Close() })
	now := time.Now().UTC()
	if _, err := tracer.armStore.upsertArm(TraceArm{
		ScopeType: TraceArmScopeTemplate, ScopeValue: "repo/worker", Source: TraceArmSourceManual,
		Level: TraceModeDetail, ArmedAt: now, ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "", now, env.cfg)
	if cycle == nil {
		t.Fatal("missing trace cycle")
	}
	reconcileSessionBeadsTraced(context.Background(), cityDir, []beads.Bead{seat}, env.desiredState,
		nil, env.cfg, env.sp, env.store, nil, nil, nil, nil, env.dt, nil, false, nil,
		"test-city", nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, cycle, env.startOptions...)
	if err := cycle.End(TraceCompletionCompleted, nil); err != nil {
		t.Fatal(err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatal(err)
	}
	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range records {
		if rec.SiteCode == TraceSiteReconcilerOrphaned && rec.OutcomeCode == TraceOutcomeKeptOpen && rec.Fields["live_assigned_work"] == true {
			return
		}
	}
	t.Fatalf("missing kept-open live-assigned-work trace: %#v", records)
}

func TestSessionAssignedWorkGuardFindsExternallyClaimedWispInRig(t *testing.T) {
	env, seat := newPoolCensusGapEnv(t)
	rigPath := t.TempDir()
	env.cfg.Rigs = []config.Rig{{Name: "repo", Path: rigPath}}
	backing := beads.NewMemStore()
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	work, err := backing.Create(beads.Bead{
		Title: "claimed step", Type: "task", Ephemeral: true,
		Assignee: "legacy", Metadata: map[string]string{beadmeta.SessionIDMetadataKey: seat.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := backing.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatal(err)
	}
	has, err := sessionHasOpenAssignedWorkBeforeDrainForConfigInfo("", env.cfg, env.store,
		map[string]beads.Store{rigPath: cache}, env.sessionInfo(seat.ID))
	if err != nil || !has {
		t.Fatalf("live rig wisp claim must protect session: has=%v err=%v", has, err)
	}
}

func TestSessionCloseGuardPreservesSuccessorClaimUnderReusedRuntimeName(t *testing.T) {
	env, seat := newPoolCensusGapEnv(t)
	work := mustCreateInProgressWork(t, env.store, "repo--worker")
	if err := env.store.SetMetadata(work.ID, beadmeta.SessionIDMetadataKey, "successor-session"); err != nil {
		t.Fatal(err)
	}
	closeSessionBeadIfUnassigned("", env.store, nil, env.cfg, seat, "orphaned", env.clk.Now(), &env.stderr)
	got, err := env.store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "in_progress" || got.Assignee != "repo--worker" || got.Metadata[beadmeta.SessionIDMetadataKey] != "successor-session" {
		t.Fatalf("closing predecessor released successor's claim: %#v", got)
	}
}
