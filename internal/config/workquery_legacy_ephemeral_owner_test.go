package config

import (
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/federation"
)

// Regression coverage for gf-pu2 finding 1e (the gf-jr0 vs-v1.5.0 gate's
// Major at internal/config/workquery.go:444): the legacy ephemeral tier of the
// pool-demand probe — `bd query 'ephemeral=true AND status=open'` filtered by
// jq, the read a bd-1.0.4-compatible city takes for wisps routed to a pool —
// sorted its candidates oldest-first and cut them to a window BEFORE any
// ownership rule, and then cut again to one row. Round 1's row-4 ordering
// (claimableFirstJQClause) covered the two `bd ready` tiers only. On a
// federated city an older bead owned by another city therefore led the
// window, the Work query served it alone, the hook's claim fence refused it,
// the worker drained, and the eligible local wisp behind it was never served
// — while the count-form (fenced since finding 1c) read 1 and kept minting
// seats for it. The ownership predicate now gates the legacy ephemeral rows
// before the sort and both cuts.

// legacyEphemeralRow renders one `bd query --json` row of an open, unassigned,
// pool-routed wisp with the given creation time and labels.
func legacyEphemeralRow(id, createdAt, target string, labels ...string) string {
	quoted := make([]string, 0, len(labels))
	for _, l := range labels {
		quoted = append(quoted, strconv.Quote(l))
	}
	return `{"id":` + strconv.Quote(id) + `,"status":"open","issue_type":"task","ephemeral":true,"assignee":"",` +
		`"created_at":` + strconv.Quote(createdAt) + `,"dependency_count":0,"labels":[` + strings.Join(quoted, ",") + `],` +
		`"metadata":{"gc.routed_to":` + strconv.Quote(target) + `}}`
}

// fakeBdLegacyEphemeralRows is a fake bd whose `bd ready` tiers are empty, so
// the probe falls through to the legacy ephemeral tier, where `bd query`
// answers rows.
func fakeBdLegacyEphemeralRows(rows ...string) string {
	return `#!/bin/sh
case "$1" in
  query) printf '%s' '[` + strings.Join(rows, ",") + `]' ;;
  *) printf '[]' ;;
esac
`
}

// TestLegacyEphemeralWorkServesLocalWorkBehindAnOlderForeignBead EXECUTES the
// generated Work query and the count-form against a fake bd whose only ready
// candidates are two legacy ephemeral wisps: an older one owned by another
// city and a newer one owned by this city. Without an identity the older
// foreign row is served (the pre-fix behavior, and what a non-federated city
// still gets); with the identity the local row is served and counted, and a
// foreign row alone is neither served nor counted — consistent with the
// fenced count-form of finding 1c.
func TestLegacyEphemeralWorkServesLocalWorkBehindAnOlderForeignBead(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	a := Agent{Name: "worker", Dir: "hello-world"}
	target := a.poolDemandTarget()
	foreign, _ := federation.OwnerLabel("jadegate")
	own, _ := federation.OwnerLabel("citadel")
	foreignOlder := legacyEphemeralRow("hw-wisp-foreign", "2026-01-01T00:00:00Z", target, foreign)
	ownNewer := legacyEphemeralRow("hw-wisp-own", "2026-02-01T00:00:00Z", target, own)

	work := func(topo QueryTopology, rows ...string) []string {
		t.Helper()
		out := strings.TrimSpace(runShellWithFakeBd(t, a.EffectiveWorkQueryFor(topo), nil, fakeBdLegacyEphemeralRows(rows...)))
		var got []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("work query output is not a JSON array: %v (output %q)", err, out)
		}
		ids := make([]string, 0, len(got))
		for _, g := range got {
			ids = append(ids, g.ID)
		}
		return ids
	}
	count := func(topo QueryTopology, rows ...string) string {
		t.Helper()
		return strings.TrimSpace(runShellWithFakeBd(t, a.EffectivePoolDemandQueryFor(topo), nil, fakeBdLegacyEphemeralRows(rows...)))
	}
	federated := QueryTopology{FederationIdentity: "citadel"}

	if got := work(QueryTopology{}, foreignOlder, ownNewer); len(got) != 1 || got[0] != "hw-wisp-foreign" {
		t.Fatalf("without an identity the oldest row is served as before; got %v", got)
	}
	if got := work(federated, foreignOlder, ownNewer); len(got) != 1 || got[0] != "hw-wisp-own" {
		t.Fatalf("with the identity the local wisp behind an older foreign one must be served; got %v", got)
	}
	if got := count(federated, foreignOlder, ownNewer); got != "1" {
		t.Fatalf("count with one foreign and one own legacy ephemeral wisp = %q, want 1", got)
	}
	if got := work(federated, foreignOlder); len(got) != 0 {
		t.Fatalf("a foreign-owned wisp alone must yield no work; got %v", got)
	}
	if got := count(federated, foreignOlder); got != "0" {
		t.Fatalf("count with a foreign-owned wisp alone = %q, want 0", got)
	}
	if got := work(federated, legacyEphemeralRow("hw-wisp-handoff", "2026-01-01T00:00:00Z", target, foreign, federation.HandoffLabel("citadel"))); len(got) != 1 || got[0] != "hw-wisp-handoff" {
		t.Fatalf("a foreign wisp handed off to this city must be served; got %v", got)
	}
}

// TestLegacyEphemeralPoolDemandShellIsByteIdenticalWithoutIdentity is the
// byte-identity claim for the legacy ephemeral tier: a non-federated city
// emits exactly the read it emits today, and an identity reaches the tier's
// jq before its sort and cut.
func TestLegacyEphemeralPoolDemandShellIsByteIdenticalWithoutIdentity(t *testing.T) {
	if legacyEphemeralPoolDemandShell(20, QueryTopology{}, true) != legacyEphemeralPoolDemandShell(20, QueryTopology{FederationIdentity: ""}, true) {
		t.Fatal("an empty identity changed the generated legacy ephemeral read")
	}
	fenced := legacyEphemeralPoolDemandShell(20, QueryTopology{FederationIdentity: "citadel"}, true)
	predicate := strings.Index(fenced, `handoff:citadel`)
	sortAt := strings.Index(fenced, `sort_by(`)
	if predicate < 0 || sortAt < 0 || predicate > sortAt {
		t.Fatalf("the ownership predicate must run before the legacy ephemeral sort and cut: %q", fenced)
	}
}
