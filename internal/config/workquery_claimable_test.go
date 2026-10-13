package config

import (
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/federation"
)

// Regression coverage for gf-4c9 row 4 (the gf-9x2 codex gate's Major at
// cmd/gc/cmd_hook_claim.go:501) and gf-xid finding 1h (the gf-pu2 re-gates'
// Major at internal/config/workquery.go:294 and :319): on a federated city the
// hook's cross-city fence (federation.MayClaim on the decoded rows) ran AFTER
// the routed pool-demand tier had cut its 20-row window. When that window was
// filled by another city's rows the hook reported no work and drained, every
// retry saw the same foreign rows, and eligible local work behind them
// starved. Round 1 ordered claimable rows ahead of the rest before the cut
// but KEPT the foreign rows, and probe_pool_demand ends on the first nonempty
// tier — so a tier whose only rows were foreign still ended the probe, the
// hook refused every row, and local work in a later tier (a legacy wisp
// behind a foreign routed task) was never reached while the fenced count kept
// minting seats for it. The generated read now FILTERS foreign rows out of
// every tier before the cut (claimableOnlyJQClause); the Go fence still
// judges every row served.

// claimableRow renders one routed ready row with the given labels.
func claimableRow(id string, labels ...string) string {
	quoted := make([]string, 0, len(labels))
	for _, l := range labels {
		quoted = append(quoted, strconv.Quote(l))
	}
	return `{"id":` + strconv.Quote(id) + `,"status":"open","issue_type":"task","assignee":"","labels":[` +
		strings.Join(quoted, ",") + `],"metadata":{"gc.routed_to":"claude"}}`
}

// fakeBdReadyRows is a fake bd whose `bd ready` prints rows regardless of
// flags; every other verb prints an empty array.
func fakeBdReadyRows(rows ...string) string {
	return `#!/bin/sh
case "$1" in
  ready) printf '%s' '[` + strings.Join(rows, ",") + `]' ;;
  *) printf '[]' ;;
esac
`
}

// TestRoutedPoolDemandWindowKeepsOnlyClaimableRows EXECUTES the routed
// pool-demand read against a fake bd that answers with twenty-five rows owned
// by another city followed by one row this city owns. Without an identity the
// window is the first twenty rows (all foreign: what the hook used to be
// handed and then fence to nothing); with the city's identity the ownership
// filter runs before the cut and the window holds the own row alone — no
// foreign row is served, so a tier of foreign rows is an EMPTY tier and the
// probe falls through to the next one.
func TestRoutedPoolDemandWindowKeepsOnlyClaimableRows(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	foreignOwner, _ := federation.OwnerLabel("boomtown")
	ownOwner, _ := federation.OwnerLabel("citadel")
	var rows []string
	for i := 1; i <= 25; i++ {
		rows = append(rows, claimableRow("hw-foreign-"+strconv.Itoa(i), foreignOwner))
	}
	rows = append(rows, claimableRow("hw-own-1", ownOwner))
	fakeBd := fakeBdReadyRows(rows...)

	run := func(topo QueryTopology) []map[string]any {
		t.Helper()
		script := `target="$1"; ` + bdReadyPoolDemandShell(20, topo)
		out := runShellWithFakeBd(t, "sh -c "+shellQuoteForTest(script)+" -- claude", nil, fakeBd)
		var got []map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
			t.Fatalf("pool-demand output is not a JSON array: %v (output %q)", err, out)
		}
		return got
	}

	unfenced := run(QueryTopology{})
	if len(unfenced) != 20 || unfenced[0]["id"] != "hw-foreign-1" {
		t.Fatalf("without an identity the window must be the first 20 rows as before; got %d rows, first %v", len(unfenced), unfenced[0])
	}

	fenced := run(QueryTopology{FederationIdentity: "citadel"})
	if len(fenced) != 1 || fenced[0]["id"] != "hw-own-1" {
		t.Fatalf("with the identity the window must hold the own row alone, every foreign row filtered before the cut; got %d rows: %v", len(fenced), fenced)
	}
	foreignOnly := fakeBdReadyRows(rows[:25]...)
	out := strings.TrimSpace(runShellWithFakeBd(t, "sh -c "+shellQuoteForTest(`target="$1"; `+bdReadyPoolDemandShell(20, QueryTopology{FederationIdentity: "citadel"}))+" -- claude", nil, foreignOnly))
	if out != "[]" {
		t.Fatalf("a tier of foreign rows alone must read as empty so the probe falls through; got %q", out)
	}
}

