package federation

import (
	"strconv"
	"strings"
	"unicode"
)

// MayClaim is the refusal side of the owner-label convention: it reports
// whether a city whose [federation] identity is thisIdentity may take
// ownership of a bead carrying labels — claim it, adopt it as its own
// in-progress work, preassign it, reopen it, or re-home it onto one of its
// sessions — and, when it may not, why. It is the ONE rule every refusing
// call site applies (gc hook --claim, which is also the pool worker's startup
// claim; the reconciler's orphan release, which is the re-route that drove the
// hw-57b63 loop; the retired-session re-home in the reconciler and in the
// API's named-session continuity), so no two sites can disagree about whose
// bead it is.
//
// The rule, in order:
//
//   - thisIdentity empty (after trimming) → ok. The city is not federated, the
//     fence is off, everything is claimable; nothing is logged.
//   - no owner:* label → ok. Legacy work, claimable by anyone.
//   - any handoff:<thisIdentity> label → ok. The explicit cross-city sling; a
//     bead may carry several handoff:* labels and one naming this city is
//     enough.
//   - every owner:* label names thisIdentity → ok. This city's own work.
//   - otherwise → refused: an owner label names another city, or a bead
//     carries owner labels for two cities (a conflict, not a licence — the
//     handoff label is the one sanctioned override), or an owner label has no
//     value at all.
//
// Labels are compared as exact strings, no trimming and no case folding: an
// identity is validated at config load against ^[a-z0-9][a-z0-9-]*$ so the
// value is lowercase by construction, and the emit side (OwnerLabel,
// EnsureOwnerLabel) spells the label from it verbatim. "Owner:Citadel" is
// therefore not an owner label at all, exactly as HasOwnerLabel and OwnerOf
// read it.
//
// reason is empty when ok, and otherwise one greppable run of key=value pairs
// naming the owner value(s) as the bead spells them, this identity, and the
// handoff label that would have permitted the claim — ClaimRefusalLine puts
// the bead id in front of it. Values that are not a single printable token
// are Go-quoted so a label cannot break a log line or forge a second key.
func MayClaim(labels []string, thisIdentity string) (ok bool, reason string) {
	thisIdentity = strings.TrimSpace(thisIdentity)
	if thisIdentity == "" {
		return true, ""
	}
	owners := Owners(labels)
	if len(owners) == 0 || HasHandoffTo(labels, thisIdentity) {
		return true, ""
	}
	foreign := false
	for _, owner := range owners {
		if owner != thisIdentity {
			foreign = true
			break
		}
	}
	if !foreign {
		return true, ""
	}
	quoted := make([]string, 0, len(owners))
	for _, owner := range owners {
		quoted = append(quoted, logToken(owner))
	}
	return false, "owner=" + strings.Join(quoted, ",") +
		" this_identity=" + logToken(thisIdentity) +
		" missing=" + logToken(HandoffLabel(thisIdentity))
}

// Owners returns the identity named by every owner label, in label order —
// including an empty identity for a malformed bare "owner:" — or nil when the
// bead carries none. OwnerOf reads the first; MayClaim needs them all, because
// a bead that names two owners is refused, not resolved by position.
func Owners(labels []string) []string {
	var owners []string
	for _, l := range labels {
		if owner, ok := strings.CutPrefix(l, OwnerLabelPrefix); ok {
			owners = append(owners, owner)
		}
	}
	return owners
}

// ClaimRefusalLine is the one line every refusing call site logs, so an
// incident responder greps one string — "cross-city-fence refused" — across
// the hook's stderr, the reconciler's log and the API server's log and gets
// the same facts in the same order: bead id, owner value(s), this identity, the missing handoff
// label. Each caller prefixes it with its own logger's context and may append
// its own detail after it.
func ClaimRefusalLine(beadID, reason string) string {
	return "cross-city-fence refused bead=" + logToken(strings.TrimSpace(beadID)) + " " + reason
}

// logToken renders s as one value of a key=value log line: bare when it is a
// single printable token, Go-quoted when it is empty or holds whitespace, a
// quote, a backslash, or any character strconv.Quote would escape (control
// characters, invalid UTF-8). Every data value the fence logs — a label value,
// the identity, a bead id — goes through it, so data written by whoever labels
// or names things cannot break the one-line contract or forge a second key.
func logToken(s string) string {
	quoted := strconv.Quote(s)
	if s != "" && quoted == `"`+s+`"` && strings.IndexFunc(s, unicode.IsSpace) < 0 {
		return s
	}
	return quoted
}
