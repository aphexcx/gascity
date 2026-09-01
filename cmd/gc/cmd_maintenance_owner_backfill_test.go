package main

import (
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
