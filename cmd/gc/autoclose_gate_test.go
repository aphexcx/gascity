package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// The automatic writers (convoy autoclose, molecule autoclose, attached-wisp
// close, wisp GC) run in every city that holds a copy of a federated store.
// These tests are the two-city race of gp-c04p: two copies of the same rows,
// one automatic writer per city, and exactly the sole owner's copy is written.

func fencedMem(t *testing.T, identity string) (beads.Store, *beads.MemStore, *bytes.Buffer) {
	t.Helper()
	inner := beads.NewMemStore()
	var log bytes.Buffer
	return (autocloseGate{identity: identity}).fence(inner, &log, "site"), inner, &log
}

func TestAutocloseFenceIsOffForANonFederatedCity(t *testing.T) {
	inner := beads.NewMemStore()
	if got := (autocloseGate{}).fence(inner, io.Discard, "site"); got != beads.Store(inner) {
		t.Fatalf("a non-federated gate must hand the store back unwrapped")
	}
	if got := autocloseGateFor(nil).fence(inner, io.Discard, "site"); got != beads.Store(inner) {
		t.Fatalf("a nil config is the non-federated gate")
	}
	cfg := &config.City{Federation: config.FederationConfig{Identity: " citadel "}}
	if got := autocloseGateFor(cfg); got.identity != "citadel" {
		t.Fatalf("gate identity = %q, want citadel", got.identity)
	}
	fenced := (autocloseGate{identity: "citadel"}).fence(inner, io.Discard, "site")
	if again := (autocloseGate{identity: "citadel"}).fence(fenced, io.Discard, "site"); again != fenced {
		t.Fatalf("fencing twice with the same gate must be idempotent")
	}
}

func TestAutocloseFenceRuleTable(t *testing.T) {
	tests := []struct {
		name      string
		identity  string
		labels    []string
		ephemeral bool
		want      bool
		wantWhy   string
	}{
		{"sole owner is this city", "citadel", []string{"owner:citadel"}, false, true, ""},
		{"unlabelled row has no sole maintainer", "citadel", nil, false, false, "owner=none this_identity=citadel rule=sole-owner"},
		{"foreign owner", "jadegate", []string{"owner:citadel"}, false, false, "owner=citadel this_identity=jadegate rule=sole-owner"},
		{"handoff to me is a claim license, not a write license", "jadegate", []string{"owner:citadel", "handoff:jadegate"}, false, false, "owner=citadel this_identity=jadegate rule=sole-owner"},
		{"handoff away keeps the owner writing", "citadel", []string{"owner:citadel", "handoff:jadegate"}, false, true, ""},
		{"two owners", "citadel", []string{"owner:citadel", "owner:jadegate"}, false, false, "owner=citadel,jadegate this_identity=citadel rule=sole-owner"},
		{"wisp-tier rows never leave the city", "jadegate", []string{"owner:citadel"}, true, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, inner, log := fencedMem(t, tc.identity)
			b, err := inner.Create(beads.Bead{Title: "row", Labels: tc.labels, Ephemeral: tc.ephemeral})
			if err != nil {
				t.Fatal(err)
			}
			err = store.Close(b.ID)
			got, _ := inner.Get(b.ID)
			if tc.want {
				if err != nil || got.Status != "closed" {
					t.Fatalf("Close = %v, status %q; want closed", err, got.Status)
				}
				if log.Len() != 0 {
					t.Fatalf("an allowed write must log nothing, got %q", log.String())
				}
				return
			}
			if !errors.Is(err, errAutomaticWriteFenced) {
				t.Fatalf("Close = %v, want errAutomaticWriteFenced", err)
			}
			if got.Status == "closed" {
				t.Fatalf("a refused row must not be written")
			}
			want := "site: cross-city-fence refused bead=" + b.ID + " " + tc.wantWhy + "\n"
			if log.String() != want {
				t.Fatalf("log = %q, want %q", log.String(), want)
			}
			// A second refusal of the same row is not logged again.
			_ = store.Close(b.ID)
			if log.String() != want {
				t.Fatalf("refusal logged twice: %q", log.String())
			}
		})
	}
}

