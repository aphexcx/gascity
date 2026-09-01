package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/federation"
)

// Regression coverage for jg-66rdw8 (jadegate half of the cross-city
// convention agreed on citadel bead gp-0uj).
//
// On 2026-09-01 a jadegate pool worker's startup claim (`gc hook --claim`)
// acquired citadel's bead hw-57b63 out of the SHARED federated hw store: the
// bead's citadel-held lease lapsed every ~5 min (a heartbeat identity split on
// their side), jadegate's reconciler saw an expired lease on ready in_progress
// work and re-routed it to a jadegate worker, and the hook wrote lease+assignee
// before any lane rule could run. The loop repeated every tick until the rig
// was suspended.
//
// The convention (internal/federation): every bead carries `owner:<identity>`,
// identity being the creating city's city.toml [federation] identity; a claim,
// startup claim, or reconciler REFUSES a bead whose owner is a foreign city
// unless the bead also carries `handoff:<this-identity>`; legacy unlabeled
// beads stay claimable; a city with no identity is not federated and the fence
// is off. The rule is federation.MayClaim. These tests pin the claim side's two
// layers: the candidate-set fence on the
// work-query rows (layer 1) and the authoritative store re-check immediately
// before every write or adoption (layer 2). The store's Claim seam is asserted
// never to be reached for a foreign bead.

const (
	crossCityTestIdentity = "claude-jg-fence"
	crossCityThisCity     = "jadegate"
	crossCityForeignCity  = "citadel"
)

// crossCityOwnerLabel spells owner:<identity> the way the emit side does.
func crossCityOwnerLabel(identity string) string {
	label, _ := federation.OwnerLabel(identity)
	return label
}

func crossCityClaimOptions() hookClaimOptions {
	return hookClaimOptions{
		Assignee:           crossCityTestIdentity,
		IdentityCandidates: hookClaimIdentityCandidates(crossCityTestIdentity),
		RouteTargets:       hookClaimRouteTargets(crossCityTestIdentity),
		FederationIdentity: crossCityThisCity,
		JSON:               true,
	}
}

// crossCityRow renders one work-query row routed to this session with the
// given status, assignee and labels. The labels key is always present (an
// empty list when there are none): this is a label-carrying projection.
func crossCityRow(id, status, assignee string, labels ...string) string {
	quoted := make([]string, 0, len(labels))
	for _, l := range labels {
		quoted = append(quoted, strconv.Quote(l))
	}
	return `{"id":` + strconv.Quote(id) + `,"status":"` + status + `","issue_type":"task","assignee":"` + assignee +
		`","labels":[` + strings.Join(quoted, ",") + `],"metadata":{` + strconv.Quote(beadmeta.RoutedToMetadataKey) + `:` +
		strconv.Quote(crossCityTestIdentity) + `}}`
}

// crossCityBlindRow renders a row whose projection OMITS the labels key — the
// shape a custom work_query with a narrow projection produces, and the shape a
// store that elides empty lists produces for legacy work. Layer 1 cannot judge
// it; layer 2 must.
func crossCityBlindRow(id, status, assignee string) string {
	return `{"id":` + strconv.Quote(id) + `,"status":"` + status + `","issue_type":"task","assignee":"` + assignee +
		`","metadata":{` + strconv.Quote(beadmeta.RoutedToMetadataKey) + `:` + strconv.Quote(crossCityTestIdentity) + `}}`
}

// crossCityRoutedRow is an unassigned open row: the fresh-claim tier's input.
func crossCityRoutedRow(id string, labels ...string) string {
	return crossCityRow(id, "open", "", labels...)
}

// crossCityStoreBead is the store's authoritative copy of a bead, as the
// ReadWorkMeta seam returns it.
func crossCityStoreBead(id, status, assignee string, labels ...string) beads.Bead {
	return beads.Bead{
		ID: id, Status: status, Assignee: assignee, Type: "task", Labels: labels,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: crossCityTestIdentity},
	}
}

func crossCityRunner(rows ...string) WorkQueryRunner {
	return func(string, string) (string, error) {
		return `[` + strings.Join(rows, ",") + `]`, nil
	}
}

// crossCityReads is a ReadWorkMeta seam answering from store and counting
// reads; an id absent from store reads as beads.ErrNotFound.
func crossCityReads(store map[string]beads.Bead, reads *int) func(context.Context, string, []string, string, string) (beads.Bead, error) {
	return func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
		if reads != nil {
			*reads++
		}
		if b, ok := store[id]; ok {
			return b, nil
		}
		return beads.Bead{}, beads.ErrNotFound
	}
}

// crossCityOps wires the work-query rows, the store the write fence re-reads,
// and the Claim seam under test.
func crossCityOps(store map[string]beads.Bead, claim hookClaimFunc, rows ...string) hookClaimOps {
	return hookClaimOps{
		Runner:       crossCityRunner(rows...),
		Claim:        claim,
		ReadWorkMeta: crossCityReads(store, nil),
	}
}

// crossCityNeverClaim is a Claim seam that fails the test if reached.
func crossCityNeverClaim(t *testing.T) hookClaimFunc {
	t.Helper()
	return func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, bool, error) {
		t.Fatalf("store.Claim called for %q; the cross-city fence must refuse a foreign-owner bead before any claim mutation", id)
		return beads.Bead{}, false, nil
	}
}

// crossCityRecordingClaim is a Claim seam that succeeds and records the id.
func crossCityRecordingClaim(claimed *string) hookClaimFunc {
	return func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
		*claimed = id
		return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Type: "task"}, true, nil
	}
}

func decodeCrossCityResult(t *testing.T, stdout *bytes.Buffer) hookClaimJSONResult {
	t.Helper()
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	return result
}

func assertCrossCityDrain(t *testing.T, result hookClaimJSONResult) {
	t.Helper()
	if result.Action == "work" {
		t.Fatalf("REGRESSION jg-66rdw8: hook served foreign-owner bead %q as action=work (reason=%q)", result.BeadID, result.Reason)
	}
	if result.Action != "drain" || result.Reason != hookClaimReasonNoWork {
		t.Fatalf("a fenced-out candidate set is no-work, not an error: want action=drain reason=%s, got action=%q reason=%q",
			hookClaimReasonNoWork, result.Action, result.Reason)
	}
}

// assertCrossCityRefusalLogged pins the greppable stderr line: bead id, the
// owner label as written, this city, the handoff label that was missing, and
// the tier that refused.
func assertCrossCityRefusalLogged(t *testing.T, stderr *bytes.Buffer, beadID, ownerLabel, tier string) {
	t.Helper()
	got := stderr.String()
	for _, want := range []string{
		"cross-city-fence refused",
		"bead=" + beadID,
		"owner=" + strings.TrimPrefix(ownerLabel, federation.OwnerLabelPrefix),
		"this_identity=" + crossCityThisCity,
		"missing=" + federation.HandoffLabel(crossCityThisCity),
		"tier=" + tier,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr lacks %q; refusal must be greppable with every fact an incident responder needs. stderr:\n%s", want, got)
		}
	}
}

