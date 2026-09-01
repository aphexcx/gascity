// Package federation holds the conventions shared by cities that federate a
// bead store — one ledger pushed and pulled (dolt) between two or more cities
// whose pool workers all claim from the same ready queue.
//
// The first convention is the per-city owner label. Every bead created in a
// federated store carries owner:<identity>, where identity is the creating
// city's [federation] identity from city.toml. Each city's claim path refuses
// beads whose owner names another city (unless the bead also carries
// handoff:<this-city>, set by an explicit cross-city sling), and a bead with
// no owner label — legacy work — stays claimable by anyone. Without the label
// a city-local bead sits in the shared ready queue looking like local work to
// every city, and the reconciler of the wrong city re-claims it every lease
// expiry (the hw-57b63 loop of 2026-09-01).
//
// This package is the one place the label's spelling lives: the `gc bd create`
// argv injector, the in-process create doors and the legacy backfill all stamp
// through it. It imports nothing but the standard library so both the config
// and the beads packages can use it.
package federation

import (
	"fmt"
	"regexp"
	"strings"
)

// OwnerLabelPrefix is the label namespace that names the city owning a bead.
const OwnerLabelPrefix = "owner:"

// HandoffLabelPrefix is the label namespace an explicit cross-city sling
// writes to let the named city claim a bead another city owns. The refusal
// side (the claim path) reads it; the emit side never writes it implicitly.
const HandoffLabelPrefix = "handoff:"

// identityRE is the shape a federation identity must have. It is spliced into
// a label and compared byte-for-byte across cities, so it is lower-case ASCII
// with no whitespace and no leading dash.
var identityRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidateIdentity reports whether identity is a well-formed federation
// identity. The empty string is valid: it means "not federated".
func ValidateIdentity(identity string) error {
	if identity == "" {
		return nil
	}
	if !identityRE.MatchString(identity) {
		return fmt.Errorf("federation.identity %q must match %s", identity, identityRE.String())
	}
	return nil
}

// OwnerLabel returns the owner label for identity, or ("", false) when the
// identity is unset — the non-federated case, in which nothing is stamped.
func OwnerLabel(identity string) (string, bool) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return "", false
	}
	return OwnerLabelPrefix + identity, true
}

// HasOwnerLabel reports whether labels carry any owner label, whichever city
// it names.
func HasOwnerLabel(labels []string) bool {
	for _, l := range labels {
		if strings.HasPrefix(l, OwnerLabelPrefix) {
			return true
		}
	}
	return false
}

// OwnerOf returns the identity named by the first owner label, or "" when the
// bead carries none.
func OwnerOf(labels []string) string {
	for _, l := range labels {
		if owner, ok := strings.CutPrefix(l, OwnerLabelPrefix); ok {
			return owner
		}
	}
	return ""
}

// EnsureOwnerLabel returns labels with owner (a full "owner:<identity>" label)
// appended when labels carry no owner label yet. An explicit owner — this
// city's or another's — is kept exactly as authored, so a deliberate
// cross-city create is never overwritten. The caller's order is preserved and
// the input slice is never mutated.
func EnsureOwnerLabel(labels []string, owner string) []string {
	if owner == "" || HasOwnerLabel(labels) {
		return labels
	}
	out := make([]string, 0, len(labels)+1)
	out = append(out, labels...)
	return append(out, owner)
}

// HandoffLabel returns the handoff:<identity> label that lets identity claim
// a bead owned by another city.
func HandoffLabel(identity string) string {
	return HandoffLabelPrefix + strings.TrimSpace(identity)
}

// HandoffTargets returns the identities named by handoff labels, in label
// order, or nil when the bead carries none.
func HandoffTargets(labels []string) []string {
	var targets []string
	for _, l := range labels {
		if target, ok := strings.CutPrefix(l, HandoffLabelPrefix); ok {
			targets = append(targets, target)
		}
	}
	return targets
}

// HasHandoffTo reports whether labels carry handoff:<identity> for exactly
// that identity. An empty identity never matches.
func HasHandoffTo(labels []string, identity string) bool {
	if identity == "" {
		return false
	}
	for _, target := range HandoffTargets(labels) {
		if target == identity {
			return true
		}
	}
	return false
}

// OwnerLabelForChild is the one rule for a create that names a parent. An
// owner already on the child wins. Otherwise the child stays in its parent's
// lane: bd copies a parent's labels onto a child by default, so a child of
// another city's bead would otherwise end up with two owners, and on a
// backend that does not copy labels it would end up in the wrong lane —
// either way the parent's owner is the child's. Only a child of an unowned
// parent (or with no parent) is the creating city's. With an empty owner
// (not federated) the inherited owner still applies and nothing else does.
func OwnerLabelForChild(labels, parentLabels []string, owner string) []string {
	if HasOwnerLabel(labels) {
		return labels
	}
	if inherited := OwnerOf(parentLabels); inherited != "" {
		return EnsureOwnerLabel(labels, OwnerLabelPrefix+inherited)
	}
	return EnsureOwnerLabel(labels, owner)
}

// PlanNode is one node of a bulk create for OwnerLabelsForPlan: its authored
// labels, the index of its in-plan parent (or -1), and the labels of the
// existing bead it names as parent (nil when it names none or it could not be
// read).
type PlanNode struct {
	Labels       []string
	ParentIndex  int
	ParentLabels []string
}

// OwnerLabelsForPlan applies OwnerLabelForChild to every node of a bulk
// create, in dependency order: a node under an in-plan parent takes that
// parent's EFFECTIVE labels (after its own resolution), so a whole subtree
// created under another city's bead stays in that city's lane. A cycle or a
// bad parent index resolves as "no in-plan parent".
func OwnerLabelsForPlan(nodes []PlanNode, owner string) [][]string {
	out := make([][]string, len(nodes))
	done := make([]bool, len(nodes))
	visiting := make([]bool, len(nodes))
	var resolve func(i int) []string
	resolve = func(i int) []string {
		if done[i] {
			return out[i]
		}
		if visiting[i] {
			return nodes[i].Labels // cycle: fall back to the authored labels
		}
		visiting[i] = true
		parentLabels := nodes[i].ParentLabels
		if p := nodes[i].ParentIndex; p >= 0 && p < len(nodes) && p != i {
			parentLabels = resolve(p)
		}
		out[i] = OwnerLabelForChild(nodes[i].Labels, parentLabels, owner)
		visiting[i] = false
		done[i] = true
		return out[i]
	}
	for i := range nodes {
		resolve(i)
	}
	return out
}
