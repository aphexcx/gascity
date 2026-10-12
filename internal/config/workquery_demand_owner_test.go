package config

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/federation"
)

// TestPoolDemandCountAppliesTheOwnershipFence pins the generated half of the
// gf-4c9 round-2 gate finding at cmd/gc/build_desired_state.go:2048
// (finding 1c): the reconciler count-form counted every routed ready row,
// so on a federated city a task owned by another city (owner:jadegate, no
// handoff) read as demand 1 for identity citadel, a seat spawned for it,
// and the hook's claim fence refused it on arrival — start, drain, repeat.
// The count now applies the claimable predicate (the jq mirror of
// federation.MayClaim) to the aggregated set before taking its length, so
// the generated counter and the in-process one agree.
func TestPoolDemandCountAppliesTheOwnershipFence(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; count-form exercises a jq pipeline")
	}
	foreign, _ := federation.OwnerLabel("jadegate")
	own, _ := federation.OwnerLabel("citadel")
	a := Agent{Name: "worker", Dir: "hello-world"}
	run := func(topo QueryTopology, rows ...string) string {
		t.Helper()
		fakeBd := `#!/bin/sh
set -eu
case "$*" in
  *"--metadata-field gc.routed_to=hello-world/worker"*)
    printf '%s' '[` + strings.Join(rows, ",") + `]'
    ;;
  *)
    printf '[]'
    ;;
esac
`
		return strings.TrimSpace(runShellWithFakeBd(t, a.EffectivePoolDemandQueryFor(topo), nil, fakeBd))
	}
	federated := QueryTopology{FederationIdentity: "citadel"}

	if got := run(federated, claimableRow("hw-foreign", foreign)); got != "0" {
		t.Fatalf("count with one foreign-owned task and no handoff = %q, want 0", got)
	}
	if got := run(federated, claimableRow("hw-own", own)); got != "1" {
		t.Fatalf("count with one own task = %q, want 1", got)
	}
	if got := run(federated, claimableRow("hw-unowned")); got != "1" {
		t.Fatalf("count with one unowned task = %q, want 1", got)
	}
	if got := run(federated, claimableRow("hw-handoff", foreign, federation.HandoffLabel("citadel"))); got != "1" {
		t.Fatalf("count with a foreign task handed off to this city = %q, want 1", got)
	}
	if got := run(federated, claimableRow("hw-foreign", foreign), claimableRow("hw-own", own), claimableRow("hw-unowned")); got != "2" {
		t.Fatalf("count with one foreign, one own and one unowned task = %q, want 2", got)
	}
	if got := run(QueryTopology{}, claimableRow("hw-foreign", foreign)); got != "1" {
		t.Fatalf("count without an identity = %q, want 1 (the fence is off on a non-federated city)", got)
	}
}
