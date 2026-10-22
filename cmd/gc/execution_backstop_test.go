package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/pkg/eventexport"
)

// The execution-backstop rows. A claim that never becomes execution is the
// residual failure after the trigger handoff is deterministic: the agent ends
// its turn holding the bead, and nothing in the fleet re-delivers the claim
// nudge because every existing backstop clears the instant the bead flips to
// in_progress. These rows own the rule that it converges in bounded minutes AND
// that a working agent never sees a keystroke.

type executionBackstopFixture struct {
	refusals claimRefusalLog
	cfg      *config.City
	store    beads.Store
	sp       *runtime.Fake
	provider runtime.Provider
	session  beads.Bead
	work     beads.Bead
	rec      *events.Fake
	drained  []string
	now      time.Time
	stdout   bytes.Buffer
	sessName string
}

func newExecutionBackstopFixture(t *testing.T) *executionBackstopFixture {
	t.Helper()
	f := &executionBackstopFixture{
		sessName: "test-city--worker-1",
		now:      time.Now().UTC(),
		rec:      events.NewFake(),
		sp:       runtime.NewFake(),
	}
	f.provider = executionDeliveryRuntime{Fake: f.sp}
	f.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:  "worker",
			Nudge: "gc hook --claim --drain-ack --json",
		}},
	}
	f.store = beads.NewMemStore()
	session, err := f.store.Create(beads.Bead{
		Title:  "session",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"session_name": f.sessName,
			"template":     "worker",
			"pool_managed": "true",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("seeding the session bead: %v", err)
	}
	f.session = session
	work, err := f.store.Create(beads.Bead{
		Title:    "claimed step",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "root-1"},
	})
	if err != nil {
		t.Fatalf("seeding the work bead: %v", err)
	}
	// Claim it the way the hook does, so the row under test is a real
	// in-progress assignment rather than a hand-built status string.
	inProgress := "in_progress"
	if err := f.store.Update(work.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &f.sessName}); err != nil {
		t.Fatalf("claiming the work bead: %v", err)
	}
	claimed, err := f.store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-reading the claimed work bead: %v", err)
	}
	f.work = claimed
	if err := f.sp.Start(context.Background(), f.sessName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("starting the fake session: %v", err)
	}
	return f
}

// claimWork seeds a step bead and claims it for the seat the way the hook does,
// so the row under test is a real in-progress assignment rather than a
// hand-built status string.
func (f *executionBackstopFixture) claimWork(t *testing.T, title string) beads.Bead {
	t.Helper()
	work, err := f.store.Create(beads.Bead{
		Title:    title,
		Type:     "task",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "root-1"},
	})
	if err != nil {
		t.Fatalf("seeding the work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := f.store.Update(work.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &f.sessName}); err != nil {
		t.Fatalf("claiming the work bead: %v", err)
	}
	claimed, err := f.store.Get(work.ID)
	if err != nil {
		t.Fatalf("re-reading the claimed work bead: %v", err)
	}
	return claimed
}

// tick runs one reconcile tick of the backstop at the fixture's current clock.
func (f *executionBackstopFixture) tick(t *testing.T) {
	t.Helper()
	sessions, err := loadSessionBeads(f.store)
	if err != nil {
		t.Fatalf("loading session beads: %v", err)
	}
	work, err := f.store.List(beads.ListQuery{Status: "in_progress"})
	if err != nil {
		t.Fatalf("listing work: %v", err)
	}
	stores := make([]beads.Store, len(work))
	refs := make([]string, len(work))
	for i := range work {
		stores[i] = f.store
	}
	nudgeStalledPoolExecution(f.provider, f.cfg, f.store, sessions, work, stores, refs, false, f.now, f.rec,
		func(sessionBead beads.Bead) error {
			f.drained = append(f.drained, strings.TrimSpace(sessionBead.Metadata["session_name"]))
			return nil
		}, &f.stdout, &f.refusals)
}

