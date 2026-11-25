package main

import (
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worktree"
)

type cityPoolFixture struct {
	root     string
	cfg      *config.City
	store    beads.Store
	rigs     map[string]beads.Store
	sessions []beads.Bead
}

func newCityPoolFixture(t *testing.T) *cityPoolFixture {
	t.Helper()
	root := t.TempDir()
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	cfg := &config.City{
		Workspace: config.Workspace{Name: "fixture"},
		Agents: []config.Agent{{
			Name: "worker", Scope: "city", StartCommand: "true",
			MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5),
		}},
	}
	rigs := make(map[string]beads.Store)
	for _, name := range []string{"alpha", "beta"} {
		cfg.Rigs = append(cfg.Rigs, config.Rig{Name: name, Path: filepath.Join(root, name)})
		s := beads.NewMemStore()
		s.HonorExplicitIDs = true
		rigs[name] = s
	}
	return &cityPoolFixture{root: root, cfg: cfg, store: store, rigs: rigs}
}

func (f *cityPoolFixture) route(t *testing.T, rig, id string) {
	t.Helper()
	store := f.store
	if rig != "" {
		store = f.rigs[rig]
	}
	b, err := store.Create(beads.Bead{ID: id, Title: id, Type: "task", Metadata: map[string]string{"gc.routed_to": f.cfg.Agents[0].QualifiedName()}})
	if err != nil || b.ID != id {
		t.Fatalf("creating %s in %q: id=%s, err=%v", id, rig, b.ID, err)
	}
}

func (f *cityPoolFixture) live(t *testing.T, ref string) beads.Bead {
	t.Helper()
	return f.liveAt(t, ref, time.Now().Add(-time.Second))
}

func (f *cityPoolFixture) liveAt(t *testing.T, ref string, createdAt time.Time) beads.Bead {
	t.Helper()
	slot := len(f.sessions) + 1
	b := protectedPoolSessionBeadAt(fmt.Sprintf("session-%d", slot), createdAt)
	b.Labels = []string{sessionBeadLabel, "template:worker"}
	b.Metadata["template"] = "worker"
	b.Metadata["agent_name"] = fmt.Sprintf("worker-%d", slot)
	b.Metadata["pool_slot"] = fmt.Sprint(slot)
	b.Metadata["session_name"] = fmt.Sprintf("worker-live-%d", slot)
	b.Metadata[beadmeta.TriggerBeadIDMetadataKey] = "previous"
	b.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey] = ref
	b, err := f.store.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	infos, err := collectAllOpenSessionInfos("", f.cfg, f.store, f.rigs, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range infos {
		if info.ID == b.ID && isPoolManagedSessionInfo(info) && poolSessionIsLiveInfo(info) {
			f.sessions = append(f.sessions, b)
			return b
		}
	}
	t.Fatal("prepared session is not live")
	return beads.Bead{}
}

func (f *cityPoolFixture) build() DesiredStateResult {
	return buildDesiredStateWithSessionBeads("fixture", f.root, time.Now().UTC(), f.cfg, runtime.NewFake(), f.store, f.rigs, newSessionBeadSnapshot(f.sessions), nil, io.Discard)
}

func TestCityPoolResumeRetainsOwnerInOriginalWorkspace(t *testing.T) {
	for _, ref := range []string{"city", "rig:beta"} {
		t.Run(ref, func(t *testing.T) {
			f := newCityPoolFixture(t)
			f.cfg.Agents[0].WorkDir = "lanes/{{.TriggerRig}}/{{.AgentBase}}"
			b := f.live(t, ref)
			f.route(t, "alpha", "claimed")
			if err := f.rigs["alpha"].Update("claimed", beads.UpdateOpts{Status: stringPtr("in_progress"), Assignee: &b.ID}); err != nil {
				t.Fatal(err)
			}
			ds := f.build()
			if len(ds.AssignedWorkBeads) != 1 || ds.AssignedWorkBeads[0].Status != "in_progress" || ds.AssignedWorkStoreRefs[0] != "alpha" {
				t.Fatalf("assigned work=%v stores=%v", ds.AssignedWorkBeads, ds.AssignedWorkStoreRefs)
			}
			got := ds.State[b.Metadata["session_name"]]
			if len(ds.State) != 1 || got.Env["GC_TRIGGER_BEAD_STORE_REF"] != ref || got.Env["GC_TRIGGER_BEAD_ID"] != "previous" {
				t.Fatalf("sessions=%v, owner trigger=%q/%q", mapKeys(ds.State), got.Env["GC_TRIGGER_BEAD_ID"], got.Env["GC_TRIGGER_BEAD_STORE_REF"])
			}
		})
	}
}