// TestHookClaimCrossCityFenceRefusesForeignOwnerFreshClaim is the primary
// regression: a routed, unassigned foreign-owner bead never reaches the claim
// CAS and the session drains as no-work.
func TestHookClaimCrossCityFenceRefusesForeignOwnerFreshClaim(t *testing.T) {
	const foreignID = "hw-57b63"
	ownerLabel := crossCityOwnerLabel(crossCityForeignCity)
	store := map[string]beads.Bead{foreignID: crossCityStoreBead(foreignID, "open", "", ownerLabel)}
	ops := crossCityOps(store, crossCityNeverClaim(t), crossCityRoutedRow(foreignID, "p1", ownerLabel))
	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", crossCityClaimOptions(), ops, &stdout, &stderr)

	assertCrossCityDrain(t, decodeCrossCityResult(t, &stdout))
	assertCrossCityRefusalLogged(t, &stderr, foreignID, ownerLabel, "candidate")
}

// TestHookClaimCrossCityFenceAllowsHandoffToThisCity: the sanctioned override.
func TestHookClaimCrossCityFenceAllowsHandoffToThisCity(t *testing.T) {
	const handedID = "hw-handed"
	labels := []string{crossCityOwnerLabel(crossCityForeignCity), federation.HandoffLabel(crossCityThisCity)}
	claimed := ""
	store := map[string]beads.Bead{handedID: crossCityStoreBead(handedID, "open", "", labels...)}
	ops := crossCityOps(store, crossCityRecordingClaim(&claimed), crossCityRoutedRow(handedID, labels...))
	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", crossCityClaimOptions(), ops, &stdout, &stderr)

	result := decodeCrossCityResult(t, &stdout)
	if result.Action != "work" || result.BeadID != handedID || claimed != handedID {
		t.Fatalf("a foreign bead handed off to this city must be claimed: got action=%q reason=%q bead=%q claimed=%q\nstderr: %s",
			result.Action, result.Reason, result.BeadID, claimed, stderr.String())
	}
	if strings.Contains(stderr.String(), "cross-city-fence refused") {
		t.Fatalf("a handed-off bead must not log a refusal:\n%s", stderr.String())
	}
}

// TestHookClaimCrossCityFenceAllowsOwnCityAndUnlabeled: own work and legacy
// unlabeled work are byte-for-byte unchanged.
func TestHookClaimCrossCityFenceAllowsOwnCityAndUnlabeled(t *testing.T) {
	cases := map[string][]string{
		"own city":                            {crossCityOwnerLabel(crossCityThisCity)},
		"miscased prefix, not an owner label": {"Owner:JadeGate"},
		"padded prefix, not an owner label":   {"  owner: jadegate "},
		"unlabeled":                           nil,
		"unrelated labels":                    {"p1", "hold-ish", "ownership-review"},
		"handoff without an owner":            {federation.HandoffLabel(crossCityThisCity)},
	}
	for name, labels := range cases {
		t.Run(name, func(t *testing.T) {
			const ownID = "jg-own"
			claimed := ""
			store := map[string]beads.Bead{ownID: crossCityStoreBead(ownID, "open", "", labels...)}
			ops := crossCityOps(store, crossCityRecordingClaim(&claimed), crossCityRoutedRow(ownID, labels...))
			var stdout, stderr bytes.Buffer
			doHookClaim("bd ready --json", "/tmp/work", crossCityClaimOptions(), ops, &stdout, &stderr)

			result := decodeCrossCityResult(t, &stdout)
			if result.Action != "work" || result.BeadID != ownID || claimed != ownID {
				t.Fatalf("labels %q must stay claimable: got action=%q reason=%q bead=%q claimed=%q\nstderr: %s",
					labels, result.Action, result.Reason, result.BeadID, claimed, stderr.String())
			}
		})
	}
}

// TestHookClaimCrossCityFenceMatchesExactLowercaseLabels: labels are exact
// strings (the identity is validated lowercase at config load and the emit side
// spells the label from it verbatim), so neither prefix nor value is trimmed or
// case-folded — a miscased or padded label is simply not an owner label, and an
// owner value differing in case from the identity is foreign. Ambiguous or
// malformed owner sets fail closed; a handoff naming this city always permits.
func TestHookClaimCrossCityFenceMatchesExactLowercaseLabels(t *testing.T) {
	refused := map[string][]string{
		"foreign + handoff elsewhere":   {"owner:citadel", "handoff:boomtown"},
		"foreign + empty handoff":       {"owner:citadel", federation.HandoffLabelPrefix},
		"mixed owner set incl. ours":    {"owner:citadel", "owner:jadegate"},
		"malformed empty owner value":   {federation.OwnerLabelPrefix},
		"owner value miscased vs ident": {"owner:Jadegate"},
		"handoff miscased vs identity":  {"owner:citadel", "handoff:Jadegate"},
	}
	for name, labels := range refused {
		t.Run("refused/"+name, func(t *testing.T) {
			store := map[string]beads.Bead{"hw-variant": crossCityStoreBead("hw-variant", "open", "", labels...)}
			ops := crossCityOps(store, crossCityNeverClaim(t), crossCityRoutedRow("hw-variant", labels...))
			var stdout, stderr bytes.Buffer
			doHookClaim("bd ready --json", "/tmp/work", crossCityClaimOptions(), ops, &stdout, &stderr)
			assertCrossCityDrain(t, decodeCrossCityResult(t, &stdout))
		})
	}
	allowed := map[string][]string{
		"mixed owner set with handoff":       {"owner:citadel", "owner:jadegate", "handoff:jadegate"},
		"several handoffs incl. ours":        {"owner:citadel", "handoff:boomtown", "handoff:jadegate"},
		"miscased prefix is not an owner":    {"Owner:Citadel"},
		"padded label is not an owner":       {" owner:citadel "},
		"identity trimmed before comparison": {"owner:jadegate"},
	}
	for name, labels := range allowed {
		t.Run("allowed/"+name, func(t *testing.T) {
			claimed := ""
			store := map[string]beads.Bead{"hw-variant": crossCityStoreBead("hw-variant", "open", "", labels...)}
			ops := crossCityOps(store, crossCityRecordingClaim(&claimed), crossCityRoutedRow("hw-variant", labels...))
			opts := crossCityClaimOptions()
			if name == "identity trimmed before comparison" {
				opts.FederationIdentity = "  jadegate "
			}
			var stdout, stderr bytes.Buffer
			doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)
			if result := decodeCrossCityResult(t, &stdout); result.Action != "work" || claimed != "hw-variant" {
				t.Fatalf("labels %q must be claimable: got action=%q reason=%q claimed=%q\nstderr: %s",
					labels, result.Action, result.Reason, claimed, stderr.String())
			}
		})
	}
}