// tickWithAssignedWorkSnapshot runs the backstop from an intentionally supplied
// assigned-work snapshot while still loading the session bead live. It lets
// rows model the real cached-work race: session metadata is current, but the
// claim snapshot can lag the authoritative work bead.
func (f *executionBackstopFixture) tickWithAssignedWorkSnapshot(t *testing.T, work []beads.Bead) {
	t.Helper()
	sessions, err := loadSessionBeads(f.store)
	if err != nil {
		t.Fatalf("loading session beads: %v", err)
	}
	stores := make([]beads.Store, len(work))
	refs := make([]string, len(work))
	for i := range work {
		stores[i] = f.store
	}
	nudgeStalledPoolExecution(f.provider, f.cfg, f.store, sessions, work, stores, refs, false, f.now, f.rec,
		func(sessionBead beads.Bead) error {
			f.drained = append(f.drained, strings.TrimSpace(sessionBead.Metadata["session_name"]))
			return nil
		}, &f.stdout, &f.refusals)
}

func (f *executionBackstopFixture) pin(t *testing.T) {
	t.Helper()
	if err := f.store.SetMetadataBatch(f.session.ID, map[string]string{"pin_awake": "true"}); err != nil {
		t.Fatalf("pinning the session bead: %v", err)
	}
}

// idleFor backdates the runtime's last-activity so the predicate observes an
// agent that has done nothing for d.
func (f *executionBackstopFixture) idleFor(t *testing.T, d time.Duration) {
	t.Helper()
	f.sp.SetActivity(f.sessName, f.now.Add(-d))
}

func (f *executionBackstopFixture) sessionMeta(t *testing.T, key string) string {
	t.Helper()
	current, err := f.store.Get(f.session.ID)
	if err != nil {
		t.Fatalf("re-reading the session bead: %v", err)
	}
	return strings.TrimSpace(current.Metadata[key])
}

func (f *executionBackstopFixture) nudgeCount() int {
	return strings.Count(f.stdout.String(), "execution-claim-nudge: nudged")
}

// TestExecutionBackstopNudgesAnIdleClaimHolderExactlyOnce is the core row: the
// slot holds an in-progress claim, the provider reports no activity past the
// grace, and the configured claim nudge is re-delivered once with the attempt
// durably reserved first.
func TestExecutionBackstopNudgesAnIdleClaimHolderExactlyOnce(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.idleFor(t, 10*time.Minute)

	f.tick(t) // first sighting: start the grace clock, do not nudge
	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges after the first sighting = %d, want 0 (observe first)", got)
	}
	if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got != f.work.ID {
		t.Fatalf("persisted work marker = %q, want %q", got, f.work.ID)
	}

	f.now = f.now.Add(idleClaimNudgeGrace + time.Second)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if got := f.nudgeCount(); got != 1 {
		t.Fatalf("nudges after the grace = %d, want exactly 1; stdout=%s", got, f.stdout.String())
	}
	if got := f.sessionMeta(t, executionClaimNudgeCountKey); got != "1" {
		t.Fatalf("persisted attempt count = %q, want 1", got)
	}

	// Inside the backoff nothing else is delivered.
	f.now = f.now.Add(time.Second)
	f.tick(t)
	if got := f.nudgeCount(); got != 1 {
		t.Fatalf("nudges inside the backoff = %d, want still 1", got)
	}
}

// Control A: a WORKING agent. The provider reports recent activity, so the
// predicate holds — zero nudges and, just as importantly, zero writes, because a
// backstop that marks a busy session is a backstop that will eventually nudge it
// (the #312 churn failure this shape exists to avoid).
func TestExecutionBackstopIsSilentForAWorkingAgent(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.idleFor(t, time.Second)

	for i := 0; i < 5; i++ {
		f.tick(t)
		f.now = f.now.Add(idleClaimNudgeGrace + time.Minute)
		f.idleFor(t, time.Second)
	}

	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges to a working agent = %d, want 0; stdout=%s", got, f.stdout.String())
	}
	if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got != "" {
		t.Fatalf("persisted work marker = %q, want no write at all for a working agent", got)
	}
	if got := f.sessionMeta(t, executionClaimNudgeAtKey); got != "" {
		t.Fatalf("persisted timestamp = %q, want no write at all for a working agent", got)
	}
}

