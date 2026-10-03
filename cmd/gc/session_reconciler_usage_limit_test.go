package main

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func newLivePoolUsageLimitEnv(t *testing.T) (*reconcilerTestEnv, beads.Bead) {
	t.Helper()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1)}}}
	env.addDesired("worker", "worker", true)
	seat := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&seat, map[string]string{
		"state": "active", "pool_managed": "true", "pool_slot": "1",
		"last_woke_at": env.clk.Now().Add(-10 * time.Minute).Format(time.RFC3339),
		"session_key":  "keep-conversation", "started_config_hash": "keep-config", "wake_attempts": "2",
	})
	env.sp.SetPeekOutput("worker", "› Claim the next task\n■ You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at 3:00 PM.")
	return env, seat
}

func TestReconcileSessionBeads_LivePoolUsageLimitParksUntilBackoff(t *testing.T) {
	env, seat := newLivePoolUsageLimitEnv(t)
	work := mustCreateInProgressWork(t, env.store, seat.ID)
	until := env.clk.Now().Add(30 * time.Minute)
	for tick := 0; tick < 3; tick++ {
		if woken := env.reconcile([]beads.Bead{seat}); woken != 0 {
			t.Fatalf("tick %d woke %d sessions during usage-limit backoff", tick, woken)
		}
		if env.sp.IsRunning("worker") {
			t.Fatal("usage-limited pool runtime is still running")
		}
		var err error
		seat, err = env.store.Get(seat.ID)
		if err != nil {
			t.Fatal(err)
		}
		if seat.Status == "closed" || seat.Metadata["state"] != "asleep" || seat.Metadata["sleep_reason"] != "rate_limit" {
			t.Fatalf("usage-limited seat was not parked: %#v", seat)
		}
		if seat.Metadata["quarantined_until"] != until.Format(time.RFC3339) {
			t.Fatalf("quarantine deadline = %q, want stable %s", seat.Metadata["quarantined_until"], until)
		}
		if seat.Metadata["session_key"] != "keep-conversation" || seat.Metadata["started_config_hash"] != "keep-config" || seat.Metadata["wake_attempts"] != "2" {
			t.Fatalf("usage wall reset continuity or counted a crash: %#v", seat.Metadata)
		}
		claimed, err := env.store.Get(work.ID)
		if err != nil || claimed.Assignee != seat.ID || claimed.Status != "in_progress" {
			t.Fatalf("usage wall changed the work claim: %#v, %v", claimed, err)
		}
		env.clk.Advance(5 * time.Minute)
	}
}

func TestReconcileSessionBeads_LivePoolUsageLimitIgnoresHealthyOrProtectedSession(t *testing.T) {
	for _, name := range []string{"old-provider-banner", "successful-response", "attached", "starting", "unmanaged"} {
		t.Run(name, func(t *testing.T) {
			env, seat := newLivePoolUsageLimitEnv(t)
			switch name {
			case "old-provider-banner":
				env.sp.SetPeekOutput("worker", "Earlier the provider said You've hit your limit, Pro plan.\n• The task is building now.")
			case "successful-response":
				env.sp.SetPeekOutput("worker", "■ You've hit your usage limit. Try again later.\n› Retry\n• The task is building now.")
			case "attached":
				env.sp.SetAttached("worker", true)
			case "starting":
				env.setSessionMetadata(&seat, map[string]string{"pending_create_claim": "true", "pending_create_started_at": env.clk.Now().Format(time.RFC3339)})
			case "unmanaged":
				env.setSessionMetadata(&seat, map[string]string{"pool_managed": "", "pool_slot": ""})
			}
			env.reconcile([]beads.Bead{seat})
			if !env.sp.IsRunning("worker") {
				t.Fatal("healthy or protected session was stopped")
			}
			if got := env.sessionInfo(seat.ID); got.SleepReason == "rate_limit" || got.QuarantinedUntil != "" {
				t.Fatalf("healthy or protected session was quarantined: %#v", got)
			}
		})
	}
}

