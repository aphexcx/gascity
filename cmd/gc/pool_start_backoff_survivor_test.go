package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestPoolStartStoreRefNormalization(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "city"},
		{" city ", "city"},
		{" r ", "rig:r"},
		{"city:demo", "city:demo"},
		{"rig:r", "rig:r"},
		{"binding:graph", "binding:graph"},
	} {
		if got := poolStartStoreRef(tc.in, false); got != tc.want {
			t.Errorf("poolStartStoreRef(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPoolStartDeferralsRespectStoreAndLegacyProvenance(t *testing.T) {
	if got := poolStartStoreRef("city", true); got != "rig:city" {
		t.Fatalf("census rig named city = %q, want rig:city", got)
	}
	d := workStartDeferrals{{StoreRef: "city", ID: "X"}: {}}
	for _, tc := range []struct {
		id, ref string
		want    bool
	}{
		{"X", "city", true},
		{"X", "city:demo", true},
		{"X", "rig:r", false},
		{"X", "", true},
		{"Y", "", false},
		{"", "city", false},
	} {
		if got := d.contains(tc.id, tc.ref); got != tc.want {
			t.Errorf("contains(%q, %q) = %v, want %v", tc.id, tc.ref, got, tc.want)
		}
	}
}

func TestComputeAwakeSet_DeferredTriggerAdmissionAndOverrides(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(*AwakeInput, *AwakeSessionBead)
		wake   bool
		reason string
	}{
		{"reset", func(_ *AwakeInput, b *AwakeSessionBead) { b.ContinuationResetPending = true }, false, ""},
		{"deferred pending create", func(_ *AwakeInput, b *AwakeSessionBead) { b.PendingCreate = true }, false, ""},
		{"deferred explicit wake", func(_ *AwakeInput, b *AwakeSessionBead) { b.ExplicitWake = true }, false, ""},
		{"eligible pending create", func(_ *AwakeInput, b *AwakeSessionBead) { b.PendingCreate = true; b.TriggerBeadID = "V" }, true, "pending-create"},
		{"eligible explicit wake", func(_ *AwakeInput, b *AwakeSessionBead) { b.ExplicitWake = true; b.TriggerBeadID = "V" }, true, "explicit-wake"},
		{"attached", func(i *AwakeInput, _ *AwakeSessionBead) { i.AttachedSessions = map[string]bool{"S": true} }, true, "attached"},
		{"pending interaction", func(i *AwakeInput, _ *AwakeSessionBead) { i.PendingSessions = map[string]bool{"S": true} }, true, "pending"},
		{"pin", func(_ *AwakeInput, b *AwakeSessionBead) { b.Pinned = true }, true, "pin"},
		{"ready wait", func(i *AwakeInput, _ *AwakeSessionBead) { i.ReadyWaitSet = map[string]bool{"s": true} }, true, "wait-ready"},
		{"manual", func(_ *AwakeInput, b *AwakeSessionBead) { b.ManualSession = true }, true, "manual"},
		{"named", func(i *AwakeInput, b *AwakeSessionBead) {
			b.NamedIdentity = "named"
			i.NamedSessions = []AwakeNamedSession{{Identity: "named", Template: "helper", Mode: "always"}}
		}, true, "named-always"},
		{"configured named", func(_ *AwakeInput, b *AwakeSessionBead) {
			b.ConfiguredNamedSession = true
			b.ContinuationResetPending = true
		}, true, "reset-pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := AwakeSessionBead{ID: "s", SessionName: "S", Template: "helper", State: "creating", TriggerBeadID: "W", TriggerBeadStoreRef: "city"}
			input := AwakeInput{Agents: []AwakeAgent{{QualifiedName: "helper"}}, DeferredTriggers: workStartDeferrals{{StoreRef: "city", ID: "W"}: {}}}
			tc.setup(&input, &b)
			input.SessionBeads = []AwakeSessionBead{b}
			got := ComputeAwakeSet(input)["S"]
			if got.ShouldWake != tc.wake || (tc.wake && got.Reason != tc.reason) {
				t.Fatalf("decision = %+v, want wake=%v reason=%q", got, tc.wake, tc.reason)
			}
		})
	}
}