// TestHookClaimCrossCityFenceDoesNotServeForeignExistingAssignment is the
// loop-stopper: the bead the incident left in_progress and assigned to a
// jadegate session must NOT be re-served as work on the next tick. Adoption
// mints no write, but serving it makes the session heartbeat a lease it should
// never hold.
func TestHookClaimCrossCityFenceDoesNotServeForeignExistingAssignment(t *testing.T) {
	const foreignID = "hw-57b63"
	ownerLabel := crossCityOwnerLabel(crossCityForeignCity)
	store := map[string]beads.Bead{foreignID: crossCityStoreBead(foreignID, "in_progress", crossCityTestIdentity, ownerLabel)}
	ops := crossCityOps(store, crossCityNeverClaim(t), crossCityRow(foreignID, "in_progress", crossCityTestIdentity, ownerLabel))
	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", crossCityClaimOptions(), ops, &stdout, &stderr)

	assertCrossCityDrain(t, decodeCrossCityResult(t, &stdout))
	assertCrossCityRefusalLogged(t, &stderr, foreignID, ownerLabel, "candidate")
}

// TestHookClaimCrossCityFenceDoesNotPromoteForeignReadyAssignment covers the
// ready-assignment tier: an OPEN foreign bead pre-pinned to this session (the
// shape a reconciler re-route produces) is not promoted to in_progress.
func TestHookClaimCrossCityFenceDoesNotPromoteForeignReadyAssignment(t *testing.T) {
	const foreignID = "hw-pinned"
	ownerLabel := crossCityOwnerLabel(crossCityForeignCity)
	store := map[string]beads.Bead{foreignID: crossCityStoreBead(foreignID, "open", crossCityTestIdentity, ownerLabel)}
	ops := crossCityOps(store, crossCityNeverClaim(t), crossCityRow(foreignID, "open", crossCityTestIdentity, ownerLabel))
	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", crossCityClaimOptions(), ops, &stdout, &stderr)

	assertCrossCityDrain(t, decodeCrossCityResult(t, &stdout))
}

// TestHookClaimCrossCityFenceSkipsForeignAndClaimsOwnWork: the fence skips,
// it does not abort — own routed work behind a foreign bead is still claimed,
// and the foreign one is still logged.
func TestHookClaimCrossCityFenceSkipsForeignAndClaimsOwnWork(t *testing.T) {
	const (
		foreignID = "hw-foreign"
		ownID     = "jg-own"
	)
	ownerLabel := crossCityOwnerLabel(crossCityForeignCity)
	claimed := ""
	store := map[string]beads.Bead{
		foreignID: crossCityStoreBead(foreignID, "open", "", ownerLabel),
		ownID:     crossCityStoreBead(ownID, "open", "", crossCityOwnerLabel(crossCityThisCity)),
	}
	ops := crossCityOps(store,
		func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			if id == foreignID {
				t.Fatalf("store.Claim called for foreign bead %q; want only %q", id, ownID)
			}
			claimed = id
			return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Type: "task"}, true, nil
		},
		crossCityRoutedRow(foreignID, ownerLabel),
		crossCityRoutedRow(ownID, crossCityOwnerLabel(crossCityThisCity)),
	)
	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", crossCityClaimOptions(), ops, &stdout, &stderr)

	result := decodeCrossCityResult(t, &stdout)
	if result.Action != "work" || result.BeadID != ownID || claimed != ownID {
		t.Fatalf("want own bead %q claimed behind the fenced foreign one, got action=%q reason=%q bead=%q claimed=%q",
			ownID, result.Action, result.Reason, result.BeadID, claimed)
	}
	assertCrossCityRefusalLogged(t, &stderr, foreignID, ownerLabel, "candidate")
}

// TestHookClaimCrossCityFenceSkipsForeignContinuationSiblings: the second
// assignee write the hook performs — preassigning open continuation-group
// siblings after a claim — runs through the fenced AssignContinuation seam. A
// foreign sibling is skipped (and logged, tier=continuation); a foreign sibling
// with a handoff and an own sibling are assigned.
func TestHookClaimCrossCityFenceSkipsForeignContinuationSiblings(t *testing.T) {
	const (
		rootID    = "jg-root"
		group     = "pool-workflow"
		claimedID = "jg-step-1"
		ownSib    = "jg-step-2"
		foreignSb = "hw-step-3"
		handedSb  = "hw-step-4"
	)
	groupMeta := func() map[string]string {
		return map[string]string{
			beadmeta.RootBeadIDMetadataKey:        rootID,
			beadmeta.ContinuationGroupMetadataKey: group,
			beadmeta.RoutedToMetadataKey:          crossCityTestIdentity,
		}
	}
	sibling := func(id string, labels ...string) beads.Bead {
		return beads.Bead{ID: id, Status: "open", Type: "task", Labels: labels, Metadata: groupMeta()}
	}
	own := sibling(ownSib, crossCityOwnerLabel(crossCityThisCity))
	foreign := sibling(foreignSb, crossCityOwnerLabel(crossCityForeignCity))
	handed := sibling(handedSb, crossCityOwnerLabel(crossCityForeignCity), federation.HandoffLabel(crossCityThisCity))
	store := map[string]beads.Bead{
		claimedID: {ID: claimedID, Status: "open", Type: "task", Labels: []string{crossCityOwnerLabel(crossCityThisCity)}, Metadata: groupMeta()},
		ownSib:    own, foreignSb: foreign, handedSb: handed,
	}
	var assigned []string
	ops := crossCityOps(store,
		func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Type: "task", Metadata: groupMeta()}, true, nil
		},
		crossCityRoutedRow(claimedID, crossCityOwnerLabel(crossCityThisCity)),
	)
	ops.ListContinuation = func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
		return []beads.Bead{own, foreign, handed}, nil
	}
	ops.AssignContinuation = func(_ context.Context, _ string, _ []string, id, _ string) error {
		if id == foreignSb {
			t.Fatalf("AssignContinuation wrote assignee onto foreign sibling %q; the fence must cover the preassign write too", id)
		}
		assigned = append(assigned, id)
		return nil
	}
	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", crossCityClaimOptions(), ops, &stdout, &stderr)

	result := decodeCrossCityResult(t, &stdout)
	if result.Action != "work" || result.BeadID != claimedID {
		t.Fatalf("want %q claimed, got action=%q reason=%q bead=%q\nstderr: %s", claimedID, result.Action, result.Reason, result.BeadID, stderr.String())
	}
	if want := []string{ownSib, handedSb}; strings.Join(assigned, ",") != strings.Join(want, ",") {
		t.Fatalf("preassigned %q, want %q (own + handed-off siblings only)", assigned, want)
	}
	if strings.Join(result.ContinuationAssigned, ",") != strings.Join(assigned, ",") {
		t.Fatalf("result.ContinuationAssigned = %q, want %q", result.ContinuationAssigned, assigned)
	}
	assertCrossCityRefusalLogged(t, &stderr, foreignSb, crossCityOwnerLabel(crossCityForeignCity), "continuation")
}