func TestCityPoolWakeRetainsRigAwareAllocation(t *testing.T) {
	f := newCityPoolFixture(t)
	f.cfg.Agents[0].MaxActiveSessions = intPtr(2)
	now := time.Now()
	a := f.live(t, "rig:alpha")
	other := f.liveAt(t, "rig:alpha", now.Add(-24*time.Hour))
	b := f.liveAt(t, "rig:beta", now.Add(-24*time.Hour))
	f.route(t, "beta", "beta-1")
	cr := &CityRuntime{
		cityName: "fixture", cityPath: f.root, cfg: f.cfg, sp: runtime.NewFake(), stderr: io.Discard,
		standaloneCityStore: f.store, standaloneRigStores: f.rigs,
		buildFnWithSessionBeads: func(*config.City, runtime.Provider, beads.Store, map[string]beads.Store, *sessionBeadSnapshot, *sessionReconcilerTraceCycle) DesiredStateResult {
			return f.build()
		},
	}
	sessions := newSessionBeadSnapshot(f.sessions)
	ds := cr.loadDemandSnapshot(sessions, nil, "poke", false).result
	if len(ds.BaseState) != 2 || ds.BaseState[b.Metadata["session_name"]].Env["GC_TRIGGER_BEAD_ID"] != "beta-1" {
		t.Fatalf("planner selected %v, want both sessions with beta-1 on the beta session", mapKeys(ds.BaseState))
	}
	awake := buildAwakeInputFromReconciler(f.cfg, f.root, sessions.OpenInfos(), ds.PoolDesiredCounts, nil, nil, nil, nil, nil, nil, nil, cr.sp, now)
	awake.CityPoolDemandSessions = selectedCityPoolDemandSessions(ds.State)
	decisions := ComputeAwakeSet(awake)
	if !decisions[a.Metadata["session_name"]].ShouldWake || !decisions[b.Metadata["session_name"]].ShouldWake || decisions[other.Metadata["session_name"]].ShouldWake {
		t.Fatalf("pool desired=%v, wake decisions=%v", ds.PoolDesiredCounts, decisions)
	}
}

func TestBuildDesiredState_CityScopedPoolCountsRigStoreRoutedDemand(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(fmt.Sprintf("warm=%v", warm), func(t *testing.T) {
			f := newCityPoolFixture(t)
			if warm {
				f.live(t, "rig:alpha")
			}
			f.route(t, "alpha", "alpha-1")
			f.route(t, "alpha", "alpha-2")
			f.route(t, "beta", "beta-1")
			if got := f.build().ScaleCheckCounts["worker"]; got != 3 {
				t.Fatalf("count=%d, want 3", got)
			}
		})
	}
}

func newSplitCityPoolFixture(t *testing.T) *cityPoolFixture {
	t.Helper()
	f := newCityPoolFixture(t)
	work := beads.NewMemStore()
	routes := splitRoutes(f.store)
	registerResidencyRoutes(f.root, routes, func() beads.Store { return work })
	t.Cleanup(func() { unregisterResidencyRoutes(f.root, routes) })
	return f
}

