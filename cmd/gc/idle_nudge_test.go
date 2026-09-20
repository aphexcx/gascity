package main

import (
	"bytes"
	"context"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	testTriggerBeadIDKey       = "gc.trigger_bead_id"
	testTriggerBeadStoreRefKey = "gc.trigger_bead_store_ref"
)

func idleClaimTestCfg() *config.City {
	return &config.City{Agents: []config.Agent{{
		Name:  "agent-a",
		Nudge: "Run gc hook --claim --json now; if it returns work, execute the claimed formula immediately.",
	}}}
}

func idleClaimPoolSession() beads.Bead {
	return beads.Bead{
		ID:     "session-bead-a",
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"session_name":       "session-a",
			"pool_managed":       "true",
			"template":           "agent-a",
			testTriggerBeadIDKey: "work-a",
		},
	}
}

//nolint:unparam // sessionName is always "session-a" today; kept as a param so new cases can vary it.
func runningIdleClaimFake(t *testing.T, sessionName string) *runtime.Fake {
	t.Helper()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatalf("fake start: %v", err)
	}
	return sp
}

func mustGetTestBead(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", id, err)
	}
	return b
}

// A bound pool trigger must obey the same owner fence as the claim hook.
func TestPoolClaimBackstopOwnerFence(t *testing.T) {
	for _, assignment := range []string{"routed", "assigned"} {
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
			t.Run(assignment+"/"+tc.name, func(t *testing.T) {
				sp := runningIdleClaimFake(t, "session-a")
				cfg := idleClaimTestCfg()
				cfg.Federation.Identity = tc.identity
				session := idleClaimPoolSession()
				work := beads.Bead{ID: "work-a", Status: "open", Type: "task", Labels: tc.labels, Metadata: map[string]string{"gc.routed_to": "agent-a"}}
				if assignment == "assigned" {
					work.Assignee = "agent-a"
				}
				store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
				now := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
				var out bytes.Buffer
				for tick := 0; tick < 2; tick++ {
					nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{mustGetTestBead(t, store, session.ID)}, []beads.Bead{work}, []string{"city"}, now, &out, nil)
					now = now.Add(idleClaimNudgeGrace + time.Second)
				}
				if got := sp.CountCalls("Nudge", "session-a"); got != tc.want {
					t.Errorf("nudges = %d, want %d; %s", got, tc.want, out.String())
				}
				if tc.want == 0 && mustGetTestBead(t, store, session.ID).Metadata[idleClaimNudgeTriggerKey] != "" {
					t.Error("foreign row entered the nudge ladder")
				}
			})
		}
	}
}

// Duplicate sightings and later ticks must not create a refusal log storm.
func TestPoolClaimBackstopOwnerFenceLogsBounded(t *testing.T) {
	cfg := idleClaimTestCfg()
	cfg.Federation.Identity = "jadegate"
	session := idleClaimPoolSession()
	sp := runningIdleClaimFake(t, "session-a")
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	work := beads.Bead{ID: "work-a", Status: "open", Labels: []string{"owner:citadel"}}
	var logs, out bytes.Buffer
	var refusals claimRefusalLog
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	now := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	for tick := 0; tick < 3; tick++ {
		nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, []beads.Bead{work, work}, []string{"city", "city:jadegate"}, now.Add(time.Duration(tick)*time.Minute), &out, &refusals)
		if got := strings.Count(logs.String(), "idle-claim-nudge: cross-city-fence refused bead=work-a owner=citadel this_identity=jadegate missing=handoff:jadegate"); got != 1 {
			t.Errorf("tick %d: refusal lines = %d, want 1; %s", tick, got, logs.String())
		}
	}
}

func TestNudgeStalledPoolClaims_NudgesAfterGrace(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-a")
	cfg := idleClaimTestCfg()
	session := idleClaimPoolSession()
	work := []beads.Bead{{ID: "work-a", Status: "open"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	clk := &clock.Fake{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	var out bytes.Buffer

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out, nil)
	if got := sp.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("first tick Nudge calls = %d, want 0 inside grace", got)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeTriggerKey]; got != "work-a" {
		t.Fatalf("idle claim marker trigger = %q, want work-a", got)
	}

	clk.Advance(idleClaimNudgeGrace + time.Second)
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out, nil)
	if got := sp.CountCalls("Nudge", "session-a"); got != 1 {
		t.Fatalf("Nudge calls = %d, want 1 after grace", got)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeCountKey]; got != "1" {
		t.Fatalf("idle claim attempt count = %q, want 1", got)
	}
	if got := session.Metadata[idleClaimNudgeAtKey]; got != clk.Now().UTC().Format(time.RFC3339) {
		t.Fatalf("idle claim last nudge at = %q, want %q", got, clk.Now().UTC().Format(time.RFC3339))
	}
}

