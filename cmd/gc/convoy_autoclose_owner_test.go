package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

func TestConvoyCheckSkipsForeignOwner(t *testing.T) {
	store := beads.NewMemStore()
	convoy, err := store.Create(beads.Bead{Title: "peer batch", Type: "convoy", Labels: []string{"owner:other-city"}})
	requireNoError(t, err)
	child, err := store.Create(beads.Bead{Title: "task"})
	requireNoError(t, err)
	requireNoError(t, store.DepAdd(convoy.ID, child.ID, "tracks"))
	requireNoError(t, store.Close(child.ID))
	var output, errors bytes.Buffer
	if code := doConvoyCheck(store, events.Discard, &output, &errors); code != 0 {
		t.Fatalf("check exit=%d stderr=%s", code, &errors)
	}
	got, err := store.Get(convoy.ID)
	requireNoError(t, err)
	if got.Status != "open" {
		t.Fatalf("foreign convoy status = %q, want open", got.Status)
	}
}

func TestConvoyAutocloseOwnerFromController(t *testing.T) {
	previous := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = previous })

	for _, tc := range []struct {
		name     string
		identity string
		labels   []string
		want     string
	}{
		{"owner", "citadel", []string{"owner:citadel"}, "closed"},
		{"peer", "jadegate", []string{"owner:citadel"}, "open"},
		{"legacy", "jadegate", nil, "closed"},
		{"identity_missing", "", []string{"owner:citadel"}, "open"},
		{"unfederated_legacy", "", nil, "closed"},
		{"handoff_is_not_ownership", "jadegate", []string{"owner:citadel", "handoff:jadegate"}, "open"},
		{"conflicting_owners", "citadel", []string{"owner:citadel", "owner:jadegate"}, "open"},
		{"empty_owner", "", []string{"owner:"}, "open"},
		{"manually_owned", "citadel", []string{"owned", "owner:citadel"}, "open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backing := beads.NewMemStore()
			convoy, err := backing.Create(beads.Bead{Title: "batch", Type: "convoy"})
			requireNoError(t, err)
			child, err := backing.Create(beads.Bead{Title: "task"})
			requireNoError(t, err)
			requireNoError(t, backing.DepAdd(convoy.ID, child.ID, "tracks"))
			requireNoError(t, backing.Close(child.ID))
			cached := beads.NewCachingStoreForTest(backing, nil)
			requireNoError(t, cached.Prime(context.Background()))
			// The cache has no labels (as in a label-skipping scan). The
			// automatic writer must check the authoritative convoy row.
			requireNoError(t, backing.Update(convoy.ID, beads.UpdateOpts{Labels: tc.labels}))
			closed, err := backing.Get(child.ID)
			requireNoError(t, err)
			payload, err := beads.EncodeBeadEventPayload(closed)
			requireNoError(t, err)
			cfg := &config.City{}
			cfg.Federation.Identity = tc.identity
			cs := &controllerState{
				cfg: cfg, cityBeadStore: wrapStoreWithBeadPolicies(cached, cfg), pokeCh: make(chan struct{}, 1),
			}
			cs.applyBeadEventToStores(events.Event{Type: events.BeadClosed, Actor: cacheReconcileActor, Subject: child.ID, Payload: payload})
			got, err := backing.Get(convoy.ID)
			requireNoError(t, err)
			if got.Status != tc.want {
				t.Fatalf("convoy status = %q, want %q", got.Status, tc.want)
			}
			if tc.want == "open" && got.Metadata["close_reason"] != "" {
				t.Fatalf("skipped convoy was mutated: %v", got.Metadata)
			}
		})
	}
}

func TestConvoyAutocloseAfterWorkerCloseAndLiveAssignmentRead(t *testing.T) {
	previous := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = previous })
	backing := &beads.MemStore{IDPrefix: "hw"}
	convoy, err := backing.Create(beads.Bead{Title: "batch", Type: "convoy", Labels: []string{"owner:citadel"}})
	requireNoError(t, err)
	child, err := backing.Create(beads.Bead{Title: "worker task", Assignee: "worker"})
	requireNoError(t, err)
	requireNoError(t, backing.Update(child.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}))
	requireNoError(t, backing.DepAdd(convoy.ID, child.ID, "tracks"))
	cfg := &config.City{}
	cfg.Federation.Identity = "citadel"
	cfg.Rigs = []config.Rig{{Name: "work", Prefix: "hw"}}
	ep := events.NewFake()
	// Use the supervisor's production cache/policy composition and emitter.
	store := wrapWithCachingStore(context.Background(), wrapStoreWithBeadPolicies(backing, cfg), ep, false)
	cs := &controllerState{cfg: cfg, cityBeadStore: beads.NewMemStore(), beadStores: map[string]beads.Store{"work": store}, pokeCh: make(chan struct{}, 1)}

	// The worker writes directly to the backing store, outside the controller.
	requireNoError(t, backing.Close(child.ID))
	_, err = store.List(beads.ListQuery{Assignee: "worker", Status: "in_progress", Live: true, TierMode: beads.TierBoth})
	requireNoError(t, err)
	notifications, err := ep.List(events.Filter{Type: events.BeadClosed, Subject: child.ID})
	requireNoError(t, err)
	if len(notifications) != 1 {
		t.Fatalf("worker close notifications = %d, want one", len(notifications))
	}
	for _, evt := range notifications {
		cs.applyBeadEventToStores(evt)
	}
	got, err := backing.Get(convoy.ID)
	requireNoError(t, err)
	if got.Status != "closed" {
		t.Fatalf("convoy status after external close and live read = %q, want closed; notifications=%d", got.Status, len(notifications))
	}
}