func TestCityPoolSplitSkipsLegacyRigDemand(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy_missing=%v", missing), func(t *testing.T) {
			f := newSplitCityPoolFixture(t)
			f.route(t, "", "retained")
			if err := f.store.Update("retained", beads.UpdateOpts{Status: stringPtr("closed")}); err != nil {
				t.Fatal(err)
			}
			f.route(t, "alpha", "retained")
			if missing {
				delete(f.rigs, "alpha")
			}
			ds := f.build()
			if ds.ScaleCheckCounts["worker"] != 0 || ds.PoolScaleCheckPartialTemplates["worker"] || len(ds.State) != 0 {
				t.Fatalf("count=%d partial=%v realized=%d", ds.ScaleCheckCounts["worker"], ds.PoolScaleCheckPartialTemplates["worker"], len(ds.State))
			}
		})
	}
	t.Run("ticket only in legacy ledger", func(t *testing.T) {
		// A ledger-resident row is not runtime-plane demand. An operator
		// moves it into the authoritative binding with gc storage migrate.
		f := newSplitCityPoolFixture(t)
		f.route(t, "alpha", "legacy")
		ds := f.build()
		if ds.ScaleCheckCounts["worker"] != 0 || ds.PoolScaleCheckPartialTemplates["worker"] || len(ds.State) != 0 {
			t.Fatalf("count=%d partial=%v realized=%d", ds.ScaleCheckCounts["worker"], ds.PoolScaleCheckPartialTemplates["worker"], len(ds.State))
		}
	})
	t.Run("open ticket in binding", func(t *testing.T) {
		f := newSplitCityPoolFixture(t)
		f.route(t, "", "bound")
		ds := f.build()
		if ds.ScaleCheckCounts["worker"] != 1 || ds.PoolScaleCheckPartialTemplates["worker"] || len(ds.State) != 1 {
			t.Fatalf("count=%d partial=%v realized=%d", ds.ScaleCheckCounts["worker"], ds.PoolScaleCheckPartialTemplates["worker"], len(ds.State))
		}
	})
}

func TestBuildDesiredState_CityScopedPoolRigDemandCarriesRigStoreRef(t *testing.T) {
	f := newCityPoolFixture(t)
	f.route(t, "alpha", "alpha-1")
	ds := f.build()
	if len(ds.ReadyUnassignedRoutedWorkBeads) != 1 || ds.ReadyUnassignedRoutedWorkBeads[0].ID != "alpha-1" || len(ds.ReadyUnassignedRoutedWorkStoreRefs) != 1 || ds.ReadyUnassignedRoutedWorkStoreRefs[0] != "rig:alpha" {
		t.Fatalf("selected work=%v refs=%v", ds.ReadyUnassignedRoutedWorkBeads, ds.ReadyUnassignedRoutedWorkStoreRefs)
	}
	for _, tp := range ds.State {
		if tp.Env["GC_TRIGGER_BEAD_ID"] == "alpha-1" && tp.Env["GC_TRIGGER_BEAD_STORE_REF"] == "rig:alpha" {
			return
		}
	}
	t.Fatal("no realized session for alpha-1 in rig:alpha")
}

func TestBuildDesiredState_CityScopedPoolMissingBoundRigStoreStaysPartial(t *testing.T) {
	f := newCityPoolFixture(t)
	delete(f.rigs, "beta")
	f.route(t, "alpha", "alpha-1")
	ds := f.build()
	if ds.ScaleCheckCounts["worker"] != 1 || !ds.ScaleCheckPartialTemplates["worker"] || len(ds.State) != 1 {
		t.Fatalf("count=%d partial=%v realized=%d", ds.ScaleCheckCounts["worker"], ds.ScaleCheckPartialTemplates, len(ds.State))
	}
	for _, tp := range ds.State {
		if tp.Env["GC_TRIGGER_BEAD_ID"] != "alpha-1" || tp.Env["GC_TRIGGER_BEAD_STORE_REF"] != "rig:alpha" {
			t.Fatalf("fresh session did not bind to the proven alpha ticket")
		}
	}
}