func TestNudgeStalledPoolClaims_UsesClaimFallbackWhenNudgeIsBlank(t *testing.T) {
	for _, tc := range []struct {
		name  string
		nudge string
	}{
		{name: "empty", nudge: ""},
		{name: "whitespace", nudge: " \t "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := runningIdleClaimFake(t, "session-a")
			cfg := idleClaimTestCfg()
			cfg.Agents[0].Nudge = tc.nudge
			session := idleClaimPoolSession()
			work := []beads.Bead{{ID: "work-a", Status: "open"}}
			store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			var out bytes.Buffer

			nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, base, &out, nil)
			nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, base.Add(idleClaimNudgeGrace+time.Second), &out, nil)

			for _, call := range sp.SnapshotCalls() {
				if call.Method == "Nudge" && call.Name == "session-a" {
					if got, want := call.Message, defaultPoolClaimNudge; got != want {
						t.Fatalf("fallback nudge = %q, want %q", got, want)
					}
					return
				}
			}
			t.Fatal("fallback nudge was not delivered")
		})
	}
}

func TestPoolClaimBackstopContent_PreservesExplicitNudge(t *testing.T) {
	cfg := idleClaimTestCfg()
	cfg.Agents[0].Nudge = "  Use the configured wake text exactly.  "

	if got, want := (poolClaimBackstop{cfg: cfg}).content(idleClaimPoolSession()), "Use the configured wake text exactly."; got != want {
		t.Fatalf("claim nudge = %q, want normalized configured value %q", got, want)
	}
}

func TestPoolClaimBackstopContent_FailsClosedForUnknownTemplate(t *testing.T) {
	cfg := idleClaimTestCfg()
	session := idleClaimPoolSession()
	session.Metadata["template"] = "unknown-agent"

	if got := (poolClaimBackstop{cfg: cfg}).content(session); got != "" {
		t.Fatalf("claim nudge for unknown template = %q, want empty", got)
	}
}

// Two stores can hold beads with the same ID, so the backstop must resolve the
// slot's trigger through the store ref it was bound to. Here the rig-scoped
// copy is still open (nudge-worthy) while the city-scoped copy of the same ID
// is closed; keying on ID alone would read the wrong bead and stay silent.
func TestNudgeStalledPoolClaims_MatchesTriggerStoreRefForDuplicateIDs(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-a")
	cfg := idleClaimTestCfg()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	session := idleClaimPoolSession()
	session.Metadata[testTriggerBeadStoreRefKey] = "rig:fixture"
	session.Metadata[idleClaimNudgeTriggerKey] = "work-a"
	session.Metadata[idleClaimNudgeCountKey] = "0"
	session.Metadata[idleClaimNudgeAtKey] = base.Format(time.RFC3339)
	work := []beads.Bead{
		{ID: "work-a", Status: "open"},
		{ID: "work-a", Status: "closed"},
	}
	storeRefs := []string{"rig:fixture", "city:test-city"}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	clk := &clock.Fake{Time: base.Add(idleClaimNudgeGrace + time.Second)}
	var out bytes.Buffer

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, storeRefs, clk.Now(), &out, nil)
	if got := sp.CountCalls("Nudge", "session-a"); got != 1 {
		t.Fatalf("Nudge calls = %d, want 1 for the open rig-scoped trigger", got)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeCountKey]; got != "1" {
		t.Fatalf("idle claim attempt count = %q, want 1", got)
	}
}

func TestNudgeStalledPoolClaims_NeverTouchesWorkingSlot(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-a")
	cfg := idleClaimTestCfg()
	session := idleClaimPoolSession()
	session.Metadata[idleClaimNudgeTriggerKey] = "work-a"
	session.Metadata[idleClaimNudgeCountKey] = "1"
	session.Metadata[idleClaimNudgeAtKey] = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	work := []beads.Bead{{ID: "work-a", Status: "in_progress", Assignee: "session-a"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	clk := &clock.Fake{Time: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)}
	var out bytes.Buffer

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out, nil)
	if got := sp.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("working slot Nudge calls = %d, want 0", got)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeTriggerKey]; got != "" {
		t.Fatalf("idle claim marker trigger = %q, want cleared", got)
	}
	if got := session.Metadata[idleClaimNudgeCountKey]; got != "" {
		t.Fatalf("idle claim marker count = %q, want cleared", got)
	}
	if got := session.Metadata[idleClaimNudgeAtKey]; got != "" {
		t.Fatalf("idle claim marker at = %q, want cleared", got)
	}
}