// Control B: the claim closes mid-grace. The marker is cleared and no nudge is
// ever delivered — a different outcome from Control A (which writes nothing at
// all) and from the core row (which nudges).
func TestExecutionBackstopClearsWhenTheClaimCompletes(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got == "" {
		t.Fatal("the grace clock did not start")
	}

	if err := f.store.Close(f.work.ID); err != nil {
		t.Fatalf("closing the claimed work: %v", err)
	}
	f.now = f.now.Add(idleClaimNudgeGrace + time.Second)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)

	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges after the claim completed = %d, want 0", got)
	}
	if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got != "" {
		t.Fatalf("persisted work marker = %q, want cleared", got)
	}
}

// Control C: a controller restart mid-backoff. The state machine lives on the
// session bead, so a fresh process resumes the same attempt count instead of
// replaying the sequence from zero — the #312/test-5il regression, which is what
// a purely in-memory grace map produced on every restart.
func TestExecutionBackstopDoesNotReplayAfterAControllerRestart(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	f.now = f.now.Add(idleClaimNudgeGrace + time.Second)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if got := f.nudgeCount(); got != 1 {
		t.Fatalf("nudges before the restart = %d, want 1", got)
	}

	// A restart keeps the store and the runtime; only in-process state is lost.
	f.stdout.Reset()
	f.now = f.now.Add(time.Second)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)

	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges immediately after a restart = %d, want 0 (the backoff is persisted)", got)
	}
	if got := f.sessionMeta(t, executionClaimNudgeCountKey); got != "1" {
		t.Fatalf("attempt count after a restart = %q, want the persisted 1", got)
	}
}

// TestExecutionBackstopEscalatesOnceWhenAttemptsAreExhausted: after the bounded
// attempts, the stall becomes an observable typed fact and the session is handed
// to the drain path that already converges (recycle -> dead-assignee reopen).
// Both happen exactly once, however many ticks follow.
func TestExecutionBackstopEscalatesOnceWhenAttemptsAreExhausted(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	for i := 0; i < idleClaimNudgeMaxAttempts; i++ {
		f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
		f.idleFor(t, 10*time.Minute)
		f.tick(t)
	}
	if got := f.nudgeCount(); got != idleClaimNudgeMaxAttempts {
		t.Fatalf("delivered nudges = %d, want the attempt cap %d; stdout=%s", got, idleClaimNudgeMaxAttempts, f.stdout.String())
	}

	for i := 0; i < 3; i++ {
		f.now = f.now.Add(idleClaimNudgeBackoff)
		f.idleFor(t, 10*time.Minute)
		f.tick(t)
	}

	var stalled []events.Event
	for _, e := range f.rec.Events {
		if e.Type == events.ExecutionStepStalled {
			stalled = append(stalled, e)
		}
	}
	if len(stalled) != 1 {
		t.Fatalf("execution.step_stalled events = %d, want exactly 1", len(stalled))
	}
	if stalled[0].Subject != f.work.ID || stalled[0].SessionID != f.session.ID {
		t.Fatalf("stalled event = subject %q session %q, want the claimed bead and its holder", stalled[0].Subject, stalled[0].SessionID)
	}
	if len(f.drained) != 1 || f.drained[0] != f.sessName {
		t.Fatalf("drain requests = %v, want exactly one for %s", f.drained, f.sessName)
	}
	if got := f.nudgeCount(); got != idleClaimNudgeMaxAttempts {
		t.Fatalf("delivered nudges after exhaustion = %d, want no further delivery", got)
	}
}

// TestExecutionStepStalledStaysOffTheExportAllowlist is the explicit egress
// decision, pinned rather than implied. execution.step_stalled is a controller
// liveness signal, not one of the four execution FACTS the export contract
// carries (which are validated on both sides for ref+run_id+step topology it
// does not have). Widening the default-deny allowlist is a separate, reviewable
// change; this row fails if it happens silently.
func TestExecutionStepStalledStaysOffTheExportAllowlist(t *testing.T) {
	if eventexport.IsAllowed(events.ExecutionStepStalled) {
		t.Fatal("execution.step_stalled is on the redacted-export allowlist; that is an egress-surface change and needs its own review")
	}
}