func TestCityPoolPartialCreateRequiresProvenDemand(t *testing.T) {
	t.Run("minimum cannot add unproven starts", func(t *testing.T) {
		f := newCityPoolFixture(t)
		f.cfg.Agents[0].MinActiveSessions = intPtr(3)
		delete(f.rigs, "beta")
		f.route(t, "alpha", "alpha-1")
		ds := f.build()
		if !ds.PoolScaleCheckPartialTemplates["worker"] || len(ds.State) != 1 {
			t.Fatalf("partial=%v realized=%d, want one proven start", ds.PoolScaleCheckPartialTemplates, len(ds.State))
		}
	})
	t.Run("rig pool still rejects fresh creates", func(t *testing.T) {
		f := newCityPoolFixture(t)
		a := &f.cfg.Agents[0]
		a.Dir, a.Scope = "alpha", "rig"
		f.rigs["alpha"] = &readyFailStore{Store: f.rigs["alpha"]}
		f.route(t, "", "city-1")
		ds := f.build()
		if !ds.PoolScaleCheckPartialTemplates[a.QualifiedName()] || ds.ScaleCheckCounts[a.QualifiedName()] != 1 || len(ds.State) != 0 {
			t.Fatalf("counts=%v partial=%v realized=%d", ds.ScaleCheckCounts, ds.PoolScaleCheckPartialTemplates, len(ds.State))
		}
	})
	t.Run("failed city read prevents fresh creates", func(t *testing.T) {
		f := newCityPoolFixture(t)
		f.store = &readyFailStore{Store: f.store}
		f.route(t, "alpha", "alpha-1")
		ds := f.build()
		if !ds.PoolScaleCheckPartialTemplates["worker"] || ds.ScaleCheckCounts["worker"] != 1 || len(ds.State) != 0 {
			t.Fatalf("counts=%v partial=%v realized=%d", ds.ScaleCheckCounts, ds.PoolScaleCheckPartialTemplates, len(ds.State))
		}
	})
	t.Run("existing seat serves the proven ticket", func(t *testing.T) {
		f := newCityPoolFixture(t)
		delete(f.rigs, "beta")
		b := f.live(t, "rig:alpha")
		f.route(t, "alpha", "previous")
		ds := f.build()
		if !ds.PoolScaleCheckPartialTemplates["worker"] || len(ds.State) != 1 {
			t.Fatalf("partial=%v realized=%d", ds.PoolScaleCheckPartialTemplates, len(ds.State))
		}
		if _, ok := ds.State[b.Metadata["session_name"]]; !ok {
			t.Fatalf("existing session missing from %v", mapKeys(ds.State))
		}
	})
	t.Run("partial retains live capacity", func(t *testing.T) {
		f := newCityPoolFixture(t)
		delete(f.rigs, "beta")
		f.route(t, "alpha", "alpha-1")
		ds := f.build()
		var live []beads.Bead
		for _, id := range []string{"slot-a", "slot-b"} {
			b := protectedPoolSessionBeadAt(id, time.Now())
			b.Metadata["template"] = "worker"
			live = append(live, b)
		}
		got := retainScaleCheckPartialPoolDesired(f.cfg, map[string]int{"worker": 1}, newSessionBeadSnapshot(live), ds.PoolPartialRetentionTemplates)
		if got["worker"] != 2 {
			t.Fatalf("retained=%v, want both live sessions", got)
		}
	})
	t.Run("cap bounds confirmed tickets", func(t *testing.T) {
		f := newCityPoolFixture(t)
		delete(f.rigs, "beta")
		f.cfg.Agents[0].MaxActiveSessions = intPtr(2)
		for i := 0; i < 5; i++ {
			f.route(t, "alpha", fmt.Sprintf("alpha-%d", i))
		}
		ds := f.build()
		if !ds.PoolScaleCheckPartialTemplates["worker"] || ds.ScaleCheckCounts["worker"] != 5 || len(ds.State) != 2 {
			t.Fatalf("counts=%v partial=%v realized=%d", ds.ScaleCheckCounts, ds.PoolScaleCheckPartialTemplates, len(ds.State))
		}
	})
}

func TestBuildDesiredState_CityScopedPoolRigDemandBoundedByMaxActive(t *testing.T) {
	f := newCityPoolFixture(t)
	f.cfg.Agents[0].MaxActiveSessions = intPtr(2)
	for i := 0; i < 5; i++ {
		f.route(t, "alpha", fmt.Sprintf("alpha-%d", i))
	}
	if ds := f.build(); ds.ScaleCheckCounts["worker"] != 5 || len(ds.State) != 2 {
		t.Fatalf("count=%d realized=%d", ds.ScaleCheckCounts["worker"], len(ds.State))
	}
}

func TestBuildDesiredState_CityScopedPoolAliasedRigStoreDoesNotDoubleCount(t *testing.T) {
	f := newCityPoolFixture(t)
	f.rigs["alpha"] = aliasStore{f.store}
	f.route(t, "", "shared-1")
	if got := f.build().ScaleCheckCounts["worker"]; got != 1 {
		t.Fatalf("count=%d, want 1", got)
	}
}