// Every write method that names a row goes through the fence; reads and
// Create do not.
func TestAutocloseFenceCoversEveryWrite(t *testing.T) {
	store, inner, log := fencedMem(t, "jadegate")
	foreign, _ := inner.Create(beads.Bead{Title: "citadel's", Labels: []string{"owner:citadel"}})
	mine, _ := inner.Create(beads.Bead{Title: "jadegate's", Labels: []string{"owner:jadegate"}})
	other, _ := inner.Create(beads.Bead{Title: "dep target", Labels: []string{"owner:jadegate"}})

	writes := map[string]func(id string) error{
		"Update":           func(id string) error { return store.Update(id, beads.UpdateOpts{Title: stringPtr("renamed")}) },
		"SetMetadata":      func(id string) error { return store.SetMetadata(id, "k", "v") },
		"SetMetadataBatch": func(id string) error { return store.SetMetadataBatch(id, map[string]string{"k": "v"}) },
		"SetLocalString":   func(id string) error { return store.SetLocalString(id, "k", "v") },
		"DepAdd":           func(id string) error { return store.DepAdd(id, other.ID, "blocks") },
		"DepRemove":        func(id string) error { return store.DepRemove(id, other.ID) },
		"Tx.Update": func(id string) error {
			return store.Tx("t", func(tx beads.Tx) error { return tx.Update(id, beads.UpdateOpts{Title: stringPtr("renamed")}) })
		},
		"Tx.SetMetadataBatch": func(id string) error {
			return store.Tx("t", func(tx beads.Tx) error { return tx.SetMetadataBatch(id, map[string]string{"k": "v"}) })
		},
		"Tx.Close":        func(id string) error { return store.Tx("t", func(tx beads.Tx) error { return tx.Close(id) }) },
		"CloseWithReason": func(id string) error { return store.(explicitReasonCloser).CloseWithReason(id, "done") },
		"Close":           func(id string) error { return store.Close(id) },
		"Reopen":          func(id string) error { return store.Reopen(id) },
		"Delete":          func(id string) error { return store.Delete(id) },
	}
	order := []string{"Update", "SetMetadata", "SetMetadataBatch", "SetLocalString", "DepAdd", "DepRemove", "Tx.Update", "Tx.SetMetadataBatch", "Tx.Close", "CloseWithReason", "Close", "Reopen", "Delete"}
	before, _ := inner.Get(foreign.ID)
	for _, name := range order {
		if err := writes[name](foreign.ID); !errors.Is(err, errAutomaticWriteFenced) {
			t.Errorf("%s on a foreign row = %v, want errAutomaticWriteFenced", name, err)
		}
		after, err := inner.Get(foreign.ID)
		if err != nil {
			t.Fatalf("%s deleted the foreign row: %v", name, err)
		}
		if after.Status != before.Status || after.Title != before.Title || len(after.Metadata) != len(before.Metadata) {
			t.Errorf("%s changed the foreign row: %+v", name, after)
		}
	}
	if deps, _ := inner.DepList(foreign.ID, "down"); len(deps) != 0 {
		t.Errorf("DepAdd wrote a dependency on a foreign row: %v", deps)
	}
	if n := strings.Count(log.String(), "cross-city-fence refused bead="+foreign.ID+" "); n != 1 {
		t.Errorf("refusal for the foreign row logged %d times, want once:\n%s", n, log.String())
	}
	for _, name := range []string{"Update", "SetMetadata", "SetMetadataBatch", "SetLocalString", "DepAdd", "DepRemove", "Tx.Update", "Tx.SetMetadataBatch", "CloseWithReason", "Reopen", "Close", "Delete"} {
		if err := writes[name](mine.ID); err != nil {
			t.Errorf("%s on my own row = %v, want nil", name, err)
		}
	}
	if _, err := inner.Get(mine.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("my own row should have been deleted last: %v", err)
	}
	if created, err := store.Create(beads.Bead{Title: "new"}); err != nil || created.ID == "" {
		t.Errorf("Create is not fenced: %v", err)
	}
	if _, err := store.Get(foreign.ID); err != nil {
		t.Errorf("reads pass through: %v", err)
	}
	h := beads.HandlesFor(store)
	if _, err := h.Live.Get(foreign.ID); err != nil {
		t.Errorf("live handle reads pass through: %v", err)
	}
	if err := h.Writer.Close(foreign.ID); !errors.Is(err, errAutomaticWriteFenced) {
		t.Errorf("the writer handle must be the fence: %v", err)
	}
	if err := store.Close("never-existed"); !errors.Is(err, beads.ErrNotFound) || errors.Is(err, errAutomaticWriteFenced) {
		t.Errorf("a missing row is the write's own not-found, not a refusal: %v", err)
	}
}

func TestAutocloseFenceCloseAllSkipsForeignRows(t *testing.T) {
	store, inner, log := fencedMem(t, "citadel")
	a, _ := inner.Create(beads.Bead{Title: "a", Labels: []string{"owner:citadel"}})
	b, _ := inner.Create(beads.Bead{Title: "b", Labels: []string{"owner:jadegate"}})
	c, _ := inner.Create(beads.Bead{Title: "c", Labels: []string{"owner:citadel"}})
	n, err := store.CloseAll([]string{a.ID, b.ID, c.ID}, map[string]string{"close_reason": "sweep"})
	if err != nil || n != 2 {
		t.Fatalf("CloseAll = (%d, %v), want (2, nil)", n, err)
	}
	for id, want := range map[string]string{a.ID: "closed", b.ID: "open", c.ID: "closed"} {
		if got, _ := inner.Get(id); got.Status != want {
			t.Errorf("%s status = %q, want %q", id, got.Status, want)
		}
	}
	if !strings.Contains(log.String(), "cross-city-fence refused bead="+b.ID+" owner=jadegate this_identity=citadel") {
		t.Errorf("log = %q", log.String())
	}
	if n, err := store.CloseAll([]string{b.ID}, nil); err != nil || n != 0 {
		t.Fatalf("an all-foreign batch = (%d, %v), want (0, nil)", n, err)
	}
}

func TestAutocloseFenceFailsClosedWhenTheRowCannotBeRead(t *testing.T) {
	inner := newGCStore([]beads.Bead{{ID: "x", Status: "open", Type: "task", Labels: []string{"owner:citadel"}}})
	inner.getErrors["x"] = errors.New("store unreachable")
	var log bytes.Buffer
	store := (autocloseGate{identity: "citadel"}).fence(inner, &log, "site")
	if err := store.Close("x"); !errors.Is(err, errAutomaticWriteFenced) {
		t.Fatalf("Close = %v, want errAutomaticWriteFenced", err)
	}
	delete(inner.getErrors, "x")
	if got, _ := inner.Get("x"); got.Status != "open" {
		t.Fatalf("row written despite the read failure: %q", got.Status)
	}
	if !strings.Contains(log.String(), `site: cross-city-fence refused bead=x read_error="store unreachable" this_identity=citadel rule=sole-owner`) {
		t.Fatalf("log = %q", log.String())
	}
}

