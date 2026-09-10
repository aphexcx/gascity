package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// The automatic writers (convoy autoclose, molecule autoclose) run in every
// city that holds a copy of a federated store. These tests are the two-city
// race of gp-c04p: two copies of the same rows, one automatic writer per
// city, and exactly the owner's copy is written.

func TestAutocloseGateAllowsTable(t *testing.T) {
	tests := []struct {
		name     string
		identity string
		labels   []string
		want     bool
	}{
		{"non-federated city writes anything", "", []string{"owner:citadel"}, true},
		{"unlabelled legacy row is anyone's", "jadegate", nil, true},
		{"own owner label", "citadel", []string{"owner:citadel"}, true},
		{"foreign owner label refused", "jadegate", []string{"owner:citadel"}, false},
		{"foreign owner with handoff to me", "jadegate", []string{"owner:citadel", "handoff:jadegate"}, true},
		{"foreign owner with handoff to someone else", "jadegate", []string{"owner:citadel", "handoff:boomtown"}, false},
		{"two owners is a conflict, not a license", "citadel", []string{"owner:citadel", "owner:jadegate"}, false},
		{"identity is trimmed", " citadel ", []string{"owner:citadel"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var log bytes.Buffer
			gate := autocloseGate{identity: strings.TrimSpace(tc.identity)}
			got := gate.allows(beads.Bead{ID: "hw-1", Labels: tc.labels}, &log, "site")
			if got != tc.want {
				t.Fatalf("allows = %v, want %v (log %q)", got, tc.want, log.String())
			}
			if got && log.Len() != 0 {
				t.Fatalf("an allowed write must log nothing, got %q", log.String())
			}
			if !got && !strings.Contains(log.String(), "site: cross-city-fence refused bead=hw-1 ") {
				t.Fatalf("refusal log = %q, want the one greppable refusal line", log.String())
			}
		})
	}
}

func TestAutocloseGateForConfig(t *testing.T) {
	if got := autocloseGateFor(nil); got != (autocloseGate{}) {
		t.Fatalf("nil config gate = %+v, want zero", got)
	}
	cfg := &config.City{Federation: config.FederationConfig{Identity: " citadel "}}
	if got := autocloseGateFor(cfg); got.identity != "citadel" {
		t.Fatalf("gate identity = %q, want citadel", got.identity)
	}
}

// seedFederatedConvoy writes one city's copy of a citadel-owned convoy whose
// children a pull just delivered as closed: the state every city's event path
// sees after `gc dolt pull` brought the closes in.
func seedFederatedConvoy(t *testing.T) (beads.Store, string, string) {
	t.Helper()
	store := beads.NewMemStore()
	convoy, err := store.Create(beads.Bead{Title: "tile batch", Type: "convoy", Labels: []string{"owner:citadel"}})
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

func TestConvoyAutocloseTwoCitiesOnlyOwnerWrites(t *testing.T) {
	tests := []struct {
		city       string
		wantClosed bool
	}{
		{"citadel", true},
		{"jadegate", false},
		{"", true}, // a non-federated city keeps today's behavior
	}
	for _, tc := range tests {
		t.Run("city="+tc.city, func(t *testing.T) {
			store, convoyID, closedChild := seedFederatedConvoy(t)
			var stdout, stderr bytes.Buffer
			doConvoyAutocloseWith(store, autocloseGate{identity: tc.city}, events.Discard, closedChild, &stdout, &stderr)

			got, err := store.Get(convoyID)
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
			want := "gc convoy autoclose: cross-city-fence refused bead=" + convoyID + " owner=citadel this_identity=jadegate missing=handoff:jadegate"
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), want)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want nothing announced", stdout.String())
			}
		})
	}
}