func TestCityPoolFreshRecoveryKeepsAssignedTicketRig(t *testing.T) {
	for _, state := range []string{"asleep", "drained"} {
		t.Run(state, func(t *testing.T) {
			f := newCityPoolFixture(t)
			f.cfg.Agents[0].WakeMode = "fresh"
			owner := f.live(t, "rig:alpha")
			if err := f.store.SetMetadata(owner.ID, "state", state); err != nil {
				t.Fatal(err)
			}
			var err error
			f.sessions[0], err = f.store.Get(owner.ID)
			if err != nil {
				t.Fatal(err)
			}
			live := f.live(t, "rig:alpha")
			f.route(t, "alpha", "owned")
			if err := f.rigs["alpha"].Update("owned", beads.UpdateOpts{Status: stringPtr("in_progress"), Assignee: &owner.ID}); err != nil {
				t.Fatal(err)
			}
			ds := f.build()
			tp := ds.BaseState[live.Metadata["session_name"]]
			if len(ds.BaseState) != 1 || tp.Env["GC_TRIGGER_BEAD_ID"] != "owned" || tp.Env["GC_TRIGGER_BEAD_STORE_REF"] != "rig:alpha" {
				t.Fatalf("recovery planned=%v, live trigger=%q/%q", mapKeys(ds.BaseState), tp.Env["GC_TRIGGER_BEAD_ID"], tp.Env["GC_TRIGGER_BEAD_STORE_REF"])
			}
		})
	}
}

func TestCityPoolAliasKeepsSingleTicketOnItsLiveSession(t *testing.T) {
	for _, tc := range []struct{ name, source, alias, wantRef string }{
		{"city ledger", "", "alpha", "city"},
		{"rig ledger", "alpha", "beta", "rig:alpha"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i := range 32 {
				f := newCityPoolFixture(t)
				ledger := f.store
				if tc.source != "" {
					ledger = f.rigs[tc.source]
				}
				f.rigs[tc.alias] = aliasStore{ledger}
				live := f.live(t, tc.wantRef)
				f.route(t, tc.source, "previous")
				ds := f.build()
				tp := ds.BaseState[live.Metadata["session_name"]]
				if ds.ScaleCheckCounts["worker"] != 1 || len(ds.BaseState) != 1 || tp.Env["GC_TRIGGER_BEAD_STORE_REF"] != tc.wantRef {
					t.Errorf("build %d: count=%d sessions=%v ref=%q, want one live session in %s", i, ds.ScaleCheckCounts["worker"], mapKeys(ds.BaseState), tp.Env["GC_TRIGGER_BEAD_STORE_REF"], tc.wantRef)
				}
			}
		})
	}
}

// gf-u7hz tracks the second worker planned when an earlier alias recovers before claim.
func TestBuildDesiredState_CityScopedPoolAliasRecoveryKnownLimit(t *testing.T) {
	f := newCityPoolFixture(t)
	f.cfg.Agents[0].MaxActiveSessions = intPtr(2)
	ledger := f.rigs["alpha"]
	f.rigs["beta"] = aliasStore{ledger}
	f.rigs["alpha"] = &readyFailStore{Store: ledger}
	f.route(t, "beta", "shared-1")

	first := f.build()
	if !first.PoolScaleCheckPartialTemplates["worker"] || first.ScaleCheckCounts["worker"] != 1 || len(first.State) != 1 {
		t.Fatalf("first tick: count=%d partial=%v sessions=%v", first.ScaleCheckCounts["worker"], first.PoolScaleCheckPartialTemplates, mapKeys(first.State))
	}
	sessions, err := f.store.ListByLabel(sessionBeadLabel, 0)
	if err != nil || len(sessions) != 1 || sessions[0].Metadata[beadmeta.TriggerBeadStoreRefMetadataKey] != "rig:beta" {
		t.Fatalf("expected one pending beta session: sessions=%v err=%v", sessions, err)
	}
	f.sessions = sessions
	f.rigs["alpha"] = ledger
	second := f.build()
	if second.PoolScaleCheckPartialTemplates["worker"] || second.ScaleCheckCounts["worker"] != 1 ||
		second.cityPoolWakeDemand["worker"].StoreRefs["shared-1"] != "rig:alpha" {
		t.Fatalf("recovered tick: count=%d partial=%v stores=%v", second.ScaleCheckCounts["worker"], second.PoolScaleCheckPartialTemplates, second.cityPoolWakeDemand["worker"].StoreRefs)
	}
	bindings := make(map[string]string)
	for name, session := range second.State {
		bindings[name] = session.Env["GC_TRIGGER_BEAD_ID"] + "/" + session.Env["GC_TRIGGER_BEAD_STORE_REF"]
	}
	if len(bindings) != 2 || bindings["worker-gc-1"] != "shared-1/rig:beta" || bindings["worker-gc-2"] != "shared-1/rig:alpha" {
		t.Fatalf("recovered tick: count=1, realized=%v; want the original beta worker and a second alpha worker", bindings)
	}
}