// seedFederatedConvoy writes one city's copy of a citadel-owned convoy whose
// children a pull just delivered as closed: the state every city's event path
// sees after `gc dolt pull` brought the closes in.
func seedFederatedConvoy(t *testing.T, extraConvoyLabels ...string) (*beads.MemStore, string, string) {
	t.Helper()
	store := beads.NewMemStore()
	labels := append([]string{"owner:citadel"}, extraConvoyLabels...)
	convoy, err := store.Create(beads.Bead{Title: "tile batch", Type: "convoy", Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	childA, err := store.Create(beads.Bead{Title: "task A", ParentID: convoy.ID, Labels: []string{"owner:citadel"}})
	if err != nil {
		t.Fatal(err)
	}
	childB, err := store.Create(beads.Bead{Title: "task B", ParentID: convoy.ID, Labels: []string{"owner:citadel"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(childA.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(childB.ID); err != nil {
		t.Fatal(err)
	}
	return store, convoy.ID, childB.ID
}

type twoCityCase struct {
	city       string
	wantClosed bool
}

var twoCityCases = []twoCityCase{{"citadel", true}, {"jadegate", false}, {"", true}}

func TestConvoyAutocloseTwoCitiesOnlyOwnerWrites(t *testing.T) {
	for _, tc := range twoCityCases {
		t.Run("city="+tc.city, func(t *testing.T) {
			inner, convoyID, closedChild := seedFederatedConvoy(t)
			var stdout, stderr bytes.Buffer
			store := (autocloseGate{identity: tc.city}).fence(inner, &stderr, "gc convoy autoclose")
			doConvoyAutocloseWith(store, events.Discard, closedChild, &stdout, &stderr)

			got, err := inner.Get(convoyID)
			if err != nil {
				t.Fatal(err)
			}
			if closed := got.Status == "closed"; closed != tc.wantClosed {
				t.Fatalf("convoy status = %q, want closed=%v; stdout=%q stderr=%q", got.Status, tc.wantClosed, stdout.String(), stderr.String())
			}
			if tc.wantClosed {
				if !strings.Contains(stdout.String(), "Auto-closed convoy "+convoyID) {
					t.Fatalf("stdout = %q, want the announcement", stdout.String())
				}
				if strings.Contains(stderr.String(), "cross-city-fence") {
					t.Fatalf("stderr = %q, want no refusal", stderr.String())
				}
				return
			}
			if _, has := got.Metadata["close_reason"]; has {
				t.Fatalf("a refused convoy must not even carry the close_reason stamp: %v", got.Metadata)
			}
			want := "gc convoy autoclose: cross-city-fence refused bead=" + convoyID + " owner=citadel this_identity=jadegate rule=sole-owner"
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), want)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want nothing announced", stdout.String())
			}
		})
	}
}

// A handoff lets jadegate claim citadel's work; it does not make jadegate the
// convoy's maintainer. Exactly one city closes it.
func TestConvoyAutocloseHandoffDoesNotMakeTwoWriters(t *testing.T) {
	closers := 0
	for _, city := range []string{"citadel", "jadegate"} {
		inner, convoyID, closedChild := seedFederatedConvoy(t, "handoff:jadegate")
		var stdout, stderr bytes.Buffer
		doConvoyAutocloseWith(autocloseGate{identity: city}.fence(inner, &stderr, "gc convoy autoclose"), events.Discard, closedChild, &stdout, &stderr)
		if got, _ := inner.Get(convoyID); got.Status == "closed" {
			closers++
			if city != "citadel" {
				t.Errorf("%s closed a convoy it does not own", city)
			}
		}
	}
	if closers != 1 {
		t.Fatalf("%d cities closed the handed-off convoy, want exactly the owner", closers)
	}
}

func TestConvoyCheckTwoCitiesOnlyOwnerWrites(t *testing.T) {
	for _, jsonOut := range []bool{false, true} {
		for _, tc := range twoCityCases {
			t.Run("city="+tc.city, func(t *testing.T) {
				inner, convoyID, _ := seedFederatedConvoy(t)
				var stdout, stderr bytes.Buffer
				views := []convoyStoreView{{store: (autocloseGate{identity: tc.city}).fence(inner, &stderr, "gc convoy check")}}
				if code := doConvoyCheckAcrossStoresJSON(views, events.Discard, jsonOut, &stdout, &stderr); code != 0 {
					t.Fatalf("exit = %d, want 0 (a refused row is a skip, never a failure); stderr=%q", code, stderr.String())
				}
				got, err := inner.Get(convoyID)
				if err != nil {
					t.Fatal(err)
				}
				if closed := got.Status == "closed"; closed != tc.wantClosed {
					t.Fatalf("convoy status = %q, want closed=%v; stdout=%q stderr=%q", got.Status, tc.wantClosed, stdout.String(), stderr.String())
				}
				wantCount := 0
				if tc.wantClosed {
					wantCount = 1
				}
				if jsonOut {
					var res convoyActionResult
					if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &res); err != nil {
						t.Fatalf("json: %v (%q)", err, stdout.String())
					}
					if res.Closed == nil || *res.Closed != wantCount {
						t.Fatalf("closed = %v, want %d", res.Closed, wantCount)
					}
				} else if !strings.Contains(stdout.String(), string(rune('0'+wantCount))+" convoy(s) auto-closed") {
					t.Fatalf("stdout = %q, want %d convoy(s) auto-closed", stdout.String(), wantCount)
				}
				refused := strings.Contains(stderr.String(), "gc convoy check: cross-city-fence refused bead="+convoyID+" owner=citadel this_identity=jadegate")
				if refused == tc.wantClosed {
					t.Fatalf("refusal logged = %v, want %v; stderr=%q", refused, !tc.wantClosed, stderr.String())
				}
			})
		}
	}
}

func TestConvoyCheckLogsRefusalOnlyAtTheWrite(t *testing.T) {
	// A foreign convoy that still has an open child is not written by
	// anyone; the sweep must stay silent about it rather than log a refusal
	// for every foreign convoy on every run.
	inner := beads.NewMemStore()
	convoy, _ := inner.Create(beads.Bead{Title: "in flight", Type: "convoy", Labels: []string{"owner:citadel"}})
	_, _ = inner.Create(beads.Bead{Title: "open task", ParentID: convoy.ID})
	var stdout, stderr bytes.Buffer
	views := []convoyStoreView{{store: (autocloseGate{identity: "jadegate"}).fence(inner, &stderr, "gc convoy check")}}
	if code := doConvoyCheckAcrossStoresJSON(views, events.Discard, false, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "cross-city-fence") {
		t.Fatalf("stderr = %q, want no refusal for a convoy nobody would write", stderr.String())
	}
}

func TestMoleculeAutocloseTwoCitiesOnlyOwnerWrites(t *testing.T) {
	for _, tc := range twoCityCases {
		t.Run("city="+tc.city, func(t *testing.T) {
			inner := beads.NewMemStore()
			root, _ := inner.Create(beads.Bead{Title: "mol-review", Type: "molecule", Labels: []string{"owner:citadel"}})
			step, _ := inner.Create(beads.Bead{Title: "Run tests", Type: "step", ParentID: root.ID, Labels: []string{"owner:citadel"}})
			_ = inner.Close(step.ID)

			var out, stderr bytes.Buffer
			doMoleculeAutocloseWith(autocloseGate{identity: tc.city}.fence(inner, &stderr, "gc molecule autoclose"), "", events.Discard, step.ID, &out)

			got, err := inner.Get(root.ID)
			if err != nil {
				t.Fatal(err)
			}
			if closed := got.Status == "closed"; closed != tc.wantClosed {
				t.Fatalf("root status = %q, want closed=%v; out=%q stderr=%q", got.Status, tc.wantClosed, out.String(), stderr.String())
			}
			if tc.wantClosed {
				if !strings.Contains(out.String(), "Auto-closed molecule "+root.ID) {
					t.Fatalf("out = %q, want the announcement", out.String())
				}
				return
			}
			if _, has := got.Metadata["close_reason"]; has {
				t.Fatalf("a refused root must not carry the close_reason stamp: %v", got.Metadata)
			}
			if out.Len() != 0 {
				t.Fatalf("out = %q, want no announcement for a refused root", out.String())
			}
			want := "gc molecule autoclose: cross-city-fence refused bead=" + root.ID + " owner=citadel this_identity=jadegate rule=sole-owner"
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), want)
			}
		})
	}
}

