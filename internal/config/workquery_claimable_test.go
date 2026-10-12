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
// cmd/gc/cmd_hook_claim.go:501): on a federated city the hook's cross-city
// fence (federation.MayClaim on the decoded rows) ran AFTER the routed
// pool-demand tier had cut its 20-row window. When that window was filled by
// another city's rows the hook reported no work and drained, every retry saw
// the same foreign rows, and eligible local work behind them starved. The
// generated read now orders claimable rows AHEAD of the rest BEFORE the cut
// (claimableFirstJQClause); the Go fence still judges every row served and
// still logs the foreign rows that fit the window.

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

// TestRoutedPoolDemandWindowIsCutAfterTheOwnershipFence EXECUTES the routed
// pool-demand read against a fake bd that answers with twenty-five rows owned
// by another city followed by one row this city owns. Without an identity the
// window is the first twenty rows (all foreign: what the hook used to be
// handed and then fence to nothing); with the city's identity the ownership
// ordering runs before the cut and the own row leads the window, with the
// foreign rows behind it for the Go fence to refuse and log.
func TestRoutedPoolDemandWindowIsCutAfterTheOwnershipFence(t *testing.T) {
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
	if len(fenced) != 20 || fenced[0]["id"] != "hw-own-1" || fenced[1]["id"] != "hw-foreign-1" {
		t.Fatalf("with the identity the own row behind 25 foreign rows must lead the window, the foreign rows following in reader order; got %d rows, first two %v %v", len(fenced), fenced[0], fenced[1])
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

// TestClaimableFirstJQClauseIsEmptyWithoutIdentity is the byte-identity
// claim: a non-federated city emits exactly the read it emits today.
func TestClaimableFirstJQClauseIsEmptyWithoutIdentity(t *testing.T) {
	if got := claimableFirstJQClause(""); got != "" {
		t.Fatalf("claimableFirstJQClause(\"\") = %q, want empty", got)
	}
	if got := claimableFirstJQClause("  "); got != "" {
		t.Fatalf("claimableFirstJQClause(blank) = %q, want empty", got)
	}
	if bdReadyPoolDemandShell(20, QueryTopology{}) != bdReadyPoolDemandShell(20, QueryTopology{FederationIdentity: ""}) {
		t.Fatal("an empty identity changed the generated read")
	}
	if !strings.Contains(bdReadyPoolDemandShell(20, QueryTopology{FederationIdentity: "citadel"}), `handoff:citadel`) {
		t.Fatal("an identity did not reach the generated read")
	}
	// The federated reader applies the Ready predicate itself; with an identity
	// it still has to read the whole set and order before cutting.
	fed := bdReadyPoolDemandShell(20, QueryTopology{FederatedReady: true, FederationIdentity: "citadel"})
	if !strings.Contains(fed, "--limit 0") || !strings.Contains(fed, ".[:20]") || !strings.Contains(fed, `handoff:citadel`) {
		t.Fatalf("federated read with an identity must order before the cut: %q", fed)
	}
}

// shellQuoteForTest single-quotes s for sh.
func shellQuoteForTest(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