func TestCityPoolCollisionUsesCompleteFirstRigRow(t *testing.T) {
	const id = "shared-1"
	repo, base := worktreeTestRepo(t)
	root := t.TempDir()
	spec := worktree.Spec{
		RepoDir: repo, Root: root, Path: filepath.Join(root, id), Branch: "work/shared", Base: base,
		BeadID: id, StoreRef: "rig:alpha", Creator: "gc-sling", Owner: "gc-sling", Generation: "1", Lifecycle: worktree.LifecycleActive,
	}
	report, err := worktree.Ensure(spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.BaseSHA = report.Provenance.BaseSHA
	alpha := beads.Bead{ID: id, Title: "Alpha ticket", Type: "task", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey: "worker",
		beadmeta.WorkDirMetadataKey:  spec.Path, beadmeta.WorkBranchMetadataKey: spec.Branch,
		beadmeta.WorktreeRepoMetadataKey: repo, beadmeta.WorktreeRootMetadataKey: root,
		beadmeta.WorktreeBaseRefMetadataKey: base, beadmeta.WorktreeBaseSHAMetadataKey: spec.BaseSHA,
		beadmeta.WorktreeCreatorMetadataKey: spec.Creator, beadmeta.WorktreeOwnerMetadataKey: spec.Owner,
		beadmeta.WorktreeGenerationMetadataKey: spec.Generation, beadmeta.WorktreeLifecycleMetadataKey: spec.Lifecycle,
	}}
	// If beta is visited first, all of its optional metadata and its invalid
	// worktree evidence must disappear when alpha replaces it.
	beta := beads.Bead{ID: id, Title: "Beta ticket", Type: "task", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey: "worker", beadmeta.PackMetadataKey: "other",
		beadmeta.PackWorkspaceMetadataKey: "/other", beadmeta.BrainParentSIDMetadataKey: "other",
		beadmeta.WorktreeOwnerMetadataKey: "incomplete",
	}}
	for i := range 32 {
		f := newCityPoolFixture(t)
		if _, err := f.rigs["beta"].Create(beta); err != nil {
			t.Fatal(err)
		}
		if _, err := f.rigs["alpha"].Create(alpha); err != nil {
			t.Fatal(err)
		}
		ds := f.build()
		d := ds.cityPoolWakeDemand["worker"]
		if ds.ScaleCheckCounts["worker"] != 1 || len(ds.BaseState) != 1 || d.StoreRefs[id] != spec.StoreRef || d.Titles[id] != alpha.Title || d.WorktreeSpecs[id] == nil || *d.WorktreeSpecs[id] != spec {
			t.Errorf("build %d: count=%d sessions=%v ref=%q title=%q spec=%+v", i, ds.ScaleCheckCounts["worker"], mapKeys(ds.BaseState), d.StoreRefs[id], d.Titles[id], d.WorktreeSpecs[id])
		}
		if d.Packs[id] != "" || d.Workspaces[id] != "" || d.ParentSIDs[id] != "" || d.WorktreeErrors[id] != "" {
			t.Errorf("build %d: beta metadata leaked: pack=%q workspace=%q parent=%q error=%q", i, d.Packs[id], d.Workspaces[id], d.ParentSIDs[id], d.WorktreeErrors[id])
		}
		for _, tp := range ds.BaseState {
			if tp.Env["GC_TRIGGER_BEAD_ID"] != id || tp.Env["GC_TRIGGER_BEAD_STORE_REF"] != spec.StoreRef {
				t.Errorf("build %d: realized trigger=%q/%q", i, tp.Env["GC_TRIGGER_BEAD_ID"], tp.Env["GC_TRIGGER_BEAD_STORE_REF"])
			}
		}
	}
}

