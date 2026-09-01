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