func TestReconcileSessionBeads_LivePoolUsageLimitRetriesFailedStop(t *testing.T) {
	env, seat := newLivePoolUsageLimitEnv(t)
	env.sp.StopErrors["worker"] = errors.New("stop unavailable")
	env.reconcile([]beads.Bead{seat})
	if !env.sp.IsRunning("worker") {
		t.Fatal("failed stop unexpectedly removed the runtime")
	}
	held := env.sessionInfo(seat.ID)
	if held.SleepReason != "rate_limit" || held.QuarantinedUntil == "" {
		t.Fatal("failed stop lost its durable usage-limit hold")
	}
	delete(env.sp.StopErrors, "worker")
	env.sp.SetPeekOutput("worker", "")
	env.clk.Advance(time.Minute)
	seat, err := env.store.Get(seat.ID)
	if err != nil {
		t.Fatal(err)
	}
	env.reconcile([]beads.Bead{seat})
	if env.sp.IsRunning("worker") {
		t.Fatal("held runtime was not stopped on retry")
	}
	if got := env.sessionInfo(seat.ID).QuarantinedUntil; got != held.QuarantinedUntil {
		t.Fatalf("stop retry extended backoff: %q, want %q", got, held.QuarantinedUntil)
	}
}

func TestReconcileSessionBeads_LivePoolUsageLimitPersistsBeforeStop(t *testing.T) {
	env, seat := newLivePoolUsageLimitEnv(t)
	failing := &failRateLimitHoldStore{MemStore: env.store.(*beads.MemStore), failRateLimitHold: true}
	env.store = failing
	env.reconcile([]beads.Bead{seat})
	if !env.sp.IsRunning("worker") {
		t.Fatal("runtime stopped before the hold persisted")
	}
	if got := env.sessionInfo(seat.ID); got.SleepReason != "" || got.LastWokeAt == "" {
		t.Fatalf("failed hold mutated session continuity: %#v", got)
	}
	failing.failRateLimitHold = false
	env.reconcile([]beads.Bead{seat})
	if env.sp.IsRunning("worker") || env.sessionInfo(seat.ID).SleepReason != "rate_limit" {
		t.Fatal("hold was not persisted and runtime stopped on retry")
	}
}

func TestReconcileSessionBeads_LivePoolUsageLimitReplacesCrashBackoff(t *testing.T) {
	env, seat := newLivePoolUsageLimitEnv(t)
	env.setSessionMetadata(&seat, map[string]string{
		"sleep_reason": "quarantine", "quarantined_until": env.clk.Now().Add(time.Minute).Format(time.RFC3339),
	})
	env.reconcile([]beads.Bead{seat})
	got := env.sessionInfo(seat.ID)
	if got.SleepReason != "rate_limit" || got.QuarantinedUntil != env.clk.Now().Add(30*time.Minute).Format(time.RFC3339) {
		t.Fatalf("provider refusal kept the crash backoff instead of its usage-limit hold: reason=%q until=%q", got.SleepReason, got.QuarantinedUntil)
	}
}

func TestReconcileSessionBeads_LivePoolUsageLimitHonorsExplicitRestart(t *testing.T) {
	for _, source := range []string{"bead", "provider"} {
		t.Run(source, func(t *testing.T) {
			env, seat := newLivePoolUsageLimitEnv(t)
			dops := newFakeDrainOps()
			if source == "bead" {
				env.setSessionMetadata(&seat, map[string]string{"restart_requested": "true"})
			} else {
				dops.restartRequested["worker"] = true
			}
			env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{seat}, map[string]int{"worker": 1}, dops)
			got := env.sessionInfo(seat.ID)
			if got.SleepReason == "rate_limit" || got.QuarantinedUntil != "" {
				t.Fatalf("explicit restart was parked behind provider-limit backoff: reason=%q until=%q", got.SleepReason, got.QuarantinedUntil)
			}
			if got.RestartRequested != "" || got.SessionKey != "" || got.StartedConfigHash != "" || env.sp.IsRunning("worker") {
				t.Fatalf("explicit restart did not perform its fresh-session handoff: %+v", got)
			}
			var err error
			seat, err = env.store.Get(seat.ID)
			if err != nil {
				t.Fatal(err)
			}
			if woken := env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{seat}, map[string]int{"worker": 1}, dops); woken != 1 {
				t.Fatalf("explicit restart woke %d sessions on next tick, want 1 without a rate-limit delay", woken)
			}
		})
	}
}