// TestHookClaimCrossCityWriteFenceJudgesTheStoreCopy is layer 2 (codex r2 P1):
// when the work-query projection omits labels, or the store's copy of the bead
// no longer agrees with the row, the authoritative copy read immediately before
// the write is what decides — on every write seam and on adoption.
func TestHookClaimCrossCityWriteFenceJudgesTheStoreCopy(t *testing.T) {
	const beadID = "hw-blind"
	foreignOwner := crossCityOwnerLabel(crossCityForeignCity)

	t.Run("label-blind fresh claim, store copy foreign: refused at the claim write", func(t *testing.T) {
		store := map[string]beads.Bead{beadID: crossCityStoreBead(beadID, "open", "", foreignOwner)}
		ops := crossCityOps(store, crossCityNeverClaim(t), crossCityBlindRow(beadID, "open", ""))
		var stdout, stderr bytes.Buffer
		doHookClaim("bd ready --json", "/tmp/work", crossCityClaimOptions(), ops, &stdout, &stderr)
		assertCrossCityDrain(t, decodeCrossCityResult(t, &stdout))
		assertCrossCityRefusalLogged(t, &stderr, beadID, foreignOwner, "claim")
	})
	t.Run("label-blind ready assignment, store copy foreign: not promoted", func(t *testing.T) {
		store := map[string]beads.Bead{beadID: crossCityStoreBead(beadID, "open", crossCityTestIdentity, foreignOwner)}
		ops := crossCityOps(store, crossCityNeverClaim(t), crossCityBlindRow(beadID, "open", crossCityTestIdentity))
		var stdout, stderr bytes.Buffer
		doHookClaim("bd ready --json", "/tmp/work", crossCityClaimOptions(), ops, &stdout, &stderr)
		assertCrossCityDrain(t, decodeCrossCityResult(t, &stdout))
		assertCrossCityRefusalLogged(t, &stderr, beadID, foreignOwner, "claim")
	})
	t.Run("label-blind existing assignment, store copy foreign: not served", func(t *testing.T) {
		store := map[string]beads.Bead{beadID: crossCityStoreBead(beadID, "in_progress", crossCityTestIdentity, foreignOwner)}
		ops := crossCityOps(store, crossCityNeverClaim(t), crossCityBlindRow(beadID, "in_progress", crossCityTestIdentity))
		var stdout, stderr bytes.Buffer
		doHookClaim("bd ready --json", "/tmp/work", crossCityClaimOptions(), ops, &stdout, &stderr)
		assertCrossCityDrain(t, decodeCrossCityResult(t, &stdout))
		assertCrossCityRefusalLogged(t, &stderr, beadID, foreignOwner, "adoption")
	})
	t.Run("existing assignment with local or empty row labels, store copy relabeled foreign: not served", func(t *testing.T) {
		for name, rowLabels := range map[string][]string{"own": {crossCityOwnerLabel(crossCityThisCity)}, "empty": {}} {
			t.Run(name, func(t *testing.T) {
				store := map[string]beads.Bead{beadID: crossCityStoreBead(beadID, "in_progress", crossCityTestIdentity, foreignOwner)}
				ops := crossCityOps(store, crossCityNeverClaim(t), crossCityRow(beadID, "in_progress", crossCityTestIdentity, rowLabels...))
				var stdout, stderr bytes.Buffer
				doHookClaim("bd ready --json", "/tmp/work", crossCityClaimOptions(), ops, &stdout, &stderr)
				assertCrossCityDrain(t, decodeCrossCityResult(t, &stdout))
				assertCrossCityRefusalLogged(t, &stderr, beadID, foreignOwner, "adoption")
			})
		}
	})
	t.Run("row says own, store copy relabeled foreign since the snapshot: refused at the claim write", func(t *testing.T) {
		store := map[string]beads.Bead{beadID: crossCityStoreBead(beadID, "open", "", foreignOwner)}
		ops := crossCityOps(store, crossCityNeverClaim(t), crossCityRoutedRow(beadID, crossCityOwnerLabel(crossCityThisCity)))
		var stdout, stderr bytes.Buffer
		doHookClaim("bd ready --json", "/tmp/work", crossCityClaimOptions(), ops, &stdout, &stderr)
		assertCrossCityDrain(t, decodeCrossCityResult(t, &stdout))
		assertCrossCityRefusalLogged(t, &stderr, beadID, foreignOwner, "claim")
	})
	t.Run("label-blind continuation sibling, store copy foreign: not assigned", func(t *testing.T) {
		const rootID, group, sib = "jg-root", "pool-workflow", "hw-sib"
		meta := map[string]string{
			beadmeta.RootBeadIDMetadataKey:        rootID,
			beadmeta.ContinuationGroupMetadataKey: group,
			beadmeta.RoutedToMetadataKey:          crossCityTestIdentity,
		}
		blindSibling := beads.Bead{ID: sib, Status: "open", Type: "task", Metadata: meta} // Labels nil
		store := map[string]beads.Bead{
			beadID: {ID: beadID, Status: "open", Type: "task", Labels: []string{}, Metadata: meta},
			sib:    {ID: sib, Status: "open", Type: "task", Labels: []string{foreignOwner}, Metadata: meta},
		}
		ops := crossCityOps(store,
			func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
				return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Type: "task", Metadata: meta}, true, nil
			},
			crossCityRoutedRow(beadID))
		ops.ListContinuation = func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
			return []beads.Bead{blindSibling}, nil
		}
		ops.AssignContinuation = func(_ context.Context, _ string, _ []string, id, _ string) error {
			t.Fatalf("AssignContinuation reached for label-blind foreign sibling %q", id)
			return nil
		}
		var stdout, stderr bytes.Buffer
		doHookClaim("bd ready --json", "/tmp/work", crossCityClaimOptions(), ops, &stdout, &stderr)
		if result := decodeCrossCityResult(t, &stdout); result.Action != "work" || len(result.ContinuationAssigned) != 0 {
			t.Fatalf("want the claim served with no siblings assigned, got %+v\nstderr: %s", result, stderr.String())
		}
		assertCrossCityRefusalLogged(t, &stderr, sib, foreignOwner, "continuation")
	})
	t.Run("label-blind row, store copy own or unlabeled: claimed", func(t *testing.T) {
		for name, labels := range map[string][]string{"own": {crossCityOwnerLabel(crossCityThisCity)}, "unlabeled": nil} {
			t.Run(name, func(t *testing.T) {
				claimed := ""
				store := map[string]beads.Bead{beadID: crossCityStoreBead(beadID, "open", "", labels...)}
				ops := crossCityOps(store, crossCityRecordingClaim(&claimed), crossCityBlindRow(beadID, "open", ""))
				var stdout, stderr bytes.Buffer
				doHookClaim("bd ready --json", "/tmp/work", crossCityClaimOptions(), ops, &stdout, &stderr)
				if result := decodeCrossCityResult(t, &stdout); result.Action != "work" || claimed != beadID {
					t.Fatalf("store labels %q: want claimed, got action=%q reason=%q claimed=%q\nstderr: %s",
						labels, result.Action, result.Reason, claimed, stderr.String())
				}
			})
		}
	})
}