type deferredSurvivorFixture struct {
	t      *testing.T
	path   string
	store  *beads.MemStore
	rigs   map[string]beads.Store
	cfg    *config.City
	sp     *runtime.Fake
	clk    *clock.Fake
	stderr bytes.Buffer
}

func newDeferredSurvivorFixture(t *testing.T) *deferredSurvivorFixture {
	t.Helper()
	return &deferredSurvivorFixture{
		t: t, path: t.TempDir(), store: beads.NewMemStore(), sp: runtime.NewFake(),
		clk: &clock.Fake{Time: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)},
		cfg: &config.City{
			Agents:    []config.Agent{{Name: "helper", MaxActiveSessions: intPtr(2), Provider: "mock"}},
			Providers: map[string]config.ProviderSpec{"mock": {Command: "true"}},
			Daemon:    config.DaemonConfig{MaxWakesPerTick: intPtr(1)},
		},
	}
}

func (f *deferredSurvivorFixture) create(store beads.Store, b beads.Bead) beads.Bead {
	f.t.Helper()
	got, err := store.Create(b)
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

func (f *deferredSurvivorFixture) work(store beads.Store, id, template, assignee string, parked bool) beads.Bead {
	f.t.Helper()
	b := beads.Bead{
		ID: id, Title: id, Type: "task", Status: "open", Assignee: assignee,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: template},
	}
	if assignee != "" {
		b.Status = "in_progress"
	}
	if parked {
		b.Metadata[beadmeta.ParkedAtMetadataKey] = f.clk.Now().Format(time.RFC3339)
	}
	got := f.create(store, b)
	if got.Status != b.Status {
		if err := store.Update(got.ID, beads.UpdateOpts{Status: &b.Status}); err != nil {
			f.t.Fatal(err)
		}
	}
	return got
}

func (f *deferredSurvivorFixture) session(name, template, slot, trigger, ref string, pending bool) beads.Bead {
	f.t.Helper()
	metadata := map[string]string{
		"template": template, "session_name": name, "session_name_explicit": "true",
		"pool_slot": slot, "pool_managed": "true", "state": "creating", "generation": "1",
		"continuation_epoch": "1", "instance_token": "token-" + name,
		beadmeta.TriggerBeadIDMetadataKey: trigger, beadmeta.TriggerBeadStoreRefMetadataKey: ref,
	}
	created := f.clk.Now().Add(-time.Hour)
	if pending {
		metadata["pending_create_claim"] = "true"
		created = created.Add(time.Minute)
	}
	return f.create(f.store, beads.Bead{
		Title: template, Type: sessionBeadType, CreatedAt: created,
		Labels: []string{sessionBeadLabel, "template:" + template}, Metadata: metadata,
	})
}

func (f *deferredSurvivorFixture) tick() {
	f.t.Helper()
	snap, err := loadSessionBeadSnapshot(f.store)
	if err != nil {
		f.t.Fatal(err)
	}
	ds := buildDesiredStateWithSessionBeadsAt("gc", f.path, f.clk.Now(), f.clk.Now(), f.cfg, f.sp, f.store, f.rigs, snap, nil, &f.stderr, nil)
	_, demand, refs := poolDemandAssignedWork(f.cfg, f.path, f.store, snap.OpenInfos(), ds.AssignedWorkBeads, ds.AssignedWorkStoreRefs, ds.PoolStartDeferredTriggers)
	counts := PoolDesiredCounts(ComputePoolDesiredStatesDeferring(f.cfg, demand, refs, snap.OpenInfos(), ds.ScaleCheckCounts, nil, ds.PoolStartDeferredTriggers, nil, ds.PoolStartDecisionTime))
	var stdout bytes.Buffer
	reconcileSessionBeadsAtPathWithNamedDemand(
		context.Background(), f.path, snap.OpenForReconcile(), snap, ds.State,
		configuredSessionNamesWithSnapshot(f.cfg, "gc", snap), f.cfg, f.sp, f.store,
		nil, ds.AssignedWorkBeads, f.rigs, nil, newDrainTracker(), nil, counts,
		ds.NamedSessionDemand, ds.NamedSessionRoutedDemand, false, nil, "gc",
		nil, f.clk, events.Discard, 0, 0, &stdout, &f.stderr,
		withStartStabilityWaiter(immediateStartStabilityWaiter),
		withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
		withReadyAssignedFlags(readyAssignedFlagsForBeads(ds.ReadyAssigned, ds.AssignedWorkBeads, ds.AssignedWorkStoreRefs)),
		withPoolStartDeferrals(ds.PoolStartDeferredTriggers, ds.AssignedWorkStoreRefs),
	)
	f.clk.Time = f.clk.Time.Add(time.Minute)
}