// The attached-wisp close (a closed work bead takes its attached molecule
// root and subtree down with it) is the path the molecule gate never saw.
func TestWispAutocloseTwoCitiesOnlyOwnerWrites(t *testing.T) {
	for _, tc := range twoCityCases {
		t.Run("city="+tc.city, func(t *testing.T) {
			inner := beads.NewMemStore()
			work, _ := inner.Create(beads.Bead{Title: "work item", Labels: []string{"owner:citadel"}})
			mol, _ := inner.Create(beads.Bead{Title: "attached", Type: "molecule", ParentID: work.ID, Labels: []string{"owner:citadel"}})
			step, _ := inner.Create(beads.Bead{Title: "step", Type: "step", ParentID: mol.ID, Labels: []string{"owner:citadel"}})
			_ = inner.Close(step.ID) // an open step would park the molecule; the root is what the hook reaps
			_ = inner.Close(work.ID)

			var stdout, stderr bytes.Buffer
			doWispAutocloseWith(autocloseGate{identity: tc.city}.fence(inner, &stderr, "gc wisp autoclose"), work.ID, &stdout)

			for _, id := range []string{mol.ID} {
				got, err := inner.Get(id)
				if err != nil {
					t.Fatal(err)
				}
				if closed := got.Status == "closed"; closed != tc.wantClosed {
					t.Fatalf("%s status = %q, want closed=%v; stdout=%q stderr=%q", id, got.Status, tc.wantClosed, stdout.String(), stderr.String())
				}
			}
			refused := strings.Contains(stderr.String(), "gc wisp autoclose: cross-city-fence refused bead=")
			if refused == tc.wantClosed {
				t.Fatalf("refusal logged = %v, want %v; stderr=%q", refused, !tc.wantClosed, stderr.String())
			}
		})
	}
}