// TestHookClaimCrossCityWriteFenceReadFailureFailsClosed pins layer 2's
// failure contract (codex r3 P1): nothing is written or served unverified. A
// read that fails surfaces as the seam's own error, so each tier handles it
// exactly as a failing write at that point:
//
//	fresh claim, not-found or store error  → skipped, drain claims_errored
//	ready assignment, not-found            → skipped as held elsewhere, drain claims_errored
//	ready assignment, store error          → fail-closed exit 1 (the tier's own rule)
//	existing assignment, unresolved        → fail-closed exit 1 (not found in any leg, or a store error)
//	continuation sibling, any failure      → the preassign fails as any assign failure does
//
// Never a claim, and never a no_work drain that launders a store failure into
// idle. Cross-leg resolution of an adopted bead is pinned separately in
// TestClaimHookWorkCrossCityFenceResolvesAcrossFederatedLegs.
func TestHookClaimCrossCityWriteFenceReadFailureFailsClosed(t *testing.T) {
	const beadID = "hw-unverified"
	failingRead := func(err error) func(context.Context, string, []string, string, string) (beads.Bead, error) {
		return func(context.Context, string, []string, string, string) (beads.Bead, error) { return beads.Bead{}, err }
	}
	for name, readErr := range map[string]error{"store error": errors.New("store contention"), "not found": beads.ErrNotFound} {
		t.Run("fresh claim, "+name, func(t *testing.T) {
			ops := crossCityOps(nil, crossCityNeverClaim(t), crossCityBlindRow(beadID, "open", ""))
			ops.ReadWorkMeta = failingRead(readErr)
			var stdout, stderr bytes.Buffer
			doHookClaim("bd ready --json", "/tmp/work", crossCityClaimOptions(), ops, &stdout, &stderr)
			result := decodeCrossCityResult(t, &stdout)
			if result.Action != "drain" || result.Reason != hookClaimReasonClaimsErrored {
				t.Fatalf("want a claims_errored drain for an unverifiable candidate, got action=%q reason=%q\nstderr: %s", result.Action, result.Reason, stderr.String())
			}
			if !strings.Contains(stderr.String(), "could not be verified before the claim") {
				t.Fatalf("stderr must name the unverified bead:\n%s", stderr.String())
			}
		})
	}
	t.Run("ready assignment, store error: fails closed", func(t *testing.T) {
		ops := crossCityOps(nil, crossCityNeverClaim(t), crossCityBlindRow(beadID, "open", crossCityTestIdentity))
		ops.ReadWorkMeta = failingRead(errors.New("store contention"))
		var stdout, stderr bytes.Buffer
		if code := doHookClaim("bd ready --json", "/tmp/work", crossCityClaimOptions(), ops, &stdout, &stderr); code != 1 || stdout.Len() != 0 {
			t.Fatalf("want exit 1 with no result for an unverifiable owned bead, got code=%d stdout=%q", code, stdout.String())
		}
	})
	t.Run("existing assignment, not found in any leg: fails closed", func(t *testing.T) {
		// A direct caller has only its own leg (no WorkLegs); a bead it cannot
		// find is never served on the strength of the row.
		ops := crossCityOps(nil, crossCityNeverClaim(t), crossCityRow(beadID, "in_progress", crossCityTestIdentity, crossCityOwnerLabel(crossCityThisCity)))
		ops.ReadWorkMeta = failingRead(beads.ErrNotFound)
		var stdout, stderr bytes.Buffer
		if code := doHookClaim("bd ready --json", "/tmp/work", crossCityClaimOptions(), ops, &stdout, &stderr); code != 1 || stdout.Len() != 0 {
			t.Fatalf("want exit 1 with nothing served for an owned bead no leg holds, got code=%d stdout=%q", code, stdout.String())
		}
	})
	t.Run("existing assignment, store error: fails closed", func(t *testing.T) {
		ops := crossCityOps(nil, crossCityNeverClaim(t), crossCityRow(beadID, "in_progress", crossCityTestIdentity, crossCityOwnerLabel(crossCityThisCity)))
		ops.ReadWorkMeta = failingRead(errors.New("store contention"))
		var stdout, stderr bytes.Buffer
		if code := doHookClaim("bd ready --json", "/tmp/work", crossCityClaimOptions(), ops, &stdout, &stderr); code != 1 || stdout.Len() != 0 {
			t.Fatalf("want exit 1 with nothing served for an unverifiable owned bead, got code=%d stdout=%q", code, stdout.String())
		}
		if !strings.Contains(stderr.String(), "could not verify the bead") {
			t.Fatalf("stderr must say why:\n%s", stderr.String())
		}
	})
	t.Run("continuation sibling, store error: not assigned", func(t *testing.T) {
		const rootID, group, sib = "jg-root", "pool-workflow", "hw-sib"
		meta := map[string]string{
			beadmeta.RootBeadIDMetadataKey:        rootID,
			beadmeta.ContinuationGroupMetadataKey: group,
			beadmeta.RoutedToMetadataKey:          crossCityTestIdentity,
		}
		own := crossCityStoreBead(beadID, "open", "", crossCityOwnerLabel(crossCityThisCity))
		own.Metadata = meta
		reads := 0
		ops := crossCityOps(map[string]beads.Bead{beadID: own},
			func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
				return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Type: "task", Metadata: meta}, true, nil
			},
			crossCityRoutedRow(beadID, crossCityOwnerLabel(crossCityThisCity)))
		ops.ReadWorkMeta = func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
			reads++
			if id == beadID {
				return own, nil
			}
			return beads.Bead{}, errors.New("store contention")
		}
		ops.ListContinuation = func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
			return []beads.Bead{{ID: sib, Status: "open", Type: "task", Metadata: meta}}, nil
		}
		ops.AssignContinuation = func(_ context.Context, _ string, _ []string, id, _ string) error {
			t.Fatalf("AssignContinuation reached for unverifiable sibling %q", id)
			return nil
		}
		var stdout, stderr bytes.Buffer
		code := doHookClaim("bd ready --json", "/tmp/work", crossCityClaimOptions(), ops, &stdout, &stderr)
		if code != 1 || !strings.Contains(stderr.String(), "could not be verified before the preassign") {
			t.Fatalf("an unverifiable sibling must fail the preassign as any assign failure does: code=%d\nstderr: %s", code, stderr.String())
		}
	})
}