func (f *deferredSurvivorFixture) requireStarts(want string) {
	f.t.Helper()
	var starts []string
	for _, call := range f.sp.SnapshotCalls() {
		if call.Method == "Start" {
			starts = append(starts, call.Name)
		}
	}
	if len(starts) != 1 || starts[0] != want {
		f.t.Fatalf("starts = %v, want exactly [%s]\n%s", starts, want, f.stderr.String())
	}
}

func (f *deferredSurvivorFixture) requireTrigger(session beads.Bead, workID string) {
	f.t.Helper()
	got, err := f.store.Get(session.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	if got.Metadata[beadmeta.TriggerBeadIDMetadataKey] != workID {
		f.t.Fatalf("session %s rebound to %q, want original trigger %s\n%s", session.ID, got.Metadata[beadmeta.TriggerBeadIDMetadataKey], workID, f.stderr.String())
	}
}

// r4-1: an assigned-only awake deferral pass loses the scale probe's parked W.
func TestPoolStartBackoff_OpenUnassignedSurvivorIsNotWoken(t *testing.T) {
	f := newDeferredSurvivorFixture(t)
	w := f.work(f.store, "W", "helper", "", true)
	v := f.work(f.store, "V", "helper", "", false)
	s := f.session("helper-1", "helper", "1", w.ID, "city", false)
	f.session("helper-2", "helper", "2", v.ID, "city", true)
	for range 3 {
		f.tick()
		f.requireTrigger(s, w.ID)
	}
	f.requireStarts("helper-2")
}

// r4-2: reuse must recognize both the current and historical actor alias.
func TestPoolStartBackoff_AliasOwnedSurvivorIsNotReused(t *testing.T) {
	for _, field := range []string{"alias", "alias_history"} {
		t.Run(field, func(t *testing.T) {
			f := newDeferredSurvivorFixture(t)
			w := f.work(f.store, "W", "helper", "helper.actor", true)
			f.work(f.store, "V", "helper", "", false)
			s := f.session("helper-1", "helper", "1", w.ID, "city", false)
			if err := f.store.Update(s.ID, beads.UpdateOpts{Metadata: map[string]string{field: "helper.actor"}}); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				f.tick()
				f.requireTrigger(s, w.ID)
			}
			f.requireStarts("helper-2-pool")
			got, err := f.store.Get(w.ID)
			if err != nil || got.Assignee != "helper.actor" || got.Status != "in_progress" {
				t.Fatalf("assigned work changed: %+v, %v", got, err)
			}
			for _, call := range f.sp.SnapshotCalls() {
				if call.Method == "Start" && call.Name == "helper-1" {
					t.Fatalf("alias-owned survivor was started: %+v\n%s", call, f.stderr.String())
				}
			}
		})
	}
}

// r4-3: city X's deferral must not remove rig X's in-flight session or wake.
func TestPoolStartBackoff_SameIDInIndependentStoresKeepsRigSessionEligible(t *testing.T) {
	f := newDeferredSurvivorFixture(t)
	rig := beads.NewMemStore()
	f.rigs = map[string]beads.Store{"r": rig}
	rigPath := t.TempDir()
	f.cfg.Rigs = []config.Rig{{Name: "r", Path: rigPath}}
	f.cfg.Agents[0].Dir = "r"
	f.cfg.Agents[0].WorkDir = rigPath
	cityWork := f.work(f.store, "X", "r/helper", "r-helper-2", true)
	w := f.work(rig, "X", "r/helper", "", false)
	if cityWork.ID != w.ID {
		t.Fatalf("fixture requires independent stores sharing an ID: %s != %s", cityWork.ID, w.ID)
	}
	f.session("r-helper-2", "r/helper", "2", cityWork.ID, "city", false)
	s := f.session("r-helper-1", "r/helper", "1", w.ID, "rig:r", false)
	for range 3 {
		f.tick()
		f.requireTrigger(s, w.ID)
	}
	f.requireStarts("r-helper-1")
	for _, call := range f.sp.SnapshotCalls() {
		if call.Method == "Start" && call.Config.WorkDir != rigPath {
			t.Fatalf("rig session started in %q, want %q", call.Config.WorkDir, rigPath)
		}
	}
	got, err := f.store.Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey] != "rig:r" {
		t.Fatalf("rig session lost its trigger store: %v", got.Metadata)
	}
}

