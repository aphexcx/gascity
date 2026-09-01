package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestOwnerBackfillRowsBucketsByTheOneRuleWeTrust is the spec for the
// legacy backfill's derivation: a bead is OURS only when its assignee carries
// this city's session prefix or its pool: label resolves in this city; every
// other unlabeled open bead stays THEIRS-OR-UNKNOWN (convention item 3: legacy
// unlabeled work is claimable by anyone). Labeled and closed beads are not
// candidates at all.
func TestOwnerBackfillRowsBucketsByTheOneRuleWeTrust(t *testing.T) {
	resolves := func(target string) bool { return target == "houmanoids-www/gc.coder" }
	tests := []struct {
		name       string
		bead       beads.Bead
		wantBucket string // "" means not a candidate
		wantRule   string // substring of the rule
	}{
		{"session id with this city's prefix", beads.Bead{ID: "hw-1", Status: "open", Assignee: "ci-abc12"}, ownerBackfillOurs, `session prefix "ci-"`},
		{"in-progress session id with this city's prefix", beads.Bead{ID: "hw-2", Status: "in_progress", Assignee: "ci-wisp-abc"}, ownerBackfillOurs, `session prefix "ci-"`},
		{"session id with another city's prefix", beads.Bead{ID: "hw-3", Status: "open", Assignee: "jg-xyz"}, ownerBackfillUnknown, "no attributable signal"},
		{"a longer prefix does not match", beads.Bead{ID: "hw-4", Status: "open", Assignee: "cix-abc"}, ownerBackfillUnknown, "no attributable signal"},
		{"unassigned and unlabeled", beads.Bead{ID: "hw-5", Status: "open"}, ownerBackfillUnknown, "no attributable signal"},
		{"a rig/agent assignee alone is not attributable", beads.Bead{ID: "hw-6", Status: "open", Assignee: "houmanoids-www/gc.coder"}, ownerBackfillUnknown, "no attributable signal"},
		{"a pool label that resolves here", beads.Bead{ID: "hw-7", Status: "open", Labels: []string{"pool:houmanoids-www/gc.coder"}}, ownerBackfillOurs, `pool label "pool:houmanoids-www/gc.coder" resolves`},
		{"a pool label that does not resolve here", beads.Bead{ID: "hw-8", Status: "open", Labels: []string{"pool:elsewhere/gc.coder"}}, ownerBackfillUnknown, "no attributable signal"},
		{"already owned by another city", beads.Bead{ID: "hw-9", Status: "open", Assignee: "ci-abc", Labels: []string{"owner:jadegate"}}, "", ""},
		{"already owned by this city", beads.Bead{ID: "hw-10", Status: "open", Assignee: "ci-abc", Labels: []string{"owner:citadel"}}, "", ""},
		{"closed beads are never touched", beads.Bead{ID: "hw-11", Status: "closed", Assignee: "ci-abc"}, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := ownerBackfillRows([]beads.Bead{tt.bead}, "ci", resolves)
			if tt.wantBucket == "" {
				if len(rows) != 0 {
					t.Fatalf("rows = %+v, want none (not a candidate)", rows)
				}
				return
			}
			if len(rows) != 1 {
				t.Fatalf("rows = %+v, want exactly one", rows)
			}
			if rows[0].Bucket != tt.wantBucket {
				t.Fatalf("bucket = %q, want %q (rule %q)", rows[0].Bucket, tt.wantBucket, rows[0].Rule)
			}
			if !strings.Contains(rows[0].Rule, tt.wantRule) {
				t.Fatalf("rule = %q, want it to mention %q", rows[0].Rule, tt.wantRule)
			}
			if rows[0].Bead.ID != tt.bead.ID {
				t.Fatalf("row bead = %q, want %q", rows[0].Bead.ID, tt.bead.ID)
			}
		})
	}
}

func TestOwnerBackfillRowsKeepStoreOrder(t *testing.T) {
	items := []beads.Bead{
		{ID: "hw-b", Status: "open", Assignee: "jg-1"},
		{ID: "hw-a", Status: "open", Assignee: "ci-1"},
	}
	rows := ownerBackfillRows(items, "ci", func(string) bool { return false })
	if len(rows) != 2 || rows[0].Bead.ID != "hw-b" || rows[1].Bead.ID != "hw-a" {
		t.Fatalf("rows = %+v, want the store's order preserved", rows)
	}
}