// TestHookClaimCrossCityWriteFenceInstallsOnce: the federated claim loop hands
// per-store copies of the ops through tryHookClaim; wrapping twice would cost
// two authoritative reads per write.
func TestHookClaimCrossCityWriteFenceInstallsOnce(t *testing.T) {
	reads := 0
	ops := hookClaimOps{
		Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee}, true, nil
		},
		ReadWorkMeta: crossCityReads(map[string]beads.Bead{"jg-1": crossCityStoreBead("jg-1", "open", "")}, &reads),
	}
	ops.applyDefaults()
	var stderr bytes.Buffer
	ops.installCrossCityWriteFence(crossCityClaimOptions(), &stderr)
	ops.installCrossCityWriteFence(crossCityClaimOptions(), &stderr)
	if _, ok, err := ops.Claim(context.Background(), "/tmp/work", nil, "jg-1", crossCityTestIdentity); err != nil || !ok {
		t.Fatalf("claim = (%v, %v), want ok", ok, err)
	}
	if reads != 1 {
		t.Fatalf("authoritative reads per claim = %d, want exactly 1", reads)
	}
}

// TestHookClaimCrossCityRefusalLineSurvivesHostileValues pins the complete
// emitted line, not only Reason(): a bead id or city name carrying a newline
// must not break the one-line contract or forge a second line (codex r2 P2).
func TestHookClaimCrossCityRefusalLineSurvivesHostileValues(t *testing.T) {
	const evilID = "hw-evil\nforged=line"
	ownerLabel := crossCityOwnerLabel(crossCityForeignCity)
	store := map[string]beads.Bead{evilID: crossCityStoreBead(evilID, "open", "", ownerLabel)}
	ops := crossCityOps(store, crossCityNeverClaim(t), crossCityRoutedRow(evilID, ownerLabel))
	opts := crossCityClaimOptions()
	opts.FederationIdentity = "jade\ngate"
	var stdout, stderr bytes.Buffer
	doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)

	assertCrossCityDrain(t, decodeCrossCityResult(t, &stdout))
	lines := strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n")
	refusals := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "forged=") || strings.HasPrefix(line, "gate") {
			t.Fatalf("a hostile value forged its own log line %q; stderr:\n%s", line, stderr.String())
		}
		if strings.Contains(line, "cross-city-fence refused") {
			refusals++
			for _, want := range []string{`bead="hw-evil\nforged=line"`, `this_identity="jade\ngate"`, `missing="handoff:jade\ngate"`} {
				if !strings.Contains(line, want) {
					t.Errorf("refusal line lacks quoted %s: %q", want, line)
				}
			}
		}
	}
	if refusals != 1 {
		t.Fatalf("want exactly one refusal line, got %d; stderr:\n%s", refusals, stderr.String())
	}
}

// TestHookClaimCrossCityFenceIsOffWithoutFederationIdentity pins the opt-in:
// a city with no [federation] identity is not federated, and the fence is off —
// an owner-labeled bead is claimed like any other, nothing is re-read, nothing
// is logged. This is also the mode every direct doHookClaim caller runs in
// unless it opts in, so the legacy hook tests are unaffected by the fence.
func TestHookClaimCrossCityFenceIsOffWithoutFederationIdentity(t *testing.T) {
	opts := crossCityClaimOptions()
	opts.FederationIdentity = ""
	for name, labels := range map[string][]string{
		"foreign owner":   {crossCityOwnerLabel(crossCityForeignCity)},
		"malformed owner": {federation.OwnerLabelPrefix},
		"unlabeled":       nil,
	} {
		t.Run(name, func(t *testing.T) {
			claimed := ""
			reads := 0
			ops := crossCityOps(map[string]beads.Bead{}, crossCityRecordingClaim(&claimed), crossCityRoutedRow("hw-any", labels...))
			ops.ReadWorkMeta = crossCityReads(map[string]beads.Bead{}, &reads)
			var stdout, stderr bytes.Buffer
			doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr)
			if result := decodeCrossCityResult(t, &stdout); result.Action != "work" || claimed != "hw-any" {
				t.Fatalf("with no federation identity every bead is claimable: labels %q gave action=%q reason=%q claimed=%q",
					labels, result.Action, result.Reason, claimed)
			}
			if reads != 0 || strings.Contains(stderr.String(), "cross-city-fence") {
				t.Fatalf("the fence must be entirely off without an identity: reads=%d stderr=%q", reads, stderr.String())
			}
		})
	}
}