// A quiet claim cannot be drained when no nudge could be delivered.
func TestExecutionBackstopHoldsWhenTheAgentHasNoNudgeConfigured(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.cfg.Agents[0].Nudge = "" // the maintainer-city pool templates, verbatim
	f.idleFor(t, 10*time.Minute)

	f.tick(t) // first sighting: start the grace clock, escalate nothing yet
	if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got != f.work.ID {
		t.Fatalf("observed work marker = %q, want %q", got, f.work.ID)
	}
	if len(f.drained) != 0 {
		t.Fatalf("drain requests inside the grace window = %v, want none", f.drained)
	}

	f.now = f.now.Add(idleClaimNudgeGrace + time.Second)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)

	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("delivered nudges with no configured nudge = %d, want 0", got)
	}
	if len(f.drained) != 0 {
		t.Fatalf("drain requests = %v, want none; stdout=%s", f.drained, f.stdout.String())
	}
	if f.sessionMeta(t, executionClaimNudgeStalledKey) == "" {
		t.Fatal("escalation was not latched; a later tick would drain the session all over again")
	}

	// Latched: further ticks hold the claim and emit no duplicate event.
	for i := 0; i < 3; i++ {
		f.now = f.now.Add(idleClaimNudgeBackoff)
		f.idleFor(t, 10*time.Minute)
		f.tick(t)
	}
	if len(f.drained) != 0 {
		t.Fatalf("drain requests after further ticks = %v, want none", f.drained)
	}
	stalled := 0
	for _, e := range f.rec.Events {
		if e.Type == events.ExecutionStepStalled {
			stalled++
		}
	}
	if stalled != 1 {
		t.Fatalf("execution.step_stalled events = %d, want exactly 1", stalled)
	}
}

func TestExecutionBackstopSkipsPinnedHumanCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pinned     bool
		checkpoint bool
		wantNudges int
	}{
		{
			name:       "pinned checkpoint hold",
			pinned:     true,
			checkpoint: true,
			wantNudges: 0,
		},
		{
			name:       "checkpoint without pin still recovers",
			checkpoint: true,
			wantNudges: 1,
		},
		{
			name:       "pin without checkpoint still recovers",
			pinned:     true,
			wantNudges: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecutionBackstopFixture(t)
			if tc.pinned {
				if err := f.store.SetMetadataBatch(f.session.ID, map[string]string{"pin_awake": "true"}); err != nil {
					t.Fatalf("pinning the session bead: %v", err)
				}
			}
			if tc.checkpoint {
				if err := f.store.SetMetadataBatch(f.work.ID, map[string]string{
					beadmeta.CheckpointMetadataKey: "round2-v2-explainer-v1 sent 2026-09-24T06:3xZ",
					"gc.checkpoint_reminder":       "next 04:12Z Fri",
				}); err != nil {
					t.Fatalf("marking the work bead as a checkpoint hold: %v", err)
				}
			}
			f.idleFor(t, 10*time.Minute)

			ticks := 2
			if tc.pinned && tc.checkpoint {
				ticks = idleClaimNudgeMaxAttempts + 3
			}
			for i := 0; i < ticks; i++ {
				f.tick(t)
				f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
				f.idleFor(t, 10*time.Minute)
			}

			if got := f.nudgeCount(); got != tc.wantNudges {
				t.Fatalf("nudges = %d, want %d; stdout=%s", got, tc.wantNudges, f.stdout.String())
			}
			if tc.wantNudges == 0 {
				if len(f.drained) != 0 {
					t.Fatalf("drain requests for a pinned checkpoint hold = %v, want none", f.drained)
				}
				if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got != "" {
					t.Fatalf("persisted work marker = %q, want no pacing write while a pinned checkpoint is held", got)
				}
			}
		})
	}
}

