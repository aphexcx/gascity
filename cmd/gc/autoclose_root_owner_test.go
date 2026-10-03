package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

type autocloseRootReadErrorStore struct {
	*beads.MemStore
	unreadableID string
}

func (s *autocloseRootReadErrorStore) Get(id string) (beads.Bead, error) {
	if id == s.unreadableID {
		return beads.Bead{}, errors.New("root live read unavailable")
	}
	return s.MemStore.Get(id)
}

// An unlabeled discovery snapshot cannot authorize a root close when its live
// owner is foreign or its authoritative row cannot be read.
func TestRootAutocloseRefreshesOwnerBeforeWriting(t *testing.T) {
	for _, kind := range []string{"molecule", "wisp"} {
		t.Run(kind, func(t *testing.T) {
			for _, unreadable := range []bool{false, true} {
				name := "foreign_live_owner"
				if unreadable {
					name = "unreadable_live_root"
				}
				t.Run(name, func(t *testing.T) {
					backing := &autocloseRootReadErrorStore{MemStore: beads.NewMemStore()}
					root, err := backing.Create(beads.Bead{Title: "root", Type: "molecule"})
					requireNoError(t, err)
					trigger := beads.Bead{Title: "completed work", Type: "step"}
					if kind == "molecule" {
						trigger.ParentID = root.ID
					} else {
						// Metadata attachment discovery reads the cached root;
						// no live child-list row supplies its missing labels.
						trigger.Metadata = map[string]string{beadmeta.MoleculeIDMetadataKey: root.ID}
					}
					closed, err := backing.Create(trigger)
					requireNoError(t, err)
					requireNoError(t, backing.Close(closed.ID))
					recorder := events.NewFake()
					cached := beads.NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
						recorder.Record(events.Event{Type: eventType, Subject: beadID})
					})
					requireNoError(t, cached.Prime(context.Background()))
					if unreadable {
						backing.unreadableID = root.ID
					} else {
						requireNoError(t, backing.Update(root.ID, beads.UpdateOpts{Labels: []string{"owner:other-city"}}))
					}
					snapshot, err := cached.Get(root.ID)
					requireNoError(t, err)
					if len(snapshot.Labels) != 0 {
						t.Fatalf("discovery labels = %v, want stale unlabeled root", snapshot.Labels)
					}
					before, err := backing.MemStore.Get(root.ID)
					requireNoError(t, err)
					var stdout bytes.Buffer
					if kind == "molecule" {
						doMoleculeAutocloseWith(cached, "citadel", "", recorder, closed.ID, &stdout, beads.GraphStore{Store: cached})
					} else {
						doWispAutocloseWith(cached, "citadel", closed.ID, &stdout, beads.GraphStore{Store: cached})
					}
					after, err := backing.MemStore.Get(root.ID)
					requireNoError(t, err)
					if !reflect.DeepEqual(after, before) {
						t.Errorf("root changed despite rejected ownership read: before=%+v after=%+v", before, after)
					}
					recorded, err := recorder.List(events.Filter{})
					requireNoError(t, err)
					if len(recorded) != 0 || stdout.Len() != 0 {
						t.Errorf("skipped root produced events=%+v stdout=%q", recorded, stdout.String())
					}
				})
			}
		})
	}
}

// The closing bead's owner must not authorize closing a differently owned root.
func TestRootAutocloseOwnerFromController(t *testing.T) {
	previous := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = previous })

	for _, kind := range []string{"molecule", "wisp"} {
		t.Run(kind, func(t *testing.T) {
			for _, tc := range []struct {
				name     string
				identity string
				labels   []string
				want     string
			}{
				{"foreign", "citadel", []string{"owner:other-city"}, "open"},
				{"unlabeled", "citadel", nil, "closed"},
				{"missing_identity", "", []string{"owner:citadel"}, "open"},
				{"matching", "citadel", []string{"owner:citadel"}, "closed"},
				{"bare", "citadel", []string{"owner:"}, "open"},
				{"mixed", "citadel", []string{"owner:citadel", "owner:other-city"}, "open"},
				{"case_mismatch", "citadel", []string{"owner:Citadel"}, "open"},
				{"unfederated_unlabeled", "", nil, "closed"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					store := beads.NewMemStore()
					root, err := store.Create(beads.Bead{Title: "root", Type: "molecule", Labels: tc.labels})
					requireNoError(t, err)
					closed, err := store.Create(beads.Bead{Title: "completed work", Type: "step", Labels: []string{"owner:citadel"}})
					requireNoError(t, err)
					if kind == "molecule" {
						requireNoError(t, store.Update(closed.ID, beads.UpdateOpts{ParentID: &root.ID}))
					} else {
						requireNoError(t, store.Update(root.ID, beads.UpdateOpts{ParentID: &closed.ID}))
					}
					requireNoError(t, store.Close(closed.ID))
					cfg := &config.City{}
					cfg.Federation.Identity = tc.identity
					cs := &controllerState{cfg: cfg, cityBeadStore: store}
					cs.runBeadCloseAutoclose(closed.ID, store, "")
					got, err := store.Get(root.ID)
					requireNoError(t, err)
					if got.Status != tc.want {
						t.Errorf("root status = %q, want %q", got.Status, tc.want)
					}
					if tc.want == "open" && got.Metadata["close_reason"] != "" {
						t.Errorf("skipped root was mutated: %v", got.Metadata)
					}
				})
			}
		})
	}
}