// TestMaintenanceOwnerBackfillScript pins the command's output shape and its
// write discipline end to end: dry run by default, --apply labels only OURS,
// idempotent on a second run, refused without [federation] identity.
func TestMaintenanceOwnerBackfillScript(t *testing.T) {
	testscript.Run(t, newTestscriptParams(t, filepath.Join("testdata", "maintenance-owner-backfill.txtar")))
}

// staleReadStore serves Get from a substitute bead, standing in for a bead
// that changed between the backfill's list and its write. It exposes the
// inner store's conditional writer the way the policy wrapper does, so the
// write path under test is the fenced one.
type staleReadStore struct {
	beads.Store
	fresh map[string]beads.Bead
}

func (s *staleReadStore) Get(id string) (beads.Bead, error) {
	if b, ok := s.fresh[id]; ok {
		return b, nil
	}
	return s.Store.Get(id)
}

func (s *staleReadStore) ConditionalWritesResolveTarget() beads.Store { return s.Store }

// TestApplyOwnerBackfillReChecksEveryBeadBeforeWriting: --apply must not
// trust the dry-run snapshot. A bead that was closed, given an owner, or lost
// its OURS signal since the list is skipped, and only a bead that still
// classifies OURS on a live read is labeled.
func TestApplyOwnerBackfillReChecksEveryBeadBeforeWriting(t *testing.T) {
	mem := beads.NewMemStore()
	mk := func(title, assignee string, labels ...string) beads.Bead {
		b, err := mem.Create(beads.Bead{Title: title, Assignee: assignee, Labels: labels})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	still := mk("still ours", "ci-1")
	closed := mk("closed since", "ci-2")
	owned := mk("owned since", "ci-3")
	moved := mk("reassigned since", "ci-4")
	rows := ownerBackfillRows([]beads.Bead{still, closed, owned, moved}, "ci", nil)
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4 OURS candidates", len(rows))
	}
	closedNow := closed
	closedNow.Status = "closed"
	ownedNow := owned
	ownedNow.Labels = []string{"owner:jadegate"}
	movedNow := moved
	movedNow.Assignee = "jg-9"
	store := &staleReadStore{Store: mem, fresh: map[string]beads.Bead{closed.ID: closedNow, owned.ID: ownedNow, moved.ID: movedNow}}

	var stdout, stderr bytes.Buffer
	labeled, skipped, unfenced, failed := applyOwnerBackfill(store, rows, "owner:citadel", "ci", nil, &stdout, &stderr)
	if labeled != 1 || skipped != 3 || unfenced != 0 || failed != 0 {
		t.Fatalf("labeled/skipped/unfenced/failed = %d/%d/%d/%d, want 1/3/0/0; stdout=%q stderr=%q", labeled, skipped, unfenced, failed, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "labeled "+still.ID+" owner:citadel") {
		t.Fatalf("stdout = %q, want the one labeled line", stdout.String())
	}
	for _, id := range []string{closed.ID, owned.ID, moved.ID} {
		if !strings.Contains(stdout.String(), "skipped "+id) {
			t.Errorf("stdout = %q, want a skipped line for %s", stdout.String(), id)
		}
		got, err := mem.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if beadLabelsContain(got.Labels, "owner:citadel") {
			t.Errorf("%s was labeled despite changing under the backfill: %v", id, got.Labels)
		}
	}
	got, err := mem.Get(still.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !beadLabelsContain(got.Labels, "owner:citadel") {
		t.Fatalf("%s labels = %v, want owner:citadel", still.ID, got.Labels)
	}
}

// TestApplyOwnerBackfillWritesThroughThePolicyWrapperFence: every store gc
// opens is policy-wrapped, and the wrapper embeds the Store interface, so the
// conditional writer is not promoted through it. The backfill must resolve
// the fence through the wrapper's resolve target, or production never fences.
func TestApplyOwnerBackfillWritesThroughThePolicyWrapperFence(t *testing.T) {
	mem := beads.NewMemStore()
	b, err := mem.Create(beads.Bead{Title: "ours", Assignee: "ci-1"})
	if err != nil {
		t.Fatal(err)
	}
	store := wrapStoreWithBeadPolicies(mem, federatedTestConfig("citadel"))
	rows := ownerBackfillRows([]beads.Bead{b}, "ci", nil)
	var stdout, stderr bytes.Buffer
	labeled, skipped, unfenced, failed := applyOwnerBackfill(store, rows, "owner:citadel", "ci", nil, &stdout, &stderr)
	if labeled != 1 || skipped != 0 || unfenced != 0 || failed != 0 {
		t.Fatalf("labeled/skipped/unfenced/failed = %d/%d/%d/%d, want 1/0/0/0; stdout=%q stderr=%q", labeled, skipped, unfenced, failed, stdout.String(), stderr.String())
	}
	got, err := mem.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !beadLabelsContain(got.Labels, "owner:citadel") {
		t.Fatalf("labels = %v, want owner:citadel", got.Labels)
	}
}

// TestApplyOwnerBackfillSkipsABeadThatMovedBetweenReadAndWrite: the fence is
// the revision the re-read returned; a bead that moved after it is skipped.
func TestApplyOwnerBackfillSkipsABeadThatMovedBetweenReadAndWrite(t *testing.T) {
	mem := beads.NewMemStore()
	b, err := mem.Create(beads.Bead{Title: "ours", Assignee: "ci-1"})
	if err != nil {
		t.Fatal(err)
	}
	// The store moves on after the re-read: a title edit bumps the revision,
	// while the stale reader keeps serving the version the backfill saw.
	retitled := "ours, edited meanwhile"
	if err := mem.Update(b.ID, beads.UpdateOpts{Title: &retitled}); err != nil {
		t.Fatal(err)
	}
	if b.Revision <= 0 {
		t.Fatalf("MemStore.Create returned revision %d, want a positive one for this fixture", b.Revision)
	}
	store := &staleReadStore{Store: mem, fresh: map[string]beads.Bead{b.ID: b}}
	rows := ownerBackfillRows([]beads.Bead{b}, "ci", nil)
	var stdout, stderr bytes.Buffer
	labeled, skipped, unfenced, failed := applyOwnerBackfill(store, rows, "owner:citadel", "ci", nil, &stdout, &stderr)
	if labeled != 0 || skipped != 1 || unfenced != 0 || failed != 0 {
		t.Fatalf("labeled/skipped/unfenced/failed = %d/%d/%d/%d, want 0/1/0/0; stdout=%q stderr=%q", labeled, skipped, unfenced, failed, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "skipped "+b.ID+": changed while labeling") {
		t.Fatalf("stdout = %q, want the changed-while-labeling skip", stdout.String())
	}
	got, err := mem.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if beadLabelsContain(got.Labels, "owner:citadel") {
		t.Fatalf("labels = %v, want no write on a moved revision", got.Labels)
	}
}

// TestApplyOwnerBackfillRefusesAnUnfencedWrite: a store that cannot prove a
// revision or fence the write gets no write at all — the backfill is
// fail-closed, never a blind add.
func TestApplyOwnerBackfillRefusesAnUnfencedWrite(t *testing.T) {
	mem := beads.NewMemStore()
	mem.DisableConditionalWrites = true
	b, err := mem.Create(beads.Bead{Title: "ours", Assignee: "ci-1"})
	if err != nil {
		t.Fatal(err)
	}
	rows := ownerBackfillRows([]beads.Bead{b}, "ci", nil)
	var stdout, stderr bytes.Buffer
	labeled, skipped, unfenced, failed := applyOwnerBackfill(mem, rows, "owner:citadel", "ci", nil, &stdout, &stderr)
	if labeled != 0 || skipped != 0 || unfenced != 1 || failed != 0 {
		t.Fatalf("labeled/skipped/unfenced/failed = %d/%d/%d/%d, want 0/0/1/0; stdout=%q stderr=%q", labeled, skipped, unfenced, failed, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "unfenced") || !strings.Contains(stderr.String(), b.ID) {
		t.Fatalf("stderr = %q, want the bead named as unfenced", stderr.String())
	}
	got, err := mem.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if beadLabelsContain(got.Labels, "owner:citadel") {
		t.Fatalf("labels = %v, want no write without a fence", got.Labels)
	}
}