func TestExecutionBackstopHoldsOnLivePinnedCheckpointWhenSnapshotIsStale(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.pin(t)
	staleWork := f.work
	if err := f.store.SetMetadataBatch(f.work.ID, map[string]string{
		beadmeta.CheckpointMetadataKey: "afik-review",
	}); err != nil {
		t.Fatalf("marking the live work bead as a checkpoint hold: %v", err)
	}
	f.idleFor(t, 10*time.Minute)

	for i := 0; i < idleClaimNudgeMaxAttempts+3; i++ {
		f.tickWithAssignedWorkSnapshot(t, []beads.Bead{staleWork})
		f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
		f.idleFor(t, 10*time.Minute)
	}

	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges with a live pinned checkpoint = %d, want 0; stdout=%s", got, f.stdout.String())
	}
	if len(f.drained) != 0 {
		t.Fatalf("drain requests with a live pinned checkpoint = %v, want none", f.drained)
	}
	if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got != "" {
		t.Fatalf("persisted work marker = %q, want no pacing write while the live checkpoint is held", got)
	}
	if got := f.sessionMeta(t, executionClaimNudgeStalledKey); got != "" {
		t.Fatalf("stalled latch = %q, want none while the live checkpoint is held", got)
	}
}

func TestExecutionBackstopResumesWhenLiveCheckpointClearsDespiteStaleSnapshot(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.pin(t)
	if err := f.store.SetMetadataBatch(f.work.ID, map[string]string{
		beadmeta.CheckpointMetadataKey: "afik-review",
	}); err != nil {
		t.Fatalf("marking the work bead as a checkpoint hold: %v", err)
	}
	staleWork, err := f.store.Get(f.work.ID)
	if err != nil {
		t.Fatalf("reading the checkpointed snapshot: %v", err)
	}
	if err := f.store.SetMetadata(f.work.ID, beadmeta.CheckpointMetadataKey, ""); err != nil {
		t.Fatalf("clearing the live checkpoint: %v", err)
	}
	if err := f.store.SetMetadataBatch(f.session.ID, map[string]string{
		executionClaimNudgeWorkKey:     f.work.ID,
		executionClaimNudgeRootKey:     f.work.Metadata[beadmeta.RootBeadIDMetadataKey],
		executionClaimNudgeStoreRefKey: "city",
		executionClaimNudgeCountKey:    "0",
		executionClaimNudgeAtKey:       f.now.Add(-idleClaimNudgeGrace - time.Second).UTC().Format(time.RFC3339),
		executionClaimNudgeStalledKey:  "",
	}); err != nil {
		t.Fatalf("seeding the existing execution marker: %v", err)
	}
	f.idleFor(t, 10*time.Minute)

	f.tickWithAssignedWorkSnapshot(t, []beads.Bead{staleWork})

	if got := f.nudgeCount(); got != 1 {
		t.Fatalf("nudges after the live checkpoint cleared = %d, want 1; stdout=%s", got, f.stdout.String())
	}
	if len(f.drained) != 0 {
		t.Fatalf("drain requests after the live checkpoint cleared = %v, want none", f.drained)
	}
	if got := f.sessionMeta(t, executionClaimNudgeCountKey); got != "1" {
		t.Fatalf("persisted attempt count = %q, want 1 after the ordinary cadence resumes", got)
	}
}

func TestExecutionBackstopDoesNotDrainExhaustedPinnedLiveCheckpoint(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.pin(t)
	staleWork := f.work
	if err := f.store.SetMetadataBatch(f.work.ID, map[string]string{
		beadmeta.CheckpointMetadataKey: "afik-review",
	}); err != nil {
		t.Fatalf("marking the live work bead as a checkpoint hold: %v", err)
	}
	if err := f.store.SetMetadataBatch(f.session.ID, map[string]string{
		executionClaimNudgeWorkKey:     f.work.ID,
		executionClaimNudgeRootKey:     f.work.Metadata[beadmeta.RootBeadIDMetadataKey],
		executionClaimNudgeStoreRefKey: "city",
		executionClaimNudgeCountKey:    strconv.Itoa(idleClaimNudgeMaxAttempts),
		executionClaimNudgeAtKey:       f.now.Add(-idleClaimNudgeBackoff - time.Second).UTC().Format(time.RFC3339),
		executionClaimNudgeStalledKey:  "",
	}); err != nil {
		t.Fatalf("seeding an exhausted execution marker: %v", err)
	}
	f.idleFor(t, 10*time.Minute)

	f.tickWithAssignedWorkSnapshot(t, []beads.Bead{staleWork})

	if len(f.drained) != 0 {
		t.Fatalf("drain requests for an exhausted live pinned checkpoint = %v, want none", f.drained)
	}
	if got := f.sessionMeta(t, executionClaimNudgeStalledKey); got != "" {
		t.Fatalf("stalled latch = %q, want none while the live checkpoint is held", got)
	}
	for _, e := range f.rec.Events {
		if e.Type == events.ExecutionStepStalled {
			t.Fatalf("execution.step_stalled event was emitted while the live checkpoint is held: %+v", e)
		}
	}
}

