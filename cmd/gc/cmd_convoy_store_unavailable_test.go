package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// errConvoyStoreDark is the shape a bd-backed candidate store reports when
// its scope's database refuses to open: a multi-line diagnostic that is NOT
// beads.ErrNotFound. This is what a PROJECT IDENTITY MISMATCH on one rig
// store looks like to the convoy commands.
var errConvoyStoreDark = errors.New("failed to open database: PROJECT IDENTITY MISMATCH — refusing to connect\n\n  Local project ID (metadata.json):  aaaa\n  Database project ID:               bbbb")

// convoyDarkStore is a convoy store candidate that has gone dark: every read
// fails with a non-not-found error. It stands in for a sibling rig store
// whose database is unavailable while the store the caller actually needs
// is healthy.
type convoyDarkStore struct{ beads.Store }

func newConvoyDarkStore() convoyDarkStore { return convoyDarkStore{Store: beads.NewMemStore()} }

func (convoyDarkStore) Get(string) (beads.Bead, error) { return beads.Bead{}, errConvoyStoreDark }

func (convoyDarkStore) List(beads.ListQuery) ([]beads.Bead, error) { return nil, errConvoyStoreDark }

func convoyDarkTestCity() *config.City {
	return &config.City{
		Rigs: []config.Rig{{
			Name:   "hello-world",
			Path:   "/rigs/hello-world",
			Prefix: "HW",
		}},
	}
}

func convoyDarkOpenStore(t *testing.T, cityStore, rigStore beads.Store) func(string) (beads.Store, error) {
	t.Helper()
	return func(dir string) (beads.Store, error) {
		switch dir {
		case "/city":
			return cityStore, nil
		case "/rigs/hello-world":
			return rigStore, nil
		default:
			t.Fatalf("unexpected store dir %q", dir)
			return nil, nil
		}
	}
}

// A convoy that lives in the healthy city store must still resolve when a
// sibling rig store cannot be read: the dark store cannot hold the bead's
// answer, so it must not veto the store that does. This is the
// `gc convoy status <city-id>` failure seen when one rig's dolt database
// refuses to open.
func TestResolveConvoyStoreSurvivesDarkSiblingStore(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityStore := beads.NewMemStore()
	convoy, _ := cityStore.Create(beads.Bead{Title: "deploy", Type: "convoy"})

	store, err := resolveConvoyStore(convoy.ID, convoyDarkTestCity(), "/city", convoyDarkOpenStore(t, cityStore, newConvoyDarkStore()))
	if err != nil {
		t.Fatalf("resolveConvoyStore: %v", err)
	}
	if store != cityStore {
		t.Fatalf("resolveConvoyStore returned wrong store")
	}
}

// When no healthy store holds the bead and a candidate was dark, the result
// is NOT "not found": the dark store may own it. The error must name the
// store that could not be consulted and carry the underlying cause.
func TestResolveConvoyStoreReportsDarkStoreWhenUnfound(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityStore := beads.NewMemStore()

	_, err := resolveConvoyStore("gc-404", convoyDarkTestCity(), "/city", convoyDarkOpenStore(t, cityStore, newConvoyDarkStore()))
	if err == nil {
		t.Fatal("resolveConvoyStore = nil error, want failure naming the dark store")
	}
	if errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("resolveConvoyStore = %v, must not claim not-found when a candidate was dark", err)
	}
	if !errors.Is(err, errConvoyStoreDark) {
		t.Fatalf("resolveConvoyStore = %v, want it to wrap the dark store's error", err)
	}
	if !strings.Contains(err.Error(), "/rigs/hello-world") {
		t.Fatalf("resolveConvoyStore = %v, want the dark store path", err)
	}
}

// Contract guard: a bead present in two healthy stores is still ambiguous.
// Tolerating dark stores must not weaken the uniquely-addressable rule.
func TestResolveConvoyStoreStillRejectsAmbiguity(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	_, _ = cityStore.Create(beads.Bead{Title: "deploy", Type: "convoy"}) // gc-1
	_, _ = rigStore.Create(beads.Bead{Title: "deploy", Type: "convoy"})  // gc-1

	_, err := resolveConvoyStore("gc-1", convoyDarkTestCity(), "/city", convoyDarkOpenStore(t, cityStore, rigStore))
	if err == nil || !strings.Contains(err.Error(), "multiple stores") {
		t.Fatalf("resolveConvoyStore = %v, want ambiguity error", err)
	}
}

