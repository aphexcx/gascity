package config

import (
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/federation"
)

// Regression coverage for gf-xid finding 1h (the gf-pu2 re-gates' Major at
// internal/config/workquery.go:294 and :319, both gates): the two `bd ready`
// tiers of the pool-demand probe reordered foreign rows behind claimable ones
// (gf-4c9 row 4) but RETAINED them, and probe_pool_demand exits on the first
// nonempty tier. A routed task owned by another city therefore ended the probe
// on its own, the hook's fence refused it, the worker drained — and a legacy
// wisp this city owns, routed to the same pool but served by a LATER tier,
// was never reached, while the fenced count-form (finding 1c) counted that
// wisp and kept minting seats for it. Every tier now filters foreign rows
// before its cut, so a tier of foreign rows is empty and the probe falls
// through.

// routedDurableRow renders one `bd ready --json` row of an open, unassigned
// task routed to target with the given labels.
func routedDurableRow(id, target string, labels ...string) string {
	quoted := make([]string, 0, len(labels))
	for _, l := range labels {
		quoted = append(quoted, strconv.Quote(l))
	}
	return `{"id":` + strconv.Quote(id) + `,"status":"open","issue_type":"task","assignee":"","labels":[` +
		strings.Join(quoted, ",") + `],"metadata":{"gc.routed_to":` + strconv.Quote(target) + `}}`
}

// fakeBdReadyAndQueryRows is a fake bd whose `bd ready` answers readyRows (the
// two `bd ready` tiers) and whose `bd query` answers queryRows (the legacy
// ephemeral tier); every other verb answers an empty array.
func fakeBdReadyAndQueryRows(readyRows, queryRows []string) string {
	return `#!/bin/sh
case "$1" in
  ready) printf '%s' '[` + strings.Join(readyRows, ",") + `]' ;;
  query) printf '%s' '[` + strings.Join(queryRows, ",") + `]' ;;
  *) printf '[]' ;;
esac
`
}

// TestForeignRoutedTierDoesNotStarveLocalLegacyWork EXECUTES the generated
// Work query and the count-form against the gates' fixture: one foreign-owned
// durable task in the routed `bd ready` tier beside one locally owned legacy
// wisp routed to the same pool. With the identity the local wisp is served and
// counted 1 (the tier of foreign rows reads empty and the probe falls through
// to the legacy tier); a foreign task alone yields no work and a count of 0;
// without an identity the foreign task is served as before, and both rows
// count.
func TestForeignRoutedTierDoesNotStarveLocalLegacyWork(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	a := Agent{Name: "worker", Dir: "hello-world"}
	target := a.poolDemandTarget()
	foreign, _ := federation.OwnerLabel("jadegate")
	own, _ := federation.OwnerLabel("citadel")
	foreignTask := routedDurableRow("hw-task-foreign", target, foreign)
	ownWisp := legacyEphemeralRow("hw-wisp-own", "2026-02-01T00:00:00Z", target, own)

	work := func(topo QueryTopology, readyRows, queryRows []string) []string {
		t.Helper()
		out := strings.TrimSpace(runShellWithFakeBd(t, a.EffectiveWorkQueryFor(topo), nil, fakeBdReadyAndQueryRows(readyRows, queryRows)))
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
	count := func(topo QueryTopology, readyRows, queryRows []string) string {
		t.Helper()
		return strings.TrimSpace(runShellWithFakeBd(t, a.EffectivePoolDemandQueryFor(topo), nil, fakeBdReadyAndQueryRows(readyRows, queryRows)))
	}
	federated := QueryTopology{FederationIdentity: "citadel"}

	if got := work(QueryTopology{}, []string{foreignTask}, []string{ownWisp}); len(got) != 1 || got[0] != "hw-task-foreign" {
		t.Fatalf("without an identity the routed tier is served as before; got %v", got)
	}
	if got := count(QueryTopology{}, []string{foreignTask}, []string{ownWisp}); got != "2" {
		t.Fatalf("count without an identity = %q, want 2 (the fence is off on a non-federated city)", got)
	}
	if got := work(federated, []string{foreignTask}, []string{ownWisp}); len(got) != 1 || got[0] != "hw-wisp-own" {
		t.Fatalf("with the identity the local legacy wisp behind a foreign routed task must be served; got %v", got)
	}
	if got := count(federated, []string{foreignTask}, []string{ownWisp}); got != "1" {
		t.Fatalf("count with one foreign routed task and one own legacy wisp = %q, want 1", got)
	}
	if got := work(federated, []string{foreignTask}, nil); len(got) != 0 {
		t.Fatalf("a foreign-owned routed task alone must yield no work; got %v", got)
	}
	if got := count(federated, []string{foreignTask}, nil); got != "0" {
		t.Fatalf("count with a foreign-owned routed task alone = %q, want 0", got)
	}
	handed := routedDurableRow("hw-task-handoff", target, foreign, federation.HandoffLabel("citadel"))
	if got := work(federated, []string{foreignTask, handed}, []string{ownWisp}); len(got) != 1 || got[0] != "hw-task-handoff" {
		t.Fatalf("a foreign task handed off to this city must be served from the routed tier ahead of the legacy tier; got %v", got)
	}
}