// r4-4: a committed fresh reset cannot bypass the trigger's park.
func TestPoolStartBackoff_CommittedResetForParkedTriggerDoesNotWake(t *testing.T) {
	f := newDeferredSurvivorFixture(t)
	w := f.work(f.store, "W", "helper", "helper-1", true)
	v := f.work(f.store, "V", "helper", "", false)
	s := f.session("helper-1", "helper", "1", w.ID, "city", false)
	f.session("helper-2", "helper", "2", v.ID, "city", true)
	if err := f.store.Update(s.ID, beads.UpdateOpts{Metadata: map[string]string{
		"continuation_reset_pending": "true", "reset_committed_at": f.clk.Now().Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		f.tick()
		f.requireTrigger(s, w.ID)
	}
	f.requireStarts("helper-2")
	got, err := f.store.Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("parked session lost its fresh-reset intent: %v", got.Metadata)
	}
}

// A legacy session without a recorded trigger still owns work via its aliases.
func TestPoolStartBackoff_LegacyAliasOwnerIsNotRebound(t *testing.T) {
	for _, tc := range []struct{ field, state string }{
		{"alias", "creating"}, {"alias_history", "creating"}, {"alias", "active"}, {"alias_history", "active"},
	} {
		t.Run(tc.field+"/"+tc.state, func(t *testing.T) {
			f := newDeferredSurvivorFixture(t)
			w := f.work(f.store, "W", "helper", "helper.actor", true)
			f.work(f.store, "V", "helper", "", false)
			s := f.session("helper-1", "helper", "1", "", "", false)
			if err := f.store.Update(s.ID, beads.UpdateOpts{Metadata: map[string]string{tc.field: "helper.actor", "state": tc.state}}); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				f.tick()
				f.requireTrigger(s, "")
			}
			got, err := f.store.Get(w.ID)
			if err != nil || got.Assignee != "helper.actor" || got.Status != "in_progress" {
				t.Fatalf("assigned work changed: %+v, %v", got, err)
			}
		})
	}
}

func TestPoolStartBackoff_DeferredSurvivorWithOtherAssignedWork(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "heartbeat_hold"}[held], func(t *testing.T) {
			f := newDeferredSurvivorFixture(t)
			w := f.work(f.store, "W", "helper", "", true)
			v := f.work(f.store, "V", "helper", "helper-1", false)
			s := f.session("helper-1", "helper", "1", w.ID, "city", false)
			if held {
				if err := f.store.Update(s.ID, beads.UpdateOpts{Metadata: map[string]string{"held_until": f.clk.Now().Add(time.Hour).Format(time.RFC3339)}}); err != nil {
					t.Fatal(err)
				}
			}
			for range 3 {
				f.tick()
				f.requireTrigger(s, w.ID)
			}
			for _, call := range f.sp.SnapshotCalls() {
				if call.Method == "Start" {
					t.Fatalf("deferred owner started: %+v\n%s", call, f.stderr.String())
				}
			}
			got, err := f.store.Get(v.ID)
			if err != nil || got.Assignee != "helper-1" || got.Status != "in_progress" {
				t.Fatalf("assigned work changed: %+v, %v", got, err)
			}
		})
	}
}