// `gc convoy list` spans every store; one dark store must cost the listing
// only that store's rows, reported on stderr, not the whole command.
func TestConvoyListAcrossStoresSkipsDarkStore(t *testing.T) {
	cityStore := beads.NewMemStore()
	_, _ = cityStore.Create(beads.Bead{Title: "city batch", Type: "convoy"}) // gc-1
	_, _ = cityStore.Create(beads.Bead{Title: "city task", ParentID: "gc-1"})

	var stdout, stderr bytes.Buffer
	code := doConvoyListAcrossStores([]convoyStoreView{
		{path: "/city", store: cityStore},
		{path: "/rigs/hello-world", store: newConvoyDarkStore()},
	}, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyListAcrossStores = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "city batch") {
		t.Errorf("stdout missing the healthy store's convoy:\n%s", stdout.String())
	}
	warn := stderr.String()
	if !strings.Contains(warn, "warning") || !strings.Contains(warn, "/rigs/hello-world") {
		t.Errorf("stderr = %q, want a warning naming the dark store", warn)
	}
	if !strings.Contains(warn, "PROJECT IDENTITY MISMATCH") {
		t.Errorf("stderr = %q, want the dark store's first diagnostic line", warn)
	}
	if strings.Contains(warn, "Local project ID") {
		t.Errorf("stderr = %q, want only the first line of the multi-line diagnostic", warn)
	}
}

// The JSON form keeps stdout a single clean JSONL record; the skip warning
// goes to stderr.
func TestConvoyListJSONSkipsDarkStore(t *testing.T) {
	cityStore := beads.NewMemStore()
	_, _ = cityStore.Create(beads.Bead{Title: "city batch", Type: "convoy"}) // gc-1

	var stdout, stderr bytes.Buffer
	code := doConvoyListAcrossStores([]convoyStoreView{
		{path: "/city", store: cityStore},
		{path: "/rigs/hello-world", store: newConvoyDarkStore()},
	}, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyListAcrossStores --json = %d, want 0; stderr: %s", code, stderr.String())
	}
	var result convoyListResultJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if result.Summary.Total != 1 || len(result.Convoys) != 1 || result.Convoys[0].ID != "gc-1" {
		t.Fatalf("result = %+v, want exactly the healthy store's convoy", result)
	}
	if !strings.Contains(stderr.String(), "/rigs/hello-world") {
		t.Errorf("stderr = %q, want a warning naming the dark store", stderr.String())
	}
}

// When every store is dark there is nothing to list and the failure is the
// real story: exit non-zero with the diagnostic, as before.
func TestConvoyListAcrossStoresFailsWhenEveryStoreDark(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := doConvoyListAcrossStores([]convoyStoreView{
		{path: "/city", store: newConvoyDarkStore()},
		{path: "/rigs/hello-world", store: newConvoyDarkStore()},
	}, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doConvoyListAcrossStores = %d, want 1; stdout: %s", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "PROJECT IDENTITY MISMATCH") {
		t.Errorf("stderr = %q, want the store diagnostic", stderr.String())
	}
}

// `gc convoy check` auto-closes what it can see; a dark sibling store must
// not stop it from closing a fully-finished convoy in a healthy store.
func TestConvoyCheckAcrossStoresSkipsDarkStore(t *testing.T) {
	cityStore := beads.NewMemStore()
	_, _ = cityStore.Create(beads.Bead{Title: "city batch", Type: "convoy"})  // gc-1
	_, _ = cityStore.Create(beads.Bead{Title: "done task", ParentID: "gc-1"}) // gc-2
	_ = cityStore.Close("gc-2")

	var stdout, stderr bytes.Buffer
	code := doConvoyCheckAcrossStores([]convoyStoreView{
		{path: "/city", store: cityStore},
		{path: "/rigs/hello-world", store: newConvoyDarkStore()},
	}, events.Discard, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyCheckAcrossStores = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Auto-closed convoy gc-1") {
		t.Errorf("stdout = %q, want the healthy convoy auto-closed", stdout.String())
	}
	got, err := cityStore.Get("gc-1")
	if err != nil || got.Status != "closed" {
		t.Fatalf("gc-1 after check = %+v, %v; want closed", got, err)
	}
	if !strings.Contains(stderr.String(), "/rigs/hello-world") {
		t.Errorf("stderr = %q, want a warning naming the dark store", stderr.String())
	}
}