// A peer's claim under the same bare seat identity cannot hold the local
// execution lane in its multiple-claims ambiguity state.
func TestExecutionClaimOwnerFenceLocalBesideForeign(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.cfg.Federation.Identity = "jadegate"
	if err := f.store.SetMetadataBatch(f.session.ID, map[string]string{"alias": "mayor"}); err != nil {
		t.Fatal(err)
	}
	assignee := "mayor"
	if err := f.store.Update(f.work.ID, beads.UpdateOpts{Assignee: &assignee, Labels: []string{"owner:jadegate"}}); err != nil {
		t.Fatal(err)
	}
	foreign := f.claimWork(t, "foreign claim")
	if err := f.store.Update(foreign.ID, beads.UpdateOpts{Assignee: &assignee, Labels: []string{"owner:citadel"}}); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got != f.work.ID {
		t.Errorf("governing work=%q, want local %q", got, f.work.ID)
	}
	f.now = f.now.Add(idleClaimNudgeGrace + time.Second)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	if got := f.nudgeCount(); got != 1 {
		t.Errorf("nudges=%d, want 1 for only the local claim; %s", got, f.stdout.String())
	}
	if got := strings.Count(logs.String(), "execution-claim-nudge: cross-city-fence refused bead="+foreign.ID+" owner=citadel this_identity=jadegate missing=handoff:jadegate"); got != 1 {
		t.Errorf("refusal lines=%d, want 1 across ticks; %s", got, logs.String())
	}
}

func TestExecutionClaimOwnerFence(t *testing.T) {
	for _, tc := range []struct {
		name, identity string
		labels         []string
		want           int
	}{
		{"foreign", "jadegate", []string{"owner:citadel"}, 0},
		{"local", "jadegate", []string{"owner:jadegate"}, 1},
		{"unowned", "jadegate", nil, 1},
		{"handoff", "jadegate", []string{"owner:citadel", "handoff:jadegate"}, 1},
		{"unfederated", "", []string{"owner:citadel"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecutionBackstopFixture(t)
			f.cfg.Federation.Identity = tc.identity
			if err := f.store.Update(f.work.ID, beads.UpdateOpts{Labels: tc.labels}); err != nil {
				t.Fatal(err)
			}
			f.idleFor(t, 10*time.Minute)
			f.tick(t)
			f.now = f.now.Add(idleClaimNudgeGrace + time.Second)
			f.idleFor(t, 10*time.Minute)
			f.tick(t)
			if got := f.nudgeCount(); got != tc.want {
				t.Errorf("nudges=%d, want %d; %s", got, tc.want, f.stdout.String())
			}
		})
	}
}

func TestExecutionClaimOwnerFenceRevalidate(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.cfg.Federation.Identity = "jadegate"
	if err := f.store.Update(f.work.ID, beads.UpdateOpts{Labels: []string{"owner:citadel"}}); err != nil {
		t.Fatal(err)
	}
	p := poolExecutionBackstop{cfg: f.cfg, now: f.now}
	if got := p.revalidate(backstopTarget{ID: f.work.ID, Assignee: f.sessName, Store: f.store}); got != backstopResolutionClear {
		t.Errorf("revalidation=%v, want clear for changed foreign owner", got)
	}
}