// TestHookClaimCrossCityFenceReadsBindingResidentBeadsThroughTheClassRoute
// composes the fence with classRoutedHookClaimOps (codex r3 P3): on a split
// city the work store answers not-found for a binding-resident bead and the
// class route escalates the read to the binding, so layer 2's authoritative
// read is the BINDING's copy. A label-blind row for a foreign binding bead is
// refused before the binding's claim CAS runs, and a foreign binding sibling is
// refused before the binding's assignee write runs; the binding is untouched.
func TestHookClaimCrossCityFenceReadsBindingResidentBeadsThroughTheClassRoute(t *testing.T) {
	foreignOwner := crossCityOwnerLabel(crossCityForeignCity)
	routed := map[string]string{beadmeta.RoutedToMetadataKey: crossCityTestIdentity}
	notFoundRead := func(context.Context, string, []string, string, string) (beads.Bead, error) {
		return beads.Bead{}, beads.ErrNotFound
	}

	t.Run("foreign binding bead is refused before the binding claim", func(t *testing.T) {
		class := newClaimRouteClassStore(t)
		foreign := mintClaimRouteBead(t, class, "gcg-900", routed)
		if err := class.Update(foreign.ID, beads.UpdateOpts{Labels: []string{foreignOwner}}); err != nil {
			t.Fatal(err)
		}
		route := newClaimRouteFor(t, class)
		ops := classRoutedHookClaimOps(hookClaimOps{
			Runner:       crossCityRunner(crossCityBlindRow(foreign.ID, "open", "")),
			Claim:        notFoundClaim(t, foreign.ID),
			ReadWorkMeta: notFoundRead,
		}, route)
		var stdout, stderr bytes.Buffer
		doHookClaim("bd ready --json", "/work", crossCityClaimOptions(), ops, &stdout, &stderr)

		assertCrossCityDrain(t, decodeCrossCityResult(t, &stdout))
		assertCrossCityRefusalLogged(t, &stderr, foreign.ID, foreignOwner, "claim")
		current, err := class.Get(foreign.ID)
		if err != nil || current.Assignee != "" || current.Status != "open" {
			t.Fatalf("binding copy of %s = (%q, %q, err=%v), want untouched open/unassigned", foreign.ID, current.Status, current.Assignee, err)
		}
	})

	t.Run("foreign binding sibling is refused before the binding preassign", func(t *testing.T) {
		class := newClaimRouteClassStore(t)
		root := mintClaimRouteBead(t, class, "gcg-root", nil)
		group := map[string]string{
			beadmeta.RootBeadIDMetadataKey:        root.ID,
			beadmeta.ContinuationGroupMetadataKey: "batch-1",
			beadmeta.RoutedToMetadataKey:          crossCityTestIdentity,
		}
		own := mintClaimRouteBead(t, class, "gcg-901", group)
		if err := class.Update(own.ID, beads.UpdateOpts{Labels: []string{crossCityOwnerLabel(crossCityThisCity)}}); err != nil {
			t.Fatal(err)
		}
		sibling := mintClaimRouteBead(t, class, "gcg-902", group)
		if err := class.Update(sibling.ID, beads.UpdateOpts{Labels: []string{foreignOwner}}); err != nil {
			t.Fatal(err)
		}
		route := newClaimRouteFor(t, class)
		ops := classRoutedHookClaimOps(hookClaimOps{
			Runner:       crossCityRunner(crossCityBlindRow(own.ID, "open", "")),
			Claim:        notFoundClaim(t, own.ID),
			ReadWorkMeta: notFoundRead,
			ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
				return nil, nil // the work store holds no member; the route fills the answer from the binding
			},
			AssignContinuation: func(_ context.Context, _ string, _ []string, id, _ string) error {
				t.Fatalf("work-scope assign ran for binding-resident sibling %q", id)
				return nil
			},
		}, route)
		var stdout, stderr bytes.Buffer
		doHookClaim("bd ready --json", "/work", crossCityClaimOptions(), ops, &stdout, &stderr)

		result := decodeCrossCityResult(t, &stdout)
		if result.Action != "work" || result.BeadID != own.ID || len(result.ContinuationAssigned) != 0 {
			t.Fatalf("want the own binding bead claimed with no siblings assigned, got %+v\nstderr: %s", result, stderr.String())
		}
		assertCrossCityRefusalLogged(t, &stderr, sibling.ID, foreignOwner, "continuation")
		claimed, err := class.Get(own.ID)
		if err != nil || claimed.Status != "in_progress" || claimed.Assignee != crossCityTestIdentity {
			t.Fatalf("binding copy of %s = (%q, %q, err=%v), want in_progress owned by this session", own.ID, claimed.Status, claimed.Assignee, err)
		}
		untouched, err := class.Get(sibling.ID)
		if err != nil || untouched.Assignee != "" {
			t.Fatalf("binding copy of foreign sibling %s = assignee %q (err=%v), want unassigned", sibling.ID, untouched.Assignee, err)
		}
	})
}