func TestNudgeStalledPoolClaims_GivesUpAtCap(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-a")
	cfg := idleClaimTestCfg()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	session := idleClaimPoolSession()
	session.Metadata[idleClaimNudgeTriggerKey] = "work-a"
	session.Metadata[idleClaimNudgeCountKey] = strconv.Itoa(idleClaimNudgeMaxAttempts)
	session.Metadata[idleClaimNudgeAtKey] = base.Format(time.RFC3339)
	work := []beads.Bead{{ID: "work-a", Status: "open"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	clk := &clock.Fake{Time: base.Add(time.Hour)}
	var out bytes.Buffer

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out, nil)
	if got := sp.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("Nudge calls past cap = %d, want 0", got)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeCountKey]; got != strconv.Itoa(idleClaimNudgeMaxAttempts) {
		t.Fatalf("idle claim attempt count = %q, want cap preserved", got)
	}
}

// The attempt is reserved on the session bead BEFORE delivery, so a nudge the
// provider fails to deliver still consumes one of the bounded attempts. That is
// what stops a slot whose provider is wedged from being re-nudged on every tick
// forever; the cost is that transient delivery failures burn the cap. The
// failing-provider fixture is continuationFailingNudgeProvider
// (continuation_nudge_test.go), shared across both backstop lanes.
func TestNudgeStalledPoolClaims_DeliveryFailureConsumesAttempt(t *testing.T) {
	sp := &continuationFailingNudgeProvider{Provider: runningIdleClaimFake(t, "session-a")}
	cfg := idleClaimTestCfg()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	session := idleClaimPoolSession()
	session.Metadata[idleClaimNudgeTriggerKey] = "work-a"
	session.Metadata[idleClaimNudgeCountKey] = "0"
	session.Metadata[idleClaimNudgeAtKey] = base.Format(time.RFC3339)
	work := []beads.Bead{{ID: "work-a", Status: "open"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	clk := &clock.Fake{Time: base.Add(idleClaimNudgeGrace + time.Second)}
	var out bytes.Buffer

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out, nil)
	if sp.nudgeCalls != 1 {
		t.Fatalf("delivery calls = %d, want 1 failed attempt", sp.nudgeCalls)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeCountKey]; got != "1" {
		t.Fatalf("persisted attempt count = %q, want 1 despite delivery failure", got)
	}
	if got := session.Metadata[idleClaimNudgeAtKey]; got != clk.Now().UTC().Format(time.RFC3339) {
		t.Fatalf("persisted attempt time = %q, want %q", got, clk.Now().UTC().Format(time.RFC3339))
	}

	// The reservation paces the next retry exactly as a delivered nudge would:
	// nothing more is attempted until the backoff elapses.
	clk.Advance(idleClaimNudgeBackoff - time.Second)
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out, nil)
	if sp.nudgeCalls != 1 {
		t.Fatalf("inside-backoff delivery calls = %d, want unchanged 1", sp.nudgeCalls)
	}

	for want := 2; want <= idleClaimNudgeMaxAttempts; want++ {
		session = mustGetTestBead(t, store, session.ID)
		clk.Advance(idleClaimNudgeBackoff + time.Second)
		nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out, nil)
		if sp.nudgeCalls != want {
			t.Fatalf("attempt %d delivery calls = %d, want %d", want, sp.nudgeCalls, want)
		}
	}

	// Every attempt failed, so exhausted() is reached without the trigger ever
	// being claimed: the lane stops attempting and leaves the cap in place.
	session = mustGetTestBead(t, store, session.ID)
	clk.Advance(time.Hour)
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out, nil)
	if sp.nudgeCalls != idleClaimNudgeMaxAttempts {
		t.Fatalf("past-cap delivery calls = %d, want %d", sp.nudgeCalls, idleClaimNudgeMaxAttempts)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeCountKey]; got != strconv.Itoa(idleClaimNudgeMaxAttempts) {
		t.Fatalf("persisted attempt count = %q, want cap %d preserved", got, idleClaimNudgeMaxAttempts)
	}
}

func TestNudgeStalledPoolClaims_SkipsNonPool(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-a")
	cfg := idleClaimTestCfg()
	session := idleClaimPoolSession()
	delete(session.Metadata, "pool_managed")
	work := []beads.Bead{{ID: "work-a", Status: "open"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	clk := &clock.Fake{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	var out bytes.Buffer

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out, nil)
	clk.Advance(time.Hour)
	session = mustGetTestBead(t, store, session.ID)
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out, nil)
	if got := sp.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("non-pool Nudge calls = %d, want 0", got)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeTriggerKey]; got != "" {
		t.Fatalf("non-pool marker trigger = %q, want empty", got)
	}
}