func TestComputePoolDesiredStates_UnclaimedUsageLimitRetainsDemandSlot(t *testing.T) {
	for _, tc := range []struct {
		name       string
		scaleCount int
		elapsed    time.Duration
		retained   bool
		drained    bool
	}{
		{name: "pending claim retains quarantined lane", scaleCount: 1, retained: true},
		{name: "zero demand creates no request", scaleCount: 0},
		{name: "expired quarantine permits fresh lane", scaleCount: 1, elapsed: 30 * time.Minute},
		{name: "drained lane releases demand despite stale quarantine", scaleCount: 1, drained: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, seat := newLivePoolUsageLimitEnv(t)
			env.cfg.Agents[0].WakeMode = "fresh"
			work, err := env.store.Create(beads.Bead{
				Title: "unclaimed routed work", Type: "task",
				Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
			})
			if err != nil {
				t.Fatal(err)
			}
			env.setSessionMetadata(&seat, map[string]string{beadmeta.TriggerBeadIDMetadataKey: work.ID})
			env.reconcile([]beads.Bead{seat})
			if env.sp.IsRunning("worker") {
				t.Fatal("usage-limited runtime was not stopped before the demand census")
			}
			env.clk.Advance(tc.elapsed)
			held := env.sessionInfo(seat.ID)
			if tc.drained {
				held, err = sessionFrontDoor(env.store).ApplyPatchInfo(held, sessionpkg.AcknowledgeDrainPatch(env.clk.Now(), true))
				if err != nil {
					t.Fatal(err)
				}
			}
			states := computePoolDesiredStatesAt(env.cfg, nil, nil, []beads.Bead{work}, []sessionpkg.Info{held},
				map[string]int{"worker": tc.scaleCount},
				map[string]scaleCheckDemand{"worker": {Count: tc.scaleCount, WorkBeadIDs: []string{work.ID}}},
				nil, env.clk.Now(), nil)
			if tc.scaleCount == 0 {
				for _, state := range states {
					if len(state.Requests) != 0 {
						t.Fatalf("zero demand retained requests: %+v", state.Requests)
					}
				}
				return
			}
			if len(states) != 1 || len(states[0].Requests) != 1 {
				t.Fatalf("one routed bead should request one lane: %+v", states)
			}
			req := states[0].Requests[0]
			wantID := ""
			if tc.retained {
				wantID = seat.ID
			}
			if req.SessionBeadID != wantID || req.WorkBeadID != work.ID {
				t.Fatalf("request = %+v, want session %q for work %q", req, wantID, work.ID)
			}
			if !tc.retained {
				return
			}
			bp := &agentBuildParams{city: env.cfg, agents: env.cfg.Agents, beadStore: env.store}
			selected, _, plan, err := selectOrPlanPoolSessionBead(bp, &env.cfg.Agents[0], "worker", &held, req,
				env.clk.Now(), map[string]bool{}, map[int]bool{})
			if err != nil || plan != nil || selected.ID != seat.ID {
				t.Fatalf("quarantined request planned a replacement: selected=%q plan=%+v err=%v", selected.ID, plan, err)
			}
			seat, err = env.store.Get(selected.ID)
			if err != nil {
				t.Fatal(err)
			}
			if woken := env.reconcile([]beads.Bead{seat}); woken != 0 || env.sp.IsRunning("worker") {
				t.Fatalf("selected quarantined lane restarted: woken=%d", woken)
			}
		})
	}
}

