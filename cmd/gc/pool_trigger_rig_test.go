package main

import (
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func triggerRigFixture(t *testing.T) (*agentBuildParams, *config.Agent) {
	t.Helper()
	f := newCityPoolFixture(t)
	a := &f.cfg.Agents[0]
	a.WorkDir = ".worktrees/{{.TriggerRig}}/lane-{{.AgentBase}}"
	a.PreStart = []string{"sh setup.sh --rig {{.TriggerRig}}"}
	bp := newAgentBuildParams("fixture", f.root, f.cfg, runtime.NewFake(), time.Now().UTC(), beads.NewMemStore(), io.Discard)
	return bp, a
}

func TestPoolTriggerWorkDirExpandsTriggerRig(t *testing.T) {
	bp, a := triggerRigFixture(t)
	for _, tc := range []struct{ ref, rig string }{
		{"rig:alpha", "alpha"}, {"alpha", "alpha"}, {"city", ""}, {"city:x", ""}, {"rig:unknown", ""}, {"rig:../x", ""}, {"class:session", ""}, {"", ""},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			got := poolTriggerWorkDir(bp, a, "worker-1", SessionRequest{WorkBeadID: "work-1", WorkStoreRef: tc.ref})
			want := filepath.Join(bp.cityPath, ".worktrees", tc.rig, "lane-worker.pool-1")
			if got != want {
				t.Fatalf("work_dir=%q, want %q", got, want)
			}
		})
	}
}

func TestResolveTemplateExpandsTriggerRigInPreStart(t *testing.T) {
	bp, a := triggerRigFixture(t)
	a.WorkDir = ".worktrees/{{.AgentBase}}"
	tp, err := resolveTemplateForSessionBeadInfo(bp, a, "worker", nil, session.Info{TriggerBeadID: "work-1", TriggerBeadStoreRef: "rig:alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tp.Hints.PreStart) != 1 || tp.Hints.PreStart[0] != "sh setup.sh --rig alpha" {
		t.Fatalf("pre_start=%q", tp.Hints.PreStart)
	}
}

func TestPoolSessionWorkDirAgreesAtCreateAndStart(t *testing.T) {
	bp, a := triggerRigFixture(t)
	instance := a.PoolSlotInstance(1)
	for _, ref := range []string{"rig:alpha", "alpha", "city", ""} {
		t.Run(ref, func(t *testing.T) {
			created := poolTriggerWorkDir(bp, a, instance.QualifiedName(), SessionRequest{WorkBeadID: "work-1", WorkStoreRef: ref})
			tp, err := resolveTemplateForSessionBeadInfo(bp, &instance, instance.QualifiedName(), nil, session.Info{TriggerBeadID: "work-1", TriggerBeadStoreRef: ref})
			if err != nil {
				t.Fatal(err)
			}
			if created == "" || tp.WorkDir != created {
				t.Fatalf("created=%q start=%q", created, tp.WorkDir)
			}
		})
	}
}

func TestCityPoolMinimumPreservesLiveSessionRig(t *testing.T) {
	f := newCityPoolFixture(t)
	f.cfg.Agents[0].MinActiveSessions = intPtr(1)
	b := f.live(t, "rig:alpha")
	// Outside post-create protection the idle live session can fill a minimum
	// seat without receiving a new ticket.
	if err := f.store.Update(b.ID, beads.UpdateOpts{Metadata: map[string]string{"state_reason": "idle"}}); err != nil {
		t.Fatal(err)
	}
	var err error
	f.sessions[0], err = f.store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	ds := f.build()
	got, err := f.store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.State) != 1 || got.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey] != "rig:alpha" {
		t.Fatalf("minimum changed live seat: sessions=%d trigger ref=%q", len(ds.State), got.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey])
	}
}

func TestTriggerRigEmptyWithoutTrigger(t *testing.T) {
	for _, tc := range []struct {
		name, dir string
		info      session.Info
	}{
		{"manual", "", session.Info{ManualSession: true, TriggerBeadStoreRef: "rig:alpha"}},
		{"named", "", session.Info{Alias: "named", TriggerBeadID: "work-1", TriggerBeadStoreRef: "city:fixture"}},
		{"rig", "alpha", session.Info{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bp, a := triggerRigFixture(t)
			a.Dir = tc.dir
			a.WorkDir = "work/{{.TriggerRig}}/lane"
			a.PreStart = []string{"rig=[{{.Rig}}] trigger=[{{.TriggerRig}}]"}
			tp, err := resolveTemplateForSessionBeadInfo(bp, a, a.QualifiedName(), nil, tc.info)
			if err != nil {
				t.Fatal(err)
			}
			want := "rig=[" + tc.dir + "] trigger=[]"
			if tp.WorkDir != filepath.Join(bp.cityPath, "work", "lane") || len(tp.Hints.PreStart) != 1 || tp.Hints.PreStart[0] != want {
				t.Fatalf("dir=%q pre_start=%q", tp.WorkDir, tp.Hints.PreStart)
			}
		})
	}
}