// TestClaimHookWorkCrossCityFenceResolvesAcrossFederatedLegs drives the real
// federated claim loop (claimHookWorkWithRunner) with two legs and the write
// fence armed (codex r4 P1/P2). The primary leg runs the city-wide reader, so
// it serves a row for a bead the CITY leg holds; the primary's authoritative
// pre-read answers not-found, and what happens next must be decided by the
// leg that holds the bead — never by the row.
func TestClaimHookWorkCrossCityFenceResolvesAcrossFederatedLegs(t *testing.T) {
	const beadID = "hw-city"
	foreignOwner := crossCityOwnerLabel(crossCityForeignCity)
	stores := []hookStore{
		{dir: "rig", env: []string{"GC_STORE=rig"}},   // primary: city-wide reader, does not hold the bead
		{dir: "city", env: []string{"GC_STORE=city"}}, // holds the bead
	}
	// legReads answers the pre-write read per leg: the rig leg never holds the
	// bead, the city leg holds the given copy.
	legReads := func(cityCopy beads.Bead, reads map[string]int) func(context.Context, string, []string, string, string) (beads.Bead, error) {
		return func(_ context.Context, dir string, _ []string, id, _ string) (beads.Bead, error) {
			reads[dir]++
			if dir == "city" && id == beadID {
				return cityCopy, nil
			}
			return beads.Bead{}, beads.ErrNotFound
		}
	}
	quietOps := func() hookClaimOps {
		return hookClaimOps{
			EmitClaimRejected: func(string, string, string) {},
			ResolveWorkBranch: func(string) string { return "" },
		}
	}

	t.Run("fresh claim: only the holding leg's claim runs, after its own copy passes", func(t *testing.T) {
		run := func(_, _ string, _ []string) (string, error) {
			return `[` + crossCityBlindRow(beadID, "open", "") + `]`, nil
		}
		reads := map[string]int{}
		var claimDirs []string
		ops := quietOps()
		ops.ReadWorkMeta = legReads(crossCityStoreBead(beadID, "open", "", crossCityOwnerLabel(crossCityThisCity)), reads)
		ops.Claim = func(_ context.Context, dir string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			claimDirs = append(claimDirs, dir)
			return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Type: "task"}, true, nil
		}
		var stdout, stderr bytes.Buffer
		code := claimHookWorkWithRunner("bd ready --json", "rig", stores[0].env, stores, crossCityClaimOptions(), ops, run, func(string, error) {}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("claimHookWorkWithRunner = %d, want 0; stderr=%s", code, stderr.String())
		}
		if result := decodeCrossCityResult(t, &stdout); result.BeadID != beadID || result.Reason != "claimed" {
			t.Fatalf("result = %+v, want %s claimed in the city leg", result, beadID)
		}
		if strings.Join(claimDirs, ",") != "city" {
			t.Fatalf("claim ran in legs %q, want only the city leg (the rig leg's pre-read was not-found, so its claim must never run)", claimDirs)
		}
		if reads["rig"] == 0 || reads["city"] == 0 {
			t.Fatalf("pre-write reads per leg = %v, want both legs consulted", reads)
		}
	})

	t.Run("fresh claim: the holding leg's copy is foreign, so no leg claims", func(t *testing.T) {
		run := func(_, _ string, _ []string) (string, error) {
			return `[` + crossCityBlindRow(beadID, "open", "") + `]`, nil
		}
		ops := quietOps()
		ops.ReadWorkMeta = legReads(crossCityStoreBead(beadID, "open", "", foreignOwner), map[string]int{})
		ops.Claim = crossCityNeverClaim(t)
		var stdout, stderr bytes.Buffer
		claimHookWorkWithRunner("bd ready --json", "rig", stores[0].env, stores, crossCityClaimOptions(), ops, run, func(string, error) {}, &stdout, &stderr)
		if result := decodeCrossCityResult(t, &stdout); result.Action != "drain" {
			t.Fatalf("want a drain with no claim anywhere, got %+v\nstderr: %s", result, stderr.String())
		}
		assertCrossCityRefusalLogged(t, &stderr, beadID, foreignOwner, "claim")
	})

	t.Run("adoption: own in_progress work held by the city leg is resolved there and served", func(t *testing.T) {
		run := func(_, _ string, _ []string) (string, error) {
			return `[` + crossCityBlindRow(beadID, "in_progress", crossCityTestIdentity) + `]`, nil
		}
		reads := map[string]int{}
		ops := quietOps()
		ops.ReadWorkMeta = legReads(crossCityStoreBead(beadID, "in_progress", crossCityTestIdentity, crossCityOwnerLabel(crossCityThisCity)), reads)
		ops.Claim = crossCityNeverClaim(t)
		var stdout, stderr bytes.Buffer
		code := claimHookWorkWithRunner("bd ready --json", "rig", stores[0].env, stores, crossCityClaimOptions(), ops, run, func(string, error) {}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("claimHookWorkWithRunner = %d, want 0; stderr=%s", code, stderr.String())
		}
		if result := decodeCrossCityResult(t, &stdout); result.Action != "work" || result.Reason != "existing_assignment" || result.BeadID != beadID {
			t.Fatalf("own in_progress work held in another leg must still be adopted once that leg vouches for it: got %+v\nstderr: %s", result, stderr.String())
		}
		if reads["city"] == 0 {
			t.Fatalf("adoption never consulted the leg that holds the bead: reads=%v", reads)
		}
	})

	t.Run("adoption: resolved through a leg the query loop never visits (scoped stores, unscoped WorkLegs)", func(t *testing.T) {
		// cmdHookWithOptions pins the city-wide read to the primary
		// (scopeFederatedHookStores), so the claim loop runs on ONE store while
		// the fan-out still has two legs: claimHookWork hands the unscoped list
		// to ops.WorkLegs. Only the primary is queried here; the city leg is
		// reachable solely through WorkLegs, and it is the one that vouches.
		run := func(_, dir string, _ []string) (string, error) {
			if dir != "rig" {
				t.Fatalf("work query ran against %q; only the primary is scoped for the query", dir)
			}
			return `[` + crossCityBlindRow(beadID, "in_progress", crossCityTestIdentity) + `]`, nil
		}
		reads := map[string]int{}
		ops := quietOps()
		ops.WorkLegs = stores
		ops.ReadWorkMeta = legReads(crossCityStoreBead(beadID, "in_progress", crossCityTestIdentity, crossCityOwnerLabel(crossCityThisCity)), reads)
		ops.Claim = crossCityNeverClaim(t)
		var stdout, stderr bytes.Buffer
		code := claimHookWorkWithRunner("bd ready --json", "rig", stores[0].env, stores[:1], crossCityClaimOptions(), ops, run, func(string, error) {}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("claimHookWorkWithRunner = %d, want 0; stderr=%s", code, stderr.String())
		}
		if result := decodeCrossCityResult(t, &stdout); result.Action != "work" || result.Reason != "existing_assignment" {
			t.Fatalf("own work held by an unqueried leg must be adopted once that leg vouches: %+v\nstderr: %s", result, stderr.String())
		}
		if reads["city"] == 0 {
			t.Fatalf("the unscoped city leg was never consulted: reads=%v", reads)
		}
	})

	t.Run("adoption: an intervening leg's read error fails closed and later legs are not trusted", func(t *testing.T) {
		three := []hookStore{stores[0], {dir: "broken", env: []string{"GC_STORE=broken"}}, stores[1]}
		run := func(_, _ string, _ []string) (string, error) {
			return `[` + crossCityBlindRow(beadID, "in_progress", crossCityTestIdentity) + `]`, nil
		}
		reads := map[string]int{}
		ops := quietOps()
		ops.ReadWorkMeta = func(_ context.Context, dir string, _ []string, id, _ string) (beads.Bead, error) {
			reads[dir]++
			switch dir {
			case "broken":
				return beads.Bead{}, errors.New("store contention")
			case "city":
				return crossCityStoreBead(beadID, "in_progress", crossCityTestIdentity, crossCityOwnerLabel(crossCityThisCity)), nil
			}
			return beads.Bead{}, beads.ErrNotFound
		}
		ops.Claim = crossCityNeverClaim(t)
		var stdout, stderr bytes.Buffer
		code := claimHookWorkWithRunner("bd ready --json", "rig", three[0].env, three, crossCityClaimOptions(), ops, run, func(string, error) {}, &stdout, &stderr)
		if code != 1 || stdout.Len() != 0 {
			t.Fatalf("want exit 1 with nothing served when a leg cannot be read, got code=%d stdout=%q\nstderr: %s", code, stdout.String(), stderr.String())
		}
		if reads["city"] != 0 {
			t.Fatalf("a leg after the broken one was consulted (%v); an unreadable leg leaves ownership unresolved", reads)
		}
		if !strings.Contains(stderr.String(), "could not verify the bead") {
			t.Fatalf("stderr must say why:\n%s", stderr.String())
		}
	})

	t.Run("adoption: no leg holds the bead, so nothing is served", func(t *testing.T) {
		run := func(_, _ string, _ []string) (string, error) {
			return `[` + crossCityBlindRow(beadID, "in_progress", crossCityTestIdentity) + `]`, nil
		}
		ops := quietOps()
		ops.ReadWorkMeta = func(context.Context, string, []string, string, string) (beads.Bead, error) {
			return beads.Bead{}, beads.ErrNotFound
		}
		ops.Claim = crossCityNeverClaim(t)
		var stdout, stderr bytes.Buffer
		code := claimHookWorkWithRunner("bd ready --json", "rig", stores[0].env, stores, crossCityClaimOptions(), ops, run, func(string, error) {}, &stdout, &stderr)
		if code != 1 || stdout.Len() != 0 {
			t.Fatalf("want exit 1 with nothing served for a row no leg holds, got code=%d stdout=%q\nstderr: %s", code, stdout.String(), stderr.String())
		}
	})

	t.Run("adoption: the city leg's copy was relabeled foreign, so the row is not served", func(t *testing.T) {
		run := func(_, _ string, _ []string) (string, error) {
			return `[` + crossCityRow(beadID, "in_progress", crossCityTestIdentity, crossCityOwnerLabel(crossCityThisCity)) + `]`, nil
		}
		ops := quietOps()
		ops.ReadWorkMeta = legReads(crossCityStoreBead(beadID, "in_progress", crossCityTestIdentity, foreignOwner), map[string]int{})
		ops.Claim = crossCityNeverClaim(t)
		var stdout, stderr bytes.Buffer
		claimHookWorkWithRunner("bd ready --json", "rig", stores[0].env, stores, crossCityClaimOptions(), ops, run, func(string, error) {}, &stdout, &stderr)
		if result := decodeCrossCityResult(t, &stdout); result.Action == "work" {
			t.Fatalf("REGRESSION jg-66rdw8: a stale local-looking row was served while the holding leg says foreign: %+v", result)
		}
		assertCrossCityRefusalLogged(t, &stderr, beadID, foreignOwner, "adoption")
	})
}