// The wisp GC tick's repair sweeps (spec sidecars of closed roots, abandoned
// open roots) and its purge are automatic writers too.
func TestWispGCTickTwoCitiesOnlyOwnerWrites(t *testing.T) {
	now := time.Now()
	for _, tc := range twoCityCases {
		t.Run("city="+tc.city, func(t *testing.T) {
			old := now.Add(-30 * time.Minute)
			store := newGCStore([]beads.Bead{
				{ID: "closed-workflow", Status: "closed", Type: "task", CreatedAt: old, UpdatedAt: old, Labels: []string{"owner:citadel"}, Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"}},
				{ID: "closed-workflow-spec", Status: "open", Type: "spec", CreatedAt: old, UpdatedAt: old, Labels: []string{"owner:citadel"}, Metadata: map[string]string{"gc.kind": "spec", "gc.root_bead_id": "closed-workflow", "gc.spec_for": "implement"}},
				{ID: "mol-root", Status: "open", Type: "molecule", CreatedAt: old, UpdatedAt: old, Labels: []string{"owner:citadel"}, Metadata: map[string]string{"gc.formula_contract": "graph.v2"}},
				{ID: "mol-root.1", Status: "closed", Type: "task", CreatedAt: old, ParentID: "mol-root", Labels: []string{"owner:citadel"}},
				{ID: "expired-root", Status: "closed", Type: "molecule", CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-3 * time.Hour), Labels: []string{"owner:citadel"}},
			})
			if err := store.DepAdd("mol-root.1", "mol-root", "parent-child"); err != nil {
				t.Fatal(err)
			}
			var log bytes.Buffer
			fenced := (autocloseGate{identity: tc.city}).fence(store, &log, "wisp gc")
			withCloseAbandonedEnforced(t, func() {
				withCloseAbandonedTTL(t, 5*time.Minute, func() {
					wg := newWispGC(5*time.Minute, time.Hour, 0)
					if _, err := wg.runGC(beads.GraphStore{Store: fenced}, beads.MailStore{Store: fenced}, now); err != nil {
						t.Fatalf("runGC: %v (a refused row is a skip, never a tick failure)", err)
					}
				})
			})
			spec, _ := store.Get("closed-workflow-spec")
			root, _ := store.Get("mol-root")
			_, expiredErr := store.Get("expired-root")
			purged := errors.Is(expiredErr, beads.ErrNotFound)
			if closed := spec.Status == "closed"; closed != tc.wantClosed {
				t.Errorf("spec sidecar closed = %v, want %v", closed, tc.wantClosed)
			}
			if closed := root.Status == "closed"; closed != tc.wantClosed {
				t.Errorf("abandoned root closed = %v, want %v", closed, tc.wantClosed)
			}
			if purged != tc.wantClosed {
				t.Errorf("expired root purged = %v, want %v", purged, tc.wantClosed)
			}
			if refused := strings.Contains(log.String(), "wisp gc: cross-city-fence refused bead="); refused == tc.wantClosed {
				t.Errorf("refusal logged = %v, want %v; log=%q", refused, !tc.wantClosed, log.String())
			}
		})
	}
}