// `gc convoy stranded` shares the multi-store collector and gets the same
// degrade-not-fail behavior.
func TestConvoyStrandedAcrossStoresSkipsDarkStore(t *testing.T) {
	cityStore := beads.NewMemStore()
	_, _ = cityStore.Create(beads.Bead{Title: "city batch", Type: "convoy"}) // gc-1
	_, _ = cityStore.Create(beads.Bead{Title: "unassigned task", ParentID: "gc-1"})

	var stdout, stderr bytes.Buffer
	code := doConvoyStrandedAcrossStores([]convoyStoreView{
		{path: "/city", store: cityStore},
		{path: "/rigs/hello-world", store: newConvoyDarkStore()},
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doConvoyStrandedAcrossStores = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "unassigned task") {
		t.Errorf("stdout missing the healthy store's stranded item:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "/rigs/hello-world") {
		t.Errorf("stderr = %q, want a warning naming the dark store", stderr.String())
	}
}

// A by-id READ (convoy status) degrades: the healthy store answers and the
// dark sibling comes back as a skipped candidate for the caller to warn about.
func TestResolveConvoyStoreForCommandReadDegradesUnderDarkSibling(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityStore := beads.NewMemStore()
	convoy, _ := cityStore.Create(beads.Bead{Title: "deploy", Type: "convoy"})

	store, skipped, err := resolveConvoyStoreForCommand(convoy.ID, convoyDarkTestCity(), "/city", convoyReadDegraded, convoyDarkOpenStore(t, cityStore, newConvoyDarkStore()))
	if err != nil {
		t.Fatalf("read resolution: %v", err)
	}
	if store != cityStore {
		t.Fatal("read resolution returned wrong store")
	}
	if len(skipped) != 1 || skipped[0].path != "/rigs/hello-world" || !errors.Is(skipped[0].err, errConvoyStoreDark) {
		t.Fatalf("skipped = %+v, want the dark rig store with its error", skipped)
	}
}

// A by-id MUTATION (convoy target/add/close/land) must not act under partial
// visibility: the bead may also live in the dark store, so unique ownership
// cannot be proven. The refusal names the dark store and wraps its error, and
// hands back no store to mutate.
func TestResolveConvoyStoreForCommandMutationRefusesUnderDarkSibling(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityStore := beads.NewMemStore()
	convoy, _ := cityStore.Create(beads.Bead{Title: "deploy", Type: "convoy"})

	store, skipped, err := resolveConvoyStoreForCommand(convoy.ID, convoyDarkTestCity(), "/city", convoyMutationStrict, convoyDarkOpenStore(t, cityStore, newConvoyDarkStore()))
	if err == nil {
		t.Fatal("mutation resolution = nil error, want refusal under partial visibility")
	}
	if store != nil || skipped != nil {
		t.Fatalf("mutation resolution returned store=%v skipped=%v, want nothing to act on", store, skipped)
	}
	if errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("mutation resolution = %v, must not claim not-found", err)
	}
	if !errors.Is(err, errConvoyStoreDark) || !strings.Contains(err.Error(), "/rigs/hello-world") {
		t.Fatalf("mutation resolution = %v, want it to name the dark store and wrap its error", err)
	}
	if !strings.Contains(err.Error(), "unique ownership") {
		t.Fatalf("mutation resolution = %v, want it to say why it refused", err)
	}
}

// Strict is not over-strict: with every store readable a mutation resolves
// exactly as before.
func TestResolveConvoyStoreForCommandMutationResolvesWhenAllStoresReadable(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityStore := beads.NewMemStore()
	convoy, _ := cityStore.Create(beads.Bead{Title: "deploy", Type: "convoy"})

	store, skipped, err := resolveConvoyStoreForCommand(convoy.ID, convoyDarkTestCity(), "/city", convoyMutationStrict, convoyDarkOpenStore(t, cityStore, beads.NewMemStore()))
	if err != nil || store != cityStore || len(skipped) != 0 {
		t.Fatalf("mutation resolution = (%v, %v, %v), want the city store, no skips, no error", store, skipped, err)
	}
}

// The bd-hook autoclose resolver acts on what it resolves, so it takes the
// strict rule: a dark sibling is a refusal, and the caller falls back rather
// than closing a copy whose ownership is unprovable.
func TestResolveOwningStoreDirRefusesUnderDarkSibling(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	cityStore := beads.NewMemStore()
	convoy, _ := cityStore.Create(beads.Bead{Title: "deploy", Type: "convoy"})

	store, dir, err := resolveOwningStoreDir(convoy.ID, convoyDarkTestCity(), "/city", convoyDarkOpenStore(t, cityStore, newConvoyDarkStore()))
	if err == nil {
		t.Fatal("resolveOwningStoreDir = nil error, want refusal under partial visibility")
	}
	if store != nil || dir != "" {
		t.Fatalf("resolveOwningStoreDir returned store=%v dir=%q, want nothing to act on", store, dir)
	}
	if !errors.Is(err, errConvoyStoreDark) || errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("resolveOwningStoreDir = %v, want the dark store's error and not not-found", err)
	}
}