// Independent stores sharing an ID prefix inherit the probe's ID-only limit.
// Either store may supply the single trigger.
func TestBuildDesiredState_CityScopedPoolSharedIDKnownLimit(t *testing.T) {
	f := newCityPoolFixture(t)
	f.route(t, "alpha", "shared-1")
	f.route(t, "beta", "shared-1")
	ds := f.build()
	if ds.ScaleCheckCounts["worker"] != 1 || len(ds.State) != 1 {
		t.Fatalf("count=%d realized=%d", ds.ScaleCheckCounts["worker"], len(ds.State))
	}
	for _, tp := range ds.State {
		ref := tp.Env["GC_TRIGGER_BEAD_STORE_REF"]
		if tp.Env["GC_TRIGGER_BEAD_ID"] != "shared-1" || (ref != "rig:alpha" && ref != "rig:beta") {
			t.Fatalf("unexpected trigger ID/ref: %q/%q", tp.Env["GC_TRIGGER_BEAD_ID"], ref)
		}
	}
}

func TestBuildDesiredState_CityScopedPoolSkipsSuspendedAndUnboundRigs(t *testing.T) {
	f := newCityPoolFixture(t)
	f.cfg.Rigs[0].Suspended = true
	f.cfg.Rigs[1].Path = ""
	delete(f.rigs, "beta")
	f.route(t, "alpha", "alpha-1")
	ds := f.build()
	if ds.ScaleCheckCounts["worker"] != 0 || ds.ScaleCheckPartialTemplates["worker"] {
		t.Fatalf("counts=%v partial=%v", ds.ScaleCheckCounts, ds.ScaleCheckPartialTemplates)
	}
}

func TestBuildDesiredState_UnscopedCityPoolDoesNotProbeRigStores(t *testing.T) {
	f := newCityPoolFixture(t)
	f.cfg.Agents[0].Scope = ""
	f.route(t, "alpha", "alpha-1")
	if got := f.build().ScaleCheckCounts["worker"]; got != 0 {
		t.Fatalf("count=%d, want 0", got)
	}
}

func TestBuildDesiredState_CityScopedCustomScaleCheckPoolUnchanged(t *testing.T) {
	f := newCityPoolFixture(t)
	f.cfg.Agents[0].ScaleCheck = "echo 0"
	f.live(t, "")
	f.route(t, "alpha", "alpha-1")
	ds := f.build()
	if got, ok := ds.ScaleCheckCounts["worker"]; !ok || got != 0 {
		t.Fatalf("counts=%v, want custom count 0", ds.ScaleCheckCounts)
	}
}

func TestBuildDesiredState_CityScopedControlDispatcherUnchanged(t *testing.T) {
	f := newCityPoolFixture(t)
	f.cfg.Agents[0].Name = config.ControlDispatcherAgentName
	f.route(t, "alpha", "alpha-1")
	if got := f.build().ScaleCheckCounts[config.ControlDispatcherAgentName]; got != 0 {
		t.Fatalf("count=%d, want 0", got)
	}
}

func TestBuildDesiredState_CityPoolLiveSessionRigAffinity(t *testing.T) {
	for _, tc := range []struct {
		name, oldRef, newRig string
		max, wantSessions    int
		rebind               bool
	}{
		{"different rig", "rig:alpha", "beta", 2, 2, false},
		{"different rig at cap", "rig:alpha", "beta", 1, 1, false},
		{"same rig", "alpha", "alpha", 2, 1, true},
		{"city to rig", "", "alpha", 2, 2, false},
		{"rig to city", "rig:alpha", "", 2, 2, false},
		{"city to city", "city:fixture", "", 2, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCityPoolFixture(t)
			f.cfg.Agents[0].MaxActiveSessions = intPtr(tc.max)
			b := f.live(t, tc.oldRef)
			f.route(t, tc.newRig, "new-1")
			ds := f.build()
			got, err := f.store.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantID, wantRef := "previous", tc.oldRef
			if tc.rebind {
				wantID, wantRef = "new-1", "city"
				if tc.newRig != "" {
					wantRef = "rig:" + tc.newRig
				}
			}
			if len(ds.State) != tc.wantSessions || got.Metadata[beadmeta.TriggerBeadIDMetadataKey] != wantID || got.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey] != wantRef {
				t.Fatalf("realized=%d, trigger=%q/%q; want %d, %q/%q", len(ds.State), got.Metadata[beadmeta.TriggerBeadIDMetadataKey], got.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey], tc.wantSessions, wantID, wantRef)
			}
			if tc.wantSessions == 2 {
				for _, tp := range ds.State {
					if tp.Env["GC_TRIGGER_BEAD_ID"] == "new-1" {
						return
					}
				}
				t.Fatal("new ticket has no session")
			}
		})
	}
}