func TestMoleculeAutocloseProtectsOwnedSourceRoot(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{Title: "work"})
	requireNoError(t, err)
	root, err := store.Create(beads.Bead{
		Title: "workflow", Type: "task", Labels: []string{"owner:other-city"},
		Metadata: map[string]string{
			beadmeta.KindMetadataKey: "workflow", beadmeta.FormulaContractMetadataKey: "graph.v2",
			beadmeta.SourceBeadIDMetadataKey: work.ID,
		},
	})
	requireNoError(t, err)
	requireNoError(t, store.Close(work.ID))
	var stdout bytes.Buffer
	doMoleculeAutocloseWith(store, "", "", events.Discard, work.ID, &stdout, beads.GraphStore{Store: store})
	got, err := store.Get(root.ID)
	requireNoError(t, err)
	if got.Status != "open" || got.Metadata["close_reason"] != "" || stdout.Len() != 0 {
		t.Fatalf("foreign source root was auto-closed: %+v; stdout=%s", got, &stdout)
	}
}

func TestWispAutocloseProtectsOwnedInputConvoyRoot(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{Title: "work"})
	requireNoError(t, err)
	convoy, err := store.Create(beads.Bead{Title: "input", Type: "convoy"})
	requireNoError(t, err)
	requireNoError(t, store.DepAdd(convoy.ID, work.ID, "tracks"))
	root, err := store.Create(beads.Bead{
		Title: "workflow", Type: "task", Labels: []string{"owner:other-city"},
		Metadata: map[string]string{
			beadmeta.KindMetadataKey: "workflow", beadmeta.FormulaContractMetadataKey: "graph.v2",
			beadmeta.InputConvoyIDMetadataKey: convoy.ID,
		},
	})
	requireNoError(t, err)
	requireNoError(t, store.Close(work.ID))
	var stdout bytes.Buffer
	doWispAutocloseWith(store, "", work.ID, &stdout, beads.GraphStore{Store: store})
	got, err := store.Get(root.ID)
	requireNoError(t, err)
	if got.Status != "open" || stdout.Len() != 0 {
		t.Fatalf("foreign input-convoy root was auto-closed: %+v; stdout=%s", got, &stdout)
	}
}

func TestWispAutocloseProtectsOwnedRootSidecars(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{
		Title: "workflow", Labels: []string{"owner:other-city"},
		Metadata: map[string]string{beadmeta.KindMetadataKey: "workflow", beadmeta.FormulaContractMetadataKey: "graph.v2"},
	})
	requireNoError(t, err)
	spec, err := store.Create(beads.Bead{
		Title: "spec", Type: "spec", Metadata: map[string]string{
			beadmeta.KindMetadataKey: "spec", beadmeta.RootBeadIDMetadataKey: root.ID,
			"gc.spec_for": "implement",
		},
	})
	requireNoError(t, err)
	requireNoError(t, store.Close(root.ID))
	var stdout bytes.Buffer
	doWispAutocloseWith(store, "", root.ID, &stdout, beads.GraphStore{Store: store})
	got, err := store.Get(spec.ID)
	requireNoError(t, err)
	if got.Status != "open" || stdout.Len() != 0 {
		t.Fatalf("foreign root sidecar was auto-closed: %+v; stdout=%s", got, &stdout)
	}
}