// The controller's bead-close event path is the one that fired on jadegate:
// a pull delivered citadel's closes, the cache reconcile emitted bead.closed
// for them, and runBeadCloseAutoclose closed citadel's convoy in jadegate's
// copy. Its fence is built from the loaded city config.
func TestApplyBeadEventToStoresAutocloseHonorsFederationIdentity(t *testing.T) {
	prev := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = prev })

	for _, tc := range twoCityCases {
		t.Run("city="+tc.city, func(t *testing.T) {
			backing := beads.NewMemStore()
			convoy, err := backing.Create(beads.Bead{Title: "tile batch", Type: "convoy", Labels: []string{"owner:citadel"}})
			if err != nil {
				t.Fatal(err)
			}
			child, err := backing.Create(beads.Bead{Title: "task", ParentID: convoy.ID, Labels: []string{"owner:citadel"}})
			if err != nil {
				t.Fatal(err)
			}
			cached := beads.NewCachingStoreForTest(backing, nil)
			if err := cached.Prime(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := backing.Close(child.ID); err != nil {
				t.Fatal(err)
			}
			if err := cached.Update(child.ID, beads.UpdateOpts{Status: stringPtr("closed")}); err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(beads.Bead{ID: child.ID, Title: "task", Status: "closed", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			cs := &controllerState{
				cfg:        &config.City{Federation: config.FederationConfig{Identity: tc.city}},
				beadStores: map[string]beads.Store{"test": cached},
				pokeCh:     make(chan struct{}, 1),
			}
			cs.applyBeadEventToStores(events.Event{Type: events.BeadClosed, Actor: "cache-reconcile", Subject: child.ID, Payload: payload})

			got, err := backing.Get(convoy.ID)
			if err != nil {
				t.Fatal(err)
			}
			if closed := got.Status == "closed"; closed != tc.wantClosed {
				t.Fatalf("convoy status = %q, want closed=%v", got.Status, tc.wantClosed)
			}
		})
	}
}

func seedFederatedConvoyInCity(t *testing.T, cityPath string) (beads.Store, string, string) {
	t.Helper()
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	convoy, err := store.Create(beads.Bead{Title: "tile batch", Type: "convoy", Labels: []string{"owner:citadel"}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Create(beads.Bead{Title: "task", ParentID: convoy.ID, Labels: []string{"owner:citadel"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(child.ID); err != nil {
		t.Fatal(err)
	}
	return store, convoy.ID, child.ID
}

// `gc convoy check` opens every store through openAllConvoyStoresAt, which
// must hand each one through this city's fence from city.toml.
func TestConvoyCheckCommandHonorsFederationIdentity(t *testing.T) {
	for _, tc := range twoCityCases {
		t.Run("city="+tc.city, func(t *testing.T) {
			cityPath := writeConvoyTestCityWithFederation(t, tc.city)
			store, convoyID, _ := seedFederatedConvoyInCity(t, cityPath)

			var stdout, stderr bytes.Buffer
			if code := routeConvoyCheck(cityPath, nil, "controller-down", false, &stdout, &stderr); code != 0 {
				t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
			}
			got, err := store.Get(convoyID)
			if err != nil {
				t.Fatal(err)
			}
			if closed := got.Status == "closed"; closed != tc.wantClosed {
				t.Fatalf("convoy status = %q, want closed=%v; stdout=%q stderr=%q", got.Status, tc.wantClosed, stdout.String(), stderr.String())
			}
			refused := strings.Contains(stderr.String(), "cross-city-fence refused bead="+convoyID)
			if refused == tc.wantClosed {
				t.Fatalf("refusal logged = %v, want %v; stderr=%q", refused, !tc.wantClosed, stderr.String())
			}
		})
	}
}

// The hidden `gc convoy autoclose <id>` hook entry resolves the owning store
// and this city's fence through autocloseOwningStore.
func TestConvoyAutocloseHookHonorsFederationIdentity(t *testing.T) {
	for _, tc := range []twoCityCase{{"citadel", true}, {"jadegate", false}} {
		t.Run("city="+tc.city, func(t *testing.T) {
			cityPath := writeConvoyTestCityWithFederation(t, tc.city)
			t.Setenv("GC_STORE_ROOT", cityPath)
			store, convoyID, childID := seedFederatedConvoyInCity(t, cityPath)

			var stdout, stderr bytes.Buffer
			doConvoyAutoclose(childID, &stdout, &stderr)

			got, err := store.Get(convoyID)
			if err != nil {
				t.Fatal(err)
			}
			if closed := got.Status == "closed"; closed != tc.wantClosed {
				t.Fatalf("convoy status = %q, want closed=%v; stdout=%q stderr=%q", got.Status, tc.wantClosed, stdout.String(), stderr.String())
			}
		})
	}
}

// A city.toml that exists but cannot be loaded proves nothing about the
// city's federation identity: the hook entries write nothing.
func TestConvoyAutocloseHookVetoesWhenCityConfigIsUnreadable(t *testing.T) {
	cityPath := writeConvoyTestCityWithFederation(t, "citadel")
	t.Setenv("GC_STORE_ROOT", cityPath)
	store, convoyID, childID := seedFederatedConvoyInCity(t, cityPath)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace\nname = broken"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	doConvoyAutoclose(childID, &stdout, &stderr)

	got, err := store.Get(convoyID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == "closed" {
		t.Fatalf("convoy closed under an unreadable city.toml; stderr=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "not autoclosing — "+filepath.Join(cityPath, "city.toml")+" could not be loaded, so this city's federation identity cannot be proven") {
		t.Fatalf("stderr = %q, want the veto line", stderr.String())
	}
}

// txGuardStore fails any read issued while its transaction is open — the
// native dolt store holds a lock across the callback, and a reconnecting
// read inside would wait on it forever.
type txGuardStore struct {
	*beads.MemStore
	inTx          bool
	txRuns        int
	readsDuringTx int
}

func (s *txGuardStore) Tx(msg string, fn func(beads.Tx) error) error {
	s.inTx = true
	s.txRuns++
	defer func() { s.inTx = false }()
	return s.MemStore.Tx(msg, fn)
}

func (s *txGuardStore) Get(id string) (beads.Bead, error) {
	if s.inTx {
		s.readsDuringTx++
		return beads.Bead{}, errors.New("read inside an open transaction")
	}
	return s.MemStore.Get(id)
}

func TestAutocloseFenceTxAuthorizesOutsideTheTransaction(t *testing.T) {
	inner := &txGuardStore{MemStore: beads.NewMemStore()}
	mine, _ := inner.Create(beads.Bead{Title: "mine", Labels: []string{"owner:citadel"}})
	foreign, _ := inner.Create(beads.Bead{Title: "theirs", Labels: []string{"owner:jadegate"}})
	var log bytes.Buffer
	store := (autocloseGate{identity: "citadel"}).fence(inner, &log, "site")

	err := store.Tx("close mine", func(tx beads.Tx) error {
		if err := tx.SetMetadataBatch(mine.ID, map[string]string{"close_reason": "done"}); err != nil {
			return err
		}
		return tx.Close(mine.ID)
	})
	if err != nil {
		t.Fatalf("Tx on my own row = %v", err)
	}
	if got, _ := inner.MemStore.Get(mine.ID); got.Status != "closed" || got.Metadata["close_reason"] != "done" {
		t.Fatalf("my row not written by the transaction: %+v", got)
	}
	if inner.readsDuringTx != 0 || inner.txRuns != 1 {
		t.Fatalf("reads during tx = %d, tx runs = %d; want 0 and 1", inner.readsDuringTx, inner.txRuns)
	}

	err = store.Tx("close theirs", func(tx beads.Tx) error { return tx.Close(foreign.ID) })
	if !errors.Is(err, errAutomaticWriteFenced) {
		t.Fatalf("Tx on a foreign row = %v, want errAutomaticWriteFenced", err)
	}
	if inner.txRuns != 1 {
		t.Fatalf("a refused transaction must never open; tx runs = %d", inner.txRuns)
	}
	if got, _ := inner.MemStore.Get(foreign.ID); got.Status != "open" {
		t.Fatalf("foreign row written: %+v", got)
	}

	boom := errors.New("callback failed")
	if err := store.Tx("fails", func(_ beads.Tx) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("callback error = %v, want it returned as is", err)
	}
	if inner.txRuns != 1 {
		t.Fatalf("a failing callback must not open the transaction; tx runs = %d", inner.txRuns)
	}
}

// The nudge-mail watchdog closes permanent read messages and nudge beads
// with no agent behind it — an automatic writer in every city.
func TestNudgeMailWatchdogTwoCitiesOnlyOwnerWrites(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	for _, tc := range twoCityCases {
		t.Run("city="+tc.city, func(t *testing.T) {
			nudge := nudgeSeed("nudge-old", "nudge-old", now.Add(-time.Hour))
			nudge.Labels = append(nudge.Labels, "owner:citadel")
			mail := mailSeed("mail-old", now.Add(-2*time.Hour))
			mail.Labels = append(mail.Labels, "owner:citadel")
			inner := beads.NewMemStoreFrom(100, []beads.Bead{nudge, mail}, nil)
			var log bytes.Buffer
			fenced := (autocloseGate{identity: tc.city}).fence(inner, &log, "watchdog")

			result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: fenced}, beads.MailStore{Store: fenced}, nil, now, 10*time.Minute, 30*time.Minute, 0)
			if err != nil {
				t.Fatalf("sweep: %v (a refused row is a skip, never a sweep error)", err)
			}
			want := 0
			if tc.wantClosed {
				want = 1
			}
			if result.NudgeClosed != want || result.MailClosed != want {
				t.Fatalf("closed nudge=%d mail=%d, want %d each; log=%q", result.NudgeClosed, result.MailClosed, want, log.String())
			}
			for _, id := range []string{"nudge-old", "mail-old"} {
				if got, _ := inner.Get(id); (got.Status == "closed") != tc.wantClosed {
					t.Errorf("%s status = %q, want closed=%v", id, got.Status, tc.wantClosed)
				}
			}
			if refused := strings.Contains(log.String(), "watchdog: cross-city-fence refused bead="); refused == tc.wantClosed {
				t.Errorf("refusal logged = %v, want %v; log=%q", refused, !tc.wantClosed, log.String())
			}
		})
	}
}

// A city.toml that is a broken symlink exists and cannot be loaded: not a
// confirmed absence, so the hook entries veto.
func TestConvoyAutocloseHookVetoesWhenCityTomlIsABrokenSymlink(t *testing.T) {
	cityPath := writeConvoyTestCityWithFederation(t, "citadel")
	t.Setenv("GC_STORE_ROOT", cityPath)
	store, convoyID, childID := seedFederatedConvoyInCity(t, cityPath)
	tomlPath := filepath.Join(cityPath, "city.toml")
	if err := os.Remove(tomlPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(cityPath, "missing-target.toml"), tomlPath); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	doConvoyAutoclose(childID, &stdout, &stderr)

	got, err := store.Get(convoyID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == "closed" {
		t.Fatalf("convoy closed under a broken city.toml symlink; stderr=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "could not be loaded, so this city's federation identity cannot be proven") {
		t.Fatalf("stderr = %q, want the veto line", stderr.String())
	}
}

// The runtime hands its maintenance jobs fenced stores on a federated city
// and bare stores otherwise; the convergence scopes are built from them.
func TestRuntimeMaintenanceStoresAreFenced(t *testing.T) {
	for _, tc := range twoCityCases {
		t.Run("city="+tc.city, func(t *testing.T) {
			inner := beads.NewMemStore()
			var stderr bytes.Buffer
			cr := &CityRuntime{
				cfg:                 &config.City{Federation: config.FederationConfig{Identity: tc.city}},
				stderr:              &stderr,
				logPrefix:           "runtime",
				standaloneCityStore: inner,
				standaloneRigStores: map[string]beads.Store{"hw": beads.NewMemStore()},
			}
			wantFenced := tc.city != ""
			fenced := cr.fenceMaintenance(inner, "job")
			if _, isFenced := fenced.(*fencedStore); isFenced != wantFenced {
				t.Fatalf("fenceMaintenance fenced = %v, want %v", isFenced, wantFenced)
			}
			scopes := cr.buildConvergenceScopes()
			if len(scopes) != 2 {
				t.Fatalf("scopes = %d, want city + rig", len(scopes))
			}
			for name, scope := range scopes {
				if _, isFenced := scope.store.(*fencedStore); isFenced != wantFenced {
					t.Errorf("convergence scope %q fenced = %v, want %v", name, isFenced, wantFenced)
				}
			}
		})
	}
}

// A callback whose control flow depends on write results can name a row on
// the real pass it did not name on the recording pass; that write is refused
// inside the transaction, without a read.
func TestAutocloseFenceTxRefusesUnrecordedWrites(t *testing.T) {
	inner := &txGuardStore{MemStore: beads.NewMemStore()}
	foreign, _ := inner.Create(beads.Bead{Title: "theirs", Labels: []string{"owner:jadegate"}})
	var log bytes.Buffer
	store := (autocloseGate{identity: "citadel"}).fence(inner, &log, "site")

	err := store.Tx("sneaky", func(tx beads.Tx) error {
		if err := tx.Close("missing"); err != nil {
			// The recorder answered success; the real transaction says not
			// found — and the callback reaches for a row it never named.
			return tx.Close(foreign.ID)
		}
		return nil
	})
	if !errors.Is(err, errAutomaticWriteFenced) {
		t.Fatalf("Tx = %v, want errAutomaticWriteFenced for the unrecorded write", err)
	}
	if got, _ := inner.MemStore.Get(foreign.ID); got.Status != "open" {
		t.Fatalf("foreign row written on the real pass: %+v", got)
	}
	if inner.readsDuringTx != 0 {
		t.Fatalf("reads during tx = %d, want 0", inner.readsDuringTx)
	}
	if !strings.Contains(log.String(), "cross-city-fence refused bead="+foreign.ID+" this_identity=citadel rule=sole-owner tx=unrecorded-write") {
		t.Fatalf("log = %q", log.String())
	}
}

// A bounded sweep must not spend its budget on rows the fence refuses, or
// another city's rows at the front of the list starve this city's own.
func TestOrderTrackingSweepsSkipForeignRowsBeforeSpendingTheBudget(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)
	seed := func(t *testing.T) (beads.Store, *beads.MemStore, []string, string) {
		t.Helper()
		var seedBeads []beads.Bead
		var foreign []string
		for _, name := range []string{"a", "b", "c", "d"} {
			id := "track-" + name
			seedBeads = append(seedBeads, beads.Bead{ID: id, Title: "order:" + name, Status: "open", Labels: []string{"order-run:" + name, labelOrderTracking, "owner:jadegate"}, CreatedAt: old})
			foreign = append(foreign, id)
		}
		seedBeads = append(seedBeads, beads.Bead{ID: "track-mine", Title: "order:mine", Status: "open", Labels: []string{"order-run:mine", labelOrderTracking, "owner:citadel"}, CreatedAt: old})
		inner := beads.NewMemStoreFrom(100, seedBeads, nil)
		return (autocloseGate{identity: "citadel"}).fence(inner, io.Discard, "sweep"), inner, foreign, "track-mine"
	}
	assertOnlyMine := func(t *testing.T, inner *beads.MemStore, foreign []string, mine string) {
		t.Helper()
		if got, _ := inner.Get(mine); got.Status != "closed" {
			t.Fatalf("my own tracking row not closed: %q", got.Status)
		}
		for _, id := range foreign {
			if got, _ := inner.Get(id); got.Status != "open" {
				t.Fatalf("foreign tracking row %s written: %q", id, got.Status)
			}
		}
	}

	t.Run("orphaned", func(t *testing.T) {
		store, inner, foreign, mine := seed(t)
		closed, err := sweepOrphanedOrderTrackingLimit(store, 1)
		if err != nil || closed != 1 {
			t.Fatalf("sweep = (%d, %v), want (1, nil)", closed, err)
		}
		assertOnlyMine(t, inner, foreign, mine)
	})
	t.Run("stale", func(t *testing.T) {
		store, inner, foreign, mine := seed(t)
		result, err := sweepStaleOrderTrackingWithOptionsLimit(store, time.Now(), time.Hour, nil, "watchdog", false, 1)
		if err != nil || result.trackingClosed != 1 {
			t.Fatalf("sweep = (%+v, %v), want one tracking row closed", result, err)
		}
		assertOnlyMine(t, inner, foreign, mine)
	})
}

// Route recovery rewrites gc.routed_to on open, unassigned beads with no
// agent behind it; only the row's own city does.
func TestRouteRecoveryTwoCitiesOnlyOwnerWrites(t *testing.T) {
	for _, tc := range twoCityCases {
		t.Run("city="+tc.city, func(t *testing.T) {
			inner := beads.NewMemStore()
			b, err := inner.Create(beads.Bead{Title: "unrouted", Labels: []string{"owner:citadel"}, Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "pool"}})
			if err != nil {
				t.Fatal(err)
			}
			var log bytes.Buffer
			lane := newRouteRecoveryLane()
			lane.fence = func(s beads.Store) beads.Store {
				return (autocloseGate{identity: tc.city}).fence(s, &log, "route recovery")
			}
			live, _ := inner.Get(b.ID)
			out := lane.restoreRoute(lane.fenced(inner), live, false)
			if out.err != nil {
				t.Fatalf("restoreRoute err = %v (a refused row is a skip, never an error)", out.err)
			}
			got, _ := inner.Get(b.ID)
			routed := got.Metadata[beadmeta.RoutedToMetadataKey] == "pool"
			if routed != tc.wantClosed || out.restored != tc.wantClosed {
				t.Fatalf("routed = %v restored = %v, want %v; log=%q", routed, out.restored, tc.wantClosed, log.String())
			}
		})
	}
	// The runtime arms the lane with this city's fence.
	cr := &CityRuntime{cfg: &config.City{Federation: config.FederationConfig{Identity: "jadegate"}}, stderr: io.Discard}
	lane := cr.routeRecoveryLaneOf()
	if _, ok := lane.fenced(beads.NewMemStore()).(*fencedStore); !ok {
		t.Fatalf("the runtime's lane must fence a leg's store on a federated city")
	}
}