// Failed delivery reservations are not evidence that the agent heard a nudge.
func TestExecutionBackstopHoldsWhenNudgeDeliveryIsUnsupported(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.cfg.Agents[0].Provider = "claude-opus"
	f.sp.NudgeErrors = map[string]error{f.sessName: errors.New("live delivery is unsupported for claude-opus")}
	f.idleFor(t, 10*time.Minute)
	for i := 0; i < idleClaimNudgeMaxAttempts+4; i++ {
		f.tick(t)
		f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
		f.idleFor(t, 10*time.Minute)
	}
	if len(f.drained) != 0 {
		t.Fatalf("drain requests = %v with zero delivered nudges; stdout=%s", f.drained, f.stdout.String())
	}
	if f.sessionMeta(t, executionClaimNudgeStalledKey) == "" {
		t.Fatal("undeliverable nudge was not latched")
	}
	if got := len(f.rec.Events); got != 1 {
		t.Fatalf("events = %d, want one stalled event", got)
	}
	if strings.Count(f.stdout.String(), "quiet for 600 s, 0 of 3 nudges deliverable (provider claude-opus); holding claim") != 1 || strings.Contains(f.stdout.String(), "unexecuted") {
		t.Fatalf("log must describe delivery evidence: %s", f.stdout.String())
	}
}

// The fixture vouches only for successful sends to its running fake session.
type executionDeliveryRuntime struct{ *runtime.Fake }

func (p executionDeliveryRuntime) NudgeDelivered(name string, content []runtime.ContentBlock) (runtime.NudgeDelivery, error) {
	err := p.Nudge(name, content)
	return runtime.NudgeDelivery{Delivered: err == nil && p.IsRunning(name)}, err
}

func TestExecutionBackstopRequiresPositiveDeliveryEvidence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		receipt    runtime.NudgeDelivery
		err        error
		legacy     bool
		wantDrains int
	}{
		{name: "zero receipt"},
		{name: "legacy nil is not proof", legacy: true},
		{name: "landed despite error", receipt: runtime.NudgeDelivery{Bytes: 1, Submit: runtime.NudgeSubmitUnconfirmed}, err: errors.New("submit failed"), wantDrains: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecutionBackstopFixture(t)
			var base runtime.Provider = executionReceiptRuntime{Fake: f.sp, receipt: tc.receipt, err: tc.err}
			if tc.legacy {
				base = f.sp
			}
			f.provider = newBoundedStatusProvider(base)
			for i := 0; i < idleClaimNudgeMaxAttempts+3; i++ {
				f.idleFor(t, 10*time.Minute)
				f.tick(t)
				f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
			}
			if len(f.drained) != tc.wantDrains {
				t.Fatalf("drains = %v, want %d; %s", f.drained, tc.wantDrains, f.stdout.String())
			}
			if len(f.rec.Events) != 1 {
				t.Fatalf("events = %d, want 1", len(f.rec.Events))
			}
			var payload events.ExecutionStepStalledPayload
			if err := json.Unmarshal(f.rec.Events[0].Payload, &payload); err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.wantDrains > 0 {
				want = idleClaimNudgeMaxAttempts
			}
			if payload.Attempts != want {
				t.Fatalf("reported deliveries = %d, want %d", payload.Attempts, want)
			}
		})
	}
}

type executionReceiptRuntime struct {
	*runtime.Fake
	receipt runtime.NudgeDelivery
	err     error
}

func (p executionReceiptRuntime) NudgeDelivered(string, []runtime.ContentBlock) (runtime.NudgeDelivery, error) {
	return p.receipt, p.err
}

func TestExecutionBackstopOneDeliveredNudgeAllowsDrain(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.idleFor(t, 10*time.Minute)
	f.tick(t)
	for i := 0; i < idleClaimNudgeMaxAttempts+2; i++ {
		f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
		f.idleFor(t, 10*time.Minute)
		if i > 0 {
			f.sp.NudgeErrors = map[string]error{f.sessName: errors.New("unavailable")}
		}
		f.tick(t)
	}
	if len(f.drained) != 1 {
		t.Fatalf("drains = %v, want one", f.drained)
	}
	if !strings.Contains(f.stdout.String(), "1 of 3 nudges delivered") {
		t.Fatalf("delivery count lost: %s", f.stdout.String())
	}
}