func TestComputeAwakeSet_DeferredSurvivorDoesNotMaskEligibleCapacity(t *testing.T) {
	for _, mode := range []string{"min_active", "assigned_scale"} {
		t.Run(mode, func(t *testing.T) {
			input := AwakeInput{
				Agents: []AwakeAgent{{QualifiedName: "helper"}},
				SessionBeads: []AwakeSessionBead{
					{ID: "s", SessionName: "S", Template: "helper", State: "creating", TriggerBeadID: "W", TriggerBeadStoreRef: "city"},
					{ID: "t", SessionName: "T", Template: "helper", State: "creating", TriggerBeadID: "V", TriggerBeadStoreRef: "city"},
				},
				DeferredTriggers: workStartDeferrals{{StoreRef: "city", ID: "W"}: {}},
			}
			if mode == "min_active" {
				input.Agents[0].MinActiveSessions = 1
				input.SessionBeads[1].State = "asleep"
				input.SessionBeads[1].SleepReason = "city-stop"
			} else {
				input.ScaleCheckCounts = map[string]int{"helper": 1}
				input.WorkBeads = []AwakeWorkBead{{ID: "other", Assignee: "S", Status: "in_progress"}}
			}
			got := ComputeAwakeSet(input)
			if got["S"].ShouldWake || !got["T"].ShouldWake {
				t.Fatalf("decisions = %+v, want only T awake", got)
			}
		})
	}
}

func TestPoolStartBackoff_CachedDemandPreservesDeferralUntilExpiry(t *testing.T) {
	f := newDeferredSurvivorFixture(t)
	f.clk.Time = time.Now()
	until := f.clk.Now().Add(time.Hour).Truncate(time.Second)
	w := f.work(f.store, "W", "helper", "", false)
	if err := f.store.Update(w.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.StartBackoffUntilMetadataKey: until.Format(time.RFC3339)}}); err != nil {
		t.Fatal(err)
	}
	f.session("helper-1", "helper", "1", w.ID, "city", false)
	cr := &CityRuntime{
		cityName: "gc", cityPath: f.path, cfg: f.cfg, sp: f.sp, stderr: &f.stderr,
		cs: &controllerState{cityName: "gc", cityPath: f.path, cityBeadStore: f.store, eventProv: events.NewFake()},
	}
	buildCalls := 0
	cr.buildFnWithSessionBeads = func(cfg *config.City, sp runtime.Provider, store beads.Store, rigs map[string]beads.Store, snap *sessionBeadSnapshot, trace *sessionReconcilerTraceCycle) DesiredStateResult {
		buildCalls++
		return buildDesiredStateWithSessionBeadsAt("gc", f.path, f.clk.Now(), f.clk.Now(), cfg, sp, store, rigs, snap, trace, &f.stderr, nil)
	}
	snap, err := loadSessionBeadSnapshot(f.store)
	if err != nil {
		t.Fatal(err)
	}
	first := cr.loadDemandSnapshot(snap, nil, "patrol", false)
	second := cr.loadDemandSnapshot(snap, nil, "patrol", false)
	if buildCalls != 1 {
		t.Fatalf("builds = %d, want cached reuse", buildCalls)
	}
	if !second.result.PoolStartDeferredTriggers.contains(w.ID, "city") || !second.result.PoolStartDecisionTime.Equal(first.result.PoolStartDecisionTime) || !second.recheckAt.Equal(until) {
		t.Fatalf("cache lost decision: %+v", second)
	}
	f.clk.Time = until
	cr.demandSnapshot.recheckAt = time.Now().Add(-time.Second)
	third := cr.loadDemandSnapshot(snap, nil, "patrol", false)
	if buildCalls != 2 || third.result.PoolStartDeferredTriggers.contains(w.ID, "city") {
		t.Fatalf("expired snapshot not refreshed: builds=%d deferred=%v", buildCalls, third.result.PoolStartDeferredTriggers)
	}
	if third.result.PoolDesiredCounts["helper"] != 1 {
		t.Fatalf("eligible demand after expiry = %v", third.result.PoolDesiredCounts)
	}
}

func TestPoolStartBackoff_DeferredResumeDoesNotReserveEligibleCapacity(t *testing.T) {
	f := newDeferredSurvivorFixture(t)
	f.cfg.Agents[0].MaxActiveSessions = intPtr(1)
	w := f.work(f.store, "W", "helper", "", true)
	f.work(f.store, "V", "helper", "helper-2", false)
	u := f.work(f.store, "U", "helper", "", false)
	s := f.session("helper-2", "helper", "2", w.ID, "city", false)
	f.session("helper-1", "helper", "1", u.ID, "city", true)
	for range 3 {
		f.tick()
		f.requireTrigger(s, w.ID)
	}
	f.requireStarts("helper-1")
}