func TestComputePoolDesiredStates_UsageLimitOccupiesMinimumCapacity(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		min, max, scaleCount     int
		omitScale, drained       bool
		work                     string
		elapsed                  time.Duration
		wantCount, wantRetained  int
		wantFloor, checkSelected bool
	}{
		{name: "minimum with omitted scale", min: 1, max: 2, omitScale: true, wantCount: 1, wantRetained: 1, wantFloor: true, checkSelected: true},
		{name: "minimum with zero scale", min: 1, max: 2, wantCount: 1, wantRetained: 1, wantFloor: true},
		{name: "minimum at maximum", min: 1, max: 1, wantCount: 1, wantRetained: 1, wantFloor: true},
		{name: "minimum zero creates no demand", max: 2},
		{name: "maximum zero creates no demand"},
		{name: "remaining minimum gets one fresh slot", min: 2, max: 2, wantCount: 2, wantRetained: 1, wantFloor: true},
		{name: "unassigned demand fulfills minimum", min: 1, max: 2, work: "unassigned", scaleCount: 1, wantCount: 1, wantRetained: 1},
		{name: "assigned demand fulfills minimum", min: 1, max: 2, work: "assigned", wantCount: 1, wantRetained: 1},
		{name: "selected demand cannot occupy minimum twice", min: 2, max: 2, work: "unassigned", scaleCount: 1, wantCount: 2, wantRetained: 1},
		{name: "expired hold allows minimum replacement", min: 1, max: 2, elapsed: 30 * time.Minute, wantCount: 1},
		{name: "drained hold allows minimum replacement", min: 1, max: 2, drained: true, wantCount: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, seat := newLivePoolUsageLimitEnv(t)
			env.cfg.Agents[0].WakeMode = "fresh"
			env.cfg.Agents[0].MinActiveSessions = intPtr(tc.min)
			env.cfg.Agents[0].MaxActiveSessions = intPtr(tc.max)
			var owned, assigned []beads.Bead
			demand := map[string]scaleCheckDemand{}
			if tc.work != "" {
				work, err := env.store.Create(beads.Bead{
					Title: "routed work", Type: "task",
					Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
				})
				if err != nil {
					t.Fatal(err)
				}
				if tc.work == "assigned" {
					if err := env.store.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress"), Assignee: &seat.ID}); err != nil {
						t.Fatal(err)
					}
					work, err = env.store.Get(work.ID)
					if err != nil {
						t.Fatal(err)
					}
					assigned = []beads.Bead{work}
				} else {
					demand["worker"] = scaleCheckDemand{Count: tc.scaleCount, WorkBeadIDs: []string{work.ID}}
				}
				owned = []beads.Bead{work}
				env.setSessionMetadata(&seat, map[string]string{beadmeta.TriggerBeadIDMetadataKey: work.ID})
			}
			env.reconcile([]beads.Bead{seat})
			env.clk.Advance(tc.elapsed)
			held := env.sessionInfo(seat.ID)
			if tc.drained {
				var err error
				held, err = sessionFrontDoor(env.store).ApplyPatchInfo(held, sessionpkg.AcknowledgeDrainPatch(env.clk.Now(), true))
				if err != nil {
					t.Fatal(err)
				}
			}
			scale := map[string]int{}
			if !tc.omitScale {
				scale["worker"] = tc.scaleCount
			}
			states := computePoolDesiredStatesAt(env.cfg, assigned, nil, owned, []sessionpkg.Info{held}, scale, demand, nil, env.clk.Now(), nil)
			count, retained := 0, 0
			for _, state := range states {
				for _, req := range state.Requests {
					count++
					if req.SessionBeadID != seat.ID {
						continue
					}
					retained++
					if req.FloorGuarantee != tc.wantFloor {
						t.Fatalf("retained request floor = %v, want %v", req.FloorGuarantee, tc.wantFloor)
					}
					if tc.checkSelected {
						bp := &agentBuildParams{city: env.cfg, agents: env.cfg.Agents, beadStore: env.store}
						selected, _, plan, err := selectOrPlanPoolSessionBead(bp, &env.cfg.Agents[0], "worker", &held, req, env.clk.Now(), map[string]bool{}, map[int]bool{})
						if err != nil || plan != nil || selected.ID != seat.ID {
							t.Fatalf("minimum request planned a replacement: selected=%q plan=%+v err=%v", selected.ID, plan, err)
						}
						seat, err = env.store.Get(selected.ID)
						if err != nil {
							t.Fatal(err)
						}
						if woken := env.reconcile([]beads.Bead{seat}); woken != 0 || env.sp.IsRunning("worker") {
							t.Fatalf("minimum request restarted held lane: woken=%d", woken)
						}
					}
				}
			}
			if count != tc.wantCount || retained != tc.wantRetained {
				t.Fatalf("requests=%+v: count=%d retained=%d, want count=%d retained=%d", states, count, retained, tc.wantCount, tc.wantRetained)
			}
		})
	}
}

func TestComputePoolDesiredStates_MinimumSelectsDistinctUsageLimitedSessions(t *testing.T) {
	for _, minimum := range []int{1, 2} {
		env := newReconcilerTestEnv()
		env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", WakeMode: "fresh", MinActiveSessions: intPtr(minimum), MaxActiveSessions: intPtr(3)}}}
		var infos []sessionpkg.Info
		candidates := make(map[string]bool)
		for _, slot := range []string{"1", "2", "3"} {
			seat := env.createSessionBead("worker-"+slot, "worker")
			env.setSessionMetadata(&seat, map[string]string{
				"pool_managed": "true", "pool_slot": slot,
				"state": "asleep", "sleep_reason": "rate_limit",
				"quarantined_until": env.clk.Now().Add(30 * time.Minute).Format(time.RFC3339),
			})
			infos = append(infos, env.sessionInfo(seat.ID))
			candidates[seat.ID] = true
		}
		states := computePoolDesiredStatesAt(env.cfg, nil, nil, nil, infos, nil, nil, nil, env.clk.Now(), nil)
		if len(states) != 1 || len(states[0].Requests) != minimum {
			t.Fatalf("minimum %d requested extra capacity: %+v", minimum, states)
		}
		used := make(map[string]bool)
		for _, req := range states[0].Requests {
			if !candidates[req.SessionBeadID] || used[req.SessionBeadID] || !req.FloorGuarantee {
				t.Fatalf("minimum %d reused or replaced a quarantined candidate: %+v", minimum, states)
			}
			used[req.SessionBeadID] = true
		}
	}
}
