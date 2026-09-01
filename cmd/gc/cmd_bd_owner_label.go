package main

import (
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/federation"
)

// bdNoOwnerLabelFlag is the gc-owned opt-out on `gc bd create`: skip the
// owner:<identity> stamp for this one create. It never reaches bd.
const bdNoOwnerLabelFlag = "--no-owner-label"

// The one stderr line an operator sees when a federated city's `gc bd create`
// was not stamped. Either way the create runs exactly as typed (minus the
// gc-owned opt-out).
const (
	// bdOwnerLabelNotInjectedNotice: the argv could not be scanned.
	bdOwnerLabelNotInjectedNotice = "gc bd: owner label not injected (ambiguous argv)"
	// bdOwnerLabelBatchNotice: --graph/--file/--stdin creates carry their
	// fields in the file or stream, where bd never reads --labels.
	bdOwnerLabelBatchNotice = "gc bd: owner label not injected (batch create: --graph/--file/--stdin supply their own fields; add owner:<identity> to each item)"
)

// rewriteBdCreateOwnerLabel is the argv door of the federation owner label
// (internal/federation). It returns the argv bd must receive — ALWAYS that,
// on every path, which is why the caller has no branch — and a notice for the
// operator, empty when there is nothing to say. On a `create` it appends
// `--labels <owner>` unless the operator already named an owner via
// -l/--labels/--label or opted out with --no-owner-label, and it strips the
// gc-owned opt-out before bd sees it. owner is the full "owner:<identity>"
// label, or "" on a non-federated city, where only the strip happens, the
// notice is always empty, and every other argv is returned as it came.
//
// It scans fail-closed, like bdMutationWriteIDs: a flag the create manifest
// does not know may consume the next token, so the label could land as a
// positional and become the title — and a global flag the manifest does not
// know may hide the verb itself. Such an argv is forwarded as it came, minus
// the gc-owned opt-out (which bd would reject), with the ambiguous-argv
// notice — not refused, not stamped — because a create that worked yesterday
// must keep working today, and bd is the one to reject a flag it does not
// take. A batch create (--graph, --file, --stdin) is forwarded the same way
// with its own notice: its fields live in the file or stream, where bd never
// reads --labels, so a stamp there would be silently dropped. The stamp lands
// before a `--` terminator so it is read as a flag, never as a positional.
func rewriteBdCreateOwnerLabel(bdArgs []string, owner string) ([]string, string) {
	notice := func(text string) string {
		if owner == "" {
			return ""
		}
		return text
	}
	if bdLeadingFlagsHideTheVerb(bdArgs) {
		// The verb cannot be located, so neither can a create be stamped nor
		// its absence trusted; say so only when a create may be in there.
		if slices.Contains(bdArgs, "create") {
			return stripBdNoOwnerLabel(bdArgs), notice(bdOwnerLabelNotInjectedNotice)
		}
		return stripBdNoOwnerLabel(bdArgs), ""
	}
	sub, rest := bdflags.SplitGlobalFlags(bdArgs)
	if sub != "create" {
		return bdArgs, ""
	}
	head := bdArgs[:len(bdArgs)-len(rest)] // the global flags and the verb
	valueFlags := bdflags.ValueFlags("create")
	boolFlags := bdflags.BoolFlags("create")
	gcFlags := bdflags.GCOwnedBoolFlags("create")

	out := make([]string, 0, len(bdArgs)+2)
	out = append(out, head...)
	optOut, hasOwner, batch := false, false, false
	terminator := -1 // index in out of the "--" token, when present
	for i := 0; i < len(rest); i++ {
		tok := rest[i]
		switch {
		case terminator >= 0:
			out = append(out, tok)
			continue
		case tok == "--":
			terminator = len(out)
			out = append(out, tok)
			continue
		case !strings.HasPrefix(tok, "-") || tok == "-":
			out = append(out, tok)
			continue
		}
		name, value, inline := strings.Cut(tok, "=")
		switch {
		case gcFlags[name]:
			if name == bdNoOwnerLabelFlag {
				optOut = true
			}
			continue // gc-owned: consumed here, never forwarded
		case boolFlags[name]:
			out = append(out, tok)
			if bdCreateBatchFlag(name) {
				batch = true
			}
		case valueFlags[name]:
			if !inline {
				if i+1 >= len(rest) {
					return stripBdNoOwnerLabel(bdArgs), notice(bdOwnerLabelNotInjectedNotice) // a value flag with no value: bd's error to give
				}
				i++
				value = rest[i]
				out = append(out, tok, value)
			} else {
				out = append(out, tok)
			}
			if bdCreateLabelFlag(name) && bdLabelValueNamesOwner(value) {
				hasOwner = true
			}
			if bdCreateBatchFlag(name) {
				batch = true
			}
		default:
			return stripBdNoOwnerLabel(bdArgs), notice(bdOwnerLabelNotInjectedNotice)
		}
	}
	if batch {
		return out, notice(bdOwnerLabelBatchNotice)
	}
	if optOut || hasOwner || owner == "" {
		return out, ""
	}
	stamp := []string{"--labels", owner}
	if terminator >= 0 {
		return slices.Insert(out, terminator, stamp...), ""
	}
	return append(out, stamp...), ""
}

// bdCreateBatchFlag reports whether name selects one of bd create's batch
// modes, whose per-item fields come from a file or stream: --graph <plan>,
// --file/-f <markdown>, --stdin.
func bdCreateBatchFlag(name string) bool {
	switch name {
	case "--graph", "--file", "-f", "--stdin":
		return true
	}
	return false
}

// bdLeadingFlagsHideTheVerb reports whether a flag before the verb is one
// neither global manifest knows. SplitGlobalFlags skips such a flag as if it
// took no value, so `--future-flag x create t` would read x as the verb: the
// verb is undecidable from this argv.
func bdLeadingFlagsHideTheVerb(bdArgs []string) bool {
	valueFlags := bdflags.GlobalValueFlags()
	boolFlags := bdflags.GlobalBoolFlags()
	for i := 0; i < len(bdArgs); i++ {
		tok := bdArgs[i]
		if !strings.HasPrefix(tok, "-") || tok == "-" || tok == "--" {
			return false // the verb (or a positional) — the leading flags are done
		}
		name, _, inline := strings.Cut(tok, "=")
		switch {
		case boolFlags[name]:
		case valueFlags[name]:
			if !inline {
				i++
			}
		default:
			return true
		}
	}
	return false
}

// stripBdNoOwnerLabel returns bdArgs without the gc-owned opt-out tokens that
// precede a `--` terminator: whatever else happens to the argv, bd must never
// see a flag only gc takes.
func stripBdNoOwnerLabel(bdArgs []string) []string {
	out := make([]string, 0, len(bdArgs))
	seenTerminator := false
	for _, tok := range bdArgs {
		if tok == "--" {
			seenTerminator = true
		}
		if !seenTerminator && tok == bdNoOwnerLabelFlag {
			continue
		}
		out = append(out, tok)
	}
	return out
}

// bdCreateLabelFlag reports whether name is one of bd create's label flags:
// -l, --labels, and the hidden --label alias bd also accepts.
func bdCreateLabelFlag(name string) bool {
	return name == "-l" || name == "--labels" || name == "--label"
}

// bdLabelValueNamesOwner reports whether a label flag's value (bd takes a
// comma-separated list per occurrence) already names an owner.
func bdLabelValueNamesOwner(value string) bool {
	for _, label := range strings.Split(value, ",") {
		if strings.HasPrefix(strings.TrimSpace(label), federation.OwnerLabelPrefix) {
			return true
		}
	}
	return false
}