// TestClaimablePredicateJQAgreesWithMayClaim pins the predicate to the Go
// rule it mirrors, label set by label set, by running the generated jq
// against each row and comparing with federation.MayClaim. A drift between
// the two would demote a claimable row behind foreign ones (the starvation
// the ordering exists to end) or promote a foreign one (which the Go fence
// still refuses).
func TestClaimablePredicateJQAgreesWithMayClaim(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	const identity = "citadel"
	own, _ := federation.OwnerLabel(identity)
	foreign, _ := federation.OwnerLabel("boomtown")
	handoff := federation.HandoffLabel(identity)
	cases := [][]string{
		nil,
		{"mysql-cutover"},
		{own},
		{own, "mysql-cutover"},
		{foreign},
		{foreign, handoff},
		{foreign, federation.HandoffLabel("jadegate")},
		{own, foreign},
		{own, foreign, handoff},
		{"owner:"}, // malformed bare owner: refused by both
		{"owner:citadel-two"},
		{"handoff:citadel"}, // a handoff with no owner is simply unowned
		{"Owner:citadel"},   // labels are exact and lowercase; this is not an owner label
	}
	filter := `[.[] | select(` + claimablePredicateJQ(identity) + `)] | length`
	for _, labels := range cases {
		row := claimableRow("x", labels...)
		out := runShellWithFakeBd(t, "sh -c "+shellQuoteForTest(`printf '%s' '[`+row+`]' | jq `+shellQuoteForTest(filter)), nil, fakeBdReadyRows())
		kept := strings.TrimSpace(out) == "1"
		want, _ := federation.MayClaim(labels, identity)
		if kept != want {
			t.Errorf("labels %v: jq kept=%v, federation.MayClaim=%v (output %q)", labels, kept, want, out)
		}
	}
}

// TestClaimableOnlyJQClauseIsEmptyWithoutIdentity is the byte-identity
// claim: a non-federated city emits exactly the read it emits today.
func TestClaimableOnlyJQClauseIsEmptyWithoutIdentity(t *testing.T) {
	if got := claimableOnlyJQClause(""); got != "" {
		t.Fatalf("claimableOnlyJQClause(\"\") = %q, want empty", got)
	}
	if got := claimableOnlyJQClause("  "); got != "" {
		t.Fatalf("claimableOnlyJQClause(blank) = %q, want empty", got)
	}
	if bdReadyPoolDemandShell(20, QueryTopology{}) != bdReadyPoolDemandShell(20, QueryTopology{FederationIdentity: ""}) {
		t.Fatal("an empty identity changed the generated read")
	}
	fenced := bdReadyPoolDemandShell(20, QueryTopology{FederationIdentity: "citadel"})
	if !strings.Contains(fenced, `handoff:citadel`) {
		t.Fatal("an identity did not reach the generated read")
	}
	// The filter keeps only the claimable rows (no "| not" half carrying the
	// foreign ones behind them) and runs before the window cut.
	if strings.Contains(fenced, `| not)`) || strings.Index(fenced, `handoff:citadel`) > strings.Index(fenced, `.[:20]`) {
		t.Fatalf("the ownership step must filter, not reorder, before the cut: %q", fenced)
	}
	// The federated reader applies the Ready predicate itself; with an identity
	// it still has to read the whole set and filter before cutting.
	fed := bdReadyPoolDemandShell(20, QueryTopology{FederatedReady: true, FederationIdentity: "citadel"})
	if !strings.Contains(fed, "--limit 0") || !strings.Contains(fed, ".[:20]") || !strings.Contains(fed, `handoff:citadel`) {
		t.Fatalf("federated read with an identity must filter before the cut: %q", fed)
	}
}

// shellQuoteForTest single-quotes s for sh.
func shellQuoteForTest(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