func TestConvoyCheckTwoCitiesOnlyOwnerWrites(t *testing.T) {
	for _, jsonOut := range []bool{false, true} {
		for _, tc := range []struct {
			city       string
			wantClosed bool
		}{{"citadel", true}, {"jadegate", false}, {"", true}} {
			t.Run("city="+tc.city, func(t *testing.T) {
				store, convoyID, _ := seedFederatedConvoy(t)
				var stdout, stderr bytes.Buffer
				views := []convoyStoreView{{store: store, gate: autocloseGate{identity: tc.city}}}
				if code := doConvoyCheckAcrossStoresJSON(views, events.Discard, jsonOut, &stdout, &stderr); code != 0 {
					t.Fatalf("exit = %d, want 0; stderr=%q", code, stderr.String())
				}
				got, err := store.Get(convoyID)
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
	store := beads.NewMemStore()
	convoy, _ := store.Create(beads.Bead{Title: "in flight", Type: "convoy", Labels: []string{"owner:citadel"}})
	_, _ = store.Create(beads.Bead{Title: "open task", ParentID: convoy.ID})
	var stdout, stderr bytes.Buffer
	views := []convoyStoreView{{store: store, gate: autocloseGate{identity: "jadegate"}}}
	if code := doConvoyCheckAcrossStoresJSON(views, events.Discard, false, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "cross-city-fence") {
		t.Fatalf("stderr = %q, want no refusal for a convoy nobody would write", stderr.String())
	}
}

func TestMoleculeAutocloseTwoCitiesOnlyOwnerWrites(t *testing.T) {
	for _, tc := range []struct {
		city       string
		wantClosed bool
	}{{"citadel", true}, {"jadegate", false}, {"", true}} {
		t.Run("city="+tc.city, func(t *testing.T) {
			store := beads.NewMemStore()
			root, _ := store.Create(beads.Bead{Title: "mol-review", Type: "molecule", Labels: []string{"owner:citadel"}})
			step, _ := store.Create(beads.Bead{Title: "Run tests", Type: "step", ParentID: root.ID, Labels: []string{"owner:citadel"}})
			_ = store.Close(step.ID)

			var out bytes.Buffer
			doMoleculeAutocloseWith(store, "", autocloseGate{identity: tc.city}, events.Discard, step.ID, &out)

			got, err := store.Get(root.ID)
			if err != nil {
				t.Fatal(err)
			}
			if closed := got.Status == "closed"; closed != tc.wantClosed {
				t.Fatalf("root status = %q, want closed=%v; out=%q", got.Status, tc.wantClosed, out.String())
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
			want := "gc molecule autoclose: cross-city-fence refused bead=" + root.ID + " owner=citadel this_identity=jadegate missing=handoff:jadegate"
			if !strings.Contains(out.String(), want) {
				t.Fatalf("out = %q, want %q", out.String(), want)
			}
		})
	}
}

// The controller's bead-close event path is the one that fired on jadegate:
// a pull delivered citadel's closes, the cache reconcile emitted bead.closed
// for them, and runBeadCloseAutoclose closed citadel's convoy in jadegate's
// copy. The gate it applies is built from the loaded city config.
func TestApplyBeadEventToStoresAutocloseHonorsFederationIdentity(t *testing.T) {
	prev := beadCloseAutocloseDispatch
	beadCloseAutocloseDispatch = func(fn func()) { fn() }
	t.Cleanup(func() { beadCloseAutocloseDispatch = prev })

	for _, tc := range []struct {
		city       string
		wantClosed bool
	}{{"citadel", true}, {"jadegate", false}, {"", true}} {
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

// `gc convoy check` opens every store through openAllConvoyStoresAt, which
// must hand each store this city's gate from city.toml.
func TestConvoyCheckCommandHonorsFederationIdentity(t *testing.T) {
	for _, tc := range []struct {
		city       string
		wantClosed bool
	}{{"citadel", true}, {"jadegate", false}, {"", true}} {
		t.Run("city="+tc.city, func(t *testing.T) {
			cityPath := writeConvoyTestCityWithFederation(t, tc.city)
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

			var stdout, stderr bytes.Buffer
			if code := routeConvoyCheck(cityPath, nil, "controller-down", false, &stdout, &stderr); code != 0 {
				t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
			}
			got, err := store.Get(convoy.ID)
			if err != nil {
				t.Fatal(err)
			}
			if closed := got.Status == "closed"; closed != tc.wantClosed {
				t.Fatalf("convoy status = %q, want closed=%v; stdout=%q stderr=%q", got.Status, tc.wantClosed, stdout.String(), stderr.String())
			}
			refused := strings.Contains(stderr.String(), "cross-city-fence refused bead="+convoy.ID)
			if refused == tc.wantClosed {
				t.Fatalf("refusal logged = %v, want %v; stderr=%q", refused, !tc.wantClosed, stderr.String())
			}
		})
	}
}

// The hidden `gc convoy autoclose <id>` hook entry resolves the owning store
// and its gate through autocloseOwningStore.
func TestConvoyAutocloseHookHonorsFederationIdentity(t *testing.T) {
	for _, tc := range []struct {
		city       string
		wantClosed bool
	}{{"citadel", true}, {"jadegate", false}} {
		t.Run("city="+tc.city, func(t *testing.T) {
			cityPath := writeConvoyTestCityWithFederation(t, tc.city)
			t.Setenv("GC_STORE_ROOT", cityPath)
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

			var stdout, stderr bytes.Buffer
			doConvoyAutoclose(child.ID, &stdout, &stderr)

			got, err := store.Get(convoy.ID)
			if err != nil {
				t.Fatal(err)
			}
			if closed := got.Status == "closed"; closed != tc.wantClosed {
				t.Fatalf("convoy status = %q, want closed=%v; stdout=%q stderr=%q", got.Status, tc.wantClosed, stdout.String(), stderr.String())
			}
		})
	}
}
