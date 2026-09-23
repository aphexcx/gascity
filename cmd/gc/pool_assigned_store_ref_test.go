package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// A rig work bead must retain its store identity from assigned demand through
// the metadata of the replacement session, so a start failure charges that rig.
func TestComputePoolDesiredStates_AssignedRigMintsTriggerStoreRef(t *testing.T) {
	city := beads.NewMemStore()
	rig := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", Scope: "city", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1)}},
	}
	work, err := rig.Create(beads.Bead{Title: "rig task", Type: "task", Status: "in_progress", Assignee: "worker", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	pass := newWorkStartDeferralPass(now, nil)
	pass.filterWithRefs([]beads.Bead{work}, []string{"riga"}, true)
	owned, rows, refs := poolDemandAssignedWork(cfg, "", nil, nil, []beads.Bead{work}, []string{"riga"}, pass.deferred)
	states := ComputePoolDesiredStatesDeferring(cfg, rows, refs, owned, nil, nil, nil, pass.deferred, nil)
	if len(states) != 1 || len(states[0].Requests) != 1 || states[0].Requests[0].Tier != "wake-known-identity" {
		t.Fatalf("desired states = %#v, want one replacement request", states)
	}
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), now, city, &stderr)
	bp.sessionBeads = &sessionBeadSnapshot{}
	bp.assignedWorkBeads = owned
	realizePoolDesiredSessions(bp, &cfg.Agents[0], states[0], map[string]TemplateParams{}, &stderr)
	sessions := bp.sessionBeads.OpenInfos()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %#v, want one; stderr=%s", sessions, stderr.String())
	}
	minted, err := city.Get(sessions[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := minted.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey]; got != "rig:riga" {
		t.Fatalf("minted trigger store ref = %q, want rig:riga", got)
	}
	if got := minted.Metadata[beadmeta.TriggerBeadIDMetadataKey]; got != work.ID {
		t.Fatalf("minted trigger bead = %q, want %q", got, work.ID)
	}
	policy := newWorkStartFailurePolicy(cfg, city, map[string]beads.Store{"riga": rig}, nil, "")
	prepared := preparedStart{candidate: startCandidate{info: sessions[0], tp: TemplateParams{TemplateName: "worker"}}}
	prepared.attachWorkStartPolicy(policy)
	recordWorkStartFailure(startResult{prepared: prepared, err: errPreStart, outcome: TraceOutcomeProviderError}, &clock.Fake{Time: now}, &stderr, nil)
	charged, err := rig.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if charged.Metadata[beadmeta.StartFailuresMetadataKey] != "1" {
		t.Fatalf("rig start failures = %q, want 1; stderr=%s", charged.Metadata[beadmeta.StartFailuresMetadataKey], stderr.String())
	}
}

// Filtering a foreign-store row and a parked row must preserve the surviving
// row's store even when independent stores contain the same bead id.
func TestPoolDemandInputsStoreRefsStayAligned(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", Dir: "riga"}}, Rigs: []config.Rig{{Name: "riga"}}}
	rows := []beads.Bead{
		{ID: "same", Status: "in_progress", Assignee: "riga/worker", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "riga/worker"}},
		{ID: "parked", Status: "in_progress", Assignee: "riga/worker", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "riga/worker", beadmeta.ParkedAtMetadataKey: "2026-09-14T12:00:00Z"}},
		{ID: "same", Status: "in_progress", Assignee: "riga/worker", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "riga/worker"}},
	}
	refs := []string{"rigb", "riga", "riga"}
	pass := newWorkStartDeferralPass(time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC), nil)
	pass.filterWithRefs(rows, refs, true)
	owned, demand, demandRefs := poolDemandAssignedWork(cfg, "", nil, nil, rows, refs, pass.deferred)
	if len(owned) != 2 || owned[0].ID != "parked" || owned[1].ID != "same" {
		t.Fatalf("owned = %#v, want parked and surviving same-id row", owned)
	}
	if len(demand) != 1 || demand[0].ID != "same" || len(demandRefs) != 1 || demandRefs[0] != "riga" {
		t.Fatalf("demand = %#v, refs = %#v, want same from riga", demand, demandRefs)
	}
	if refs[0] != "rigb" || rows[0].ID != "same" || rows[1].ID != "parked" {
		t.Fatal("filter modified the source rows or refs")
	}
}

func TestComputePoolDesiredStates_AssignedRequestStoreRefs(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}}
	for _, tc := range []struct {
		name   string
		refs   []string
		resume bool
		want   string
	}{
		{name: "wake rig", refs: []string{"riga"}, want: "rig:riga"},
		{name: "resume rig", refs: []string{"riga"}, resume: true, want: "rig:riga"},
		{name: "rig named city", refs: []string{"city"}, want: "rig:city"},
		{name: "known city", refs: []string{""}, want: "city"},
		{name: "canonical", refs: []string{"  rig:riga  "}, want: "rig:riga"},
		{name: "unknown nil"},
		{name: "unknown empty", refs: []string{}, resume: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work := beads.Bead{ID: "work", Status: "in_progress", Assignee: "worker", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}}
			var sessions []sessionpkg.Info
			wantTier := "wake-known-identity"
			if tc.resume {
				work.Assignee = "holder"
				sessions = []sessionpkg.Info{{ID: "holder", Template: "worker"}}
				wantTier = "resume"
			}
			states := ComputePoolDesiredStates(cfg, []beads.Bead{work}, tc.refs, sessions, nil)
			if len(states) != 1 || len(states[0].Requests) != 1 {
				t.Fatalf("states = %#v, want one request", states)
			}
			req := states[0].Requests[0]
			if req.Tier != wantTier || req.WorkStoreRef != tc.want {
				t.Fatalf("request = %#v, want tier %s and ref %q", req, wantTier, tc.want)
			}
		})
	}
}
