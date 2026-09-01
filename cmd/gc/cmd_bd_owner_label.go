package main

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/federation"
)

// bdNoOwnerLabelFlag is the gc-owned opt-out on `gc bd create`: leave this
// one create unlabeled. It never reaches bd.
const bdNoOwnerLabelFlag = "--no-owner-label"

// The stderr lines an operator sees when a federated city's create ran but
// was not labeled. The create itself is untouched either way.
const (
	// bdOwnerLabelNotAppliedNotice: gc could not find a created bead in bd's
	// output.
	bdOwnerLabelNotAppliedNotice = "gc bd: owner label not applied (could not read the created bead id from bd's output)"
	// bdOwnerLabelNotAppliedOnFailureNotice: bd exited non-zero and named no
	// created bead. A batch can persist beads before failing, in a shape gc
	// does not read, so the operator is told rather than left to assume
	// nothing was created.
	bdOwnerLabelNotAppliedOnFailureNotice = "gc bd: owner label not applied (bd exited non-zero and named no created bead in its output; if it created any, label each with gc bd update <id> --add-label owner:<identity>)"
	// bdOwnerLabelHiddenVerbNotice: a leading flag neither global manifest
	// knows hides the verb, so gc cannot tell a create from a read and must
	// not mutate what a read printed.
	bdOwnerLabelHiddenVerbNotice = "gc bd: owner label not applied (a flag gc does not know hides the verb; if this was a create, label the bead with gc bd update <id> --add-label owner:<identity>)"
)

// The argv door of the federation owner label (internal/federation) works
// AFTER the create, on the bead bd says it created:
//
//  1. The argv reaches bd exactly as typed, minus the gc-owned opt-out. gc
//     does not parse bd's create surface — every alias, inline boolean,
//     batch mode, inherited label and flag bd adds tomorrow is bd's own
//     business — so no create that worked yesterday can break, and nothing
//     gc fails to understand can leave a bead silently unlabeled.
//  2. bd's stdout is teed and the ids it reports as created are read out of
//     it (its single-create line, its --json object, its --silent bare id,
//     its --graph and --file listings).
//  3. Each created bead is read back through the store and labeled
//     owner:<identity> unless it already carries an owner — its own, or one
//     bd copied from its parent. The bead's stored labels are the operand no
//     argv variant can move.
//
// A create whose output names no bead (a format gc does not know) is said
// out loud, once, on stderr, whatever bd's exit code — a failed batch may have
// persisted beads before it failed; a --dry-run creates nothing and is left
// alone.

// bdFlagTokens walks the flag tokens of a create argv before a `--`
// terminator, skipping the VALUE of every value flag bd's create surface and
// its globals know (`--description --dry-run` is a description, not a dry
// run). A token is visited as (index, name, value, inline).
func bdFlagTokens(bdArgs []string, visit func(i int, name, value string, inline bool)) {
	valueFlags := bdflags.ValueFlags("create")
	for i := 0; i < len(bdArgs); i++ {
		tok := bdArgs[i]
		if tok == "--" {
			return
		}
		if !strings.HasPrefix(tok, "-") || tok == "-" {
			continue
		}
		name, value, inline := strings.Cut(tok, "=")
		visit(i, name, value, inline)
		if !inline && valueFlags[name] {
			i++ // its value is not a flag
		}
	}
}

// stripBdNoOwnerLabel removes the gc-owned opt-out in every boolean spelling
// bd's flag parser accepts (bare, =true, =false, =1, =0 …) and reports whether
// the operator opted out — which `=false` does not. A value it cannot parse is
// left in place for bd to reject, never read as an opt-out.
func stripBdNoOwnerLabel(bdArgs []string) ([]string, bool) {
	drop := make(map[int]bool)
	optOut := false
	bdFlagTokens(bdArgs, func(i int, name, value string, inline bool) {
		if name != bdNoOwnerLabelFlag {
			return
		}
		if !inline {
			drop[i], optOut = true, true
			return
		}
		if v, err := strconv.ParseBool(value); err == nil {
			drop[i] = true
			optOut = optOut || v
		}
	})
	if len(drop) == 0 {
		return bdArgs, false
	}
	out := make([]string, 0, len(bdArgs))
	for i, tok := range bdArgs {
		if !drop[i] {
			out = append(out, tok)
		}
	}
	return out, optOut
}

// bdVerb is what gc can tell about the verb of a bd argv.
type bdVerb int

const (
	bdVerbOther  bdVerb = iota // a verb that is not create
	bdVerbCreate               // create, or its alias new
	bdVerbHidden               // a leading flag neither global manifest knows hides the verb
)

// bdCreateVerdict locates the verb the way bd will — after the global flags
// and their values — and reports create/new, another verb, or that the verb
// cannot be located because a leading flag is one gc does not know. A global
// flag's VALUE is never the verb: `--actor create show x` is a show, and
// stamping what it printed would mutate a read.
func bdCreateVerdict(bdArgs []string) bdVerb {
	verb, hidden := bdVerbAfterGlobals(bdArgs)
	if hidden {
		return bdVerbHidden
	}
	switch verb {
	case "create", "new":
		return bdVerbCreate
	}
	return bdVerbOther
}

// bdVerbAfterGlobals walks the flags before the verb the way bd does — cobra's
// subcommand search decides which token is the verb, pflag's shorthand rules
// decide what each flag token means — and returns the verb, or hidden when a
// leading flag is one neither global manifest knows: bd would read a value gc
// cannot see (`--future-flag x create t` may be a create or an x), so the verb
// is undecidable from this argv.
//
// cobra's search skips a `--flag` or a lone `-f` together with the next token
// when the flag takes a value, skips every other dash token on its own, and
// takes the first remaining token as the verb. pflag then reads a dash token
// as a long flag, or as a cluster of shorthands in which each letter is a
// boolean or a value flag that takes the rest of the token (`-C/path`,
// `-C=/path`), an `=` after a boolean ending the cluster (`-v=false`). The
// one place the two disagree is a cluster whose LAST letter takes a value with
// nothing attached (`-vC dir`): pflag would take dir, cobra reads it as the
// verb, and bd runs what cobra found.
func bdVerbAfterGlobals(bdArgs []string) (verb string, hidden bool) {
	valueFlags := bdflags.GlobalValueFlags()
	boolFlags := bdflags.GlobalBoolFlags()
	for i := 0; i < len(bdArgs); i++ {
		tok := bdArgs[i]
		switch {
		case tok == "--":
			return "", false // bd's flags end here; nothing after is a verb
		case tok == "-":
			continue // skipped by the search, a positional to the verb's parser
		case !strings.HasPrefix(tok, "-"):
			return tok, false
		case strings.HasPrefix(tok, "--"):
			name, _, inline := strings.Cut(tok, "=")
			switch {
			case boolFlags[name]:
			case valueFlags[name]:
				if !inline {
					i++
				}
			default:
				return "", true
			}
		default:
			for j := 1; j < len(tok); j++ {
				short := "-" + tok[j:j+1]
				switch {
				case boolFlags[short] && j+1 < len(tok) && tok[j+1] == '=':
					j = len(tok) // `-v=false`: an inline value ends the cluster
				case boolFlags[short]:
				case valueFlags[short] && j+1 < len(tok):
					j = len(tok) // `-C/path`, `-C=/path`: the rest is its value
				case valueFlags[short] && len(tok) == 2:
					i++ // `-C path`: the next token is its value
				case valueFlags[short]:
					// `-vC path`: cobra reads path as the verb (see above)
				default:
					return "", true
				}
			}
		}
	}
	return "", false
}

// bdHasBoolFlag reports whether a boolean flag is set before a `--`
// terminator, honoring an inline value and skipping value flags' values.
func bdHasBoolFlag(bdArgs []string, flag string) bool {
	set := false
	bdFlagTokens(bdArgs, func(_ int, name, value string, inline bool) {
		if name != flag || set {
			return
		}
		if !inline {
			set = true
			return
		}
		if v, err := strconv.ParseBool(value); err != nil || v {
			set = true
		}
	})
	return set
}

// bdBeadIDRE is the shape of a bead id — <prefix>-<suffix>, with an optional
// class or wisp segment, and bd's dotted hierarchical children (ci-root.1);
// anything else in bd's output is not an id.
var bdBeadIDRE = regexp.MustCompile(`^[A-Za-z0-9]+(?:[-.][A-Za-z0-9]+)+$`)

var (
	// bdCreatedIssueRE matches bd's single-create success line:
	//   ✓ Created issue: <id> — <title>
	bdCreatedIssueRE = regexp.MustCompile(`Created issue: (\S+)`)
	// bdGraphCreatedRE matches one line of `bd create --graph`'s text listing
	// after its "Created N issues" header:   <key> -> <id>
	bdGraphCreatedRE = regexp.MustCompile(`^\s+\S+ -> (\S+)\s*$`)
	// bdMarkdownCreatedRE matches one line of `bd create --file`'s text
	// listing after its "Created N issues from <file>:" header:
	//   <id>: <title> [P<n>, <type>]
	bdMarkdownCreatedRE = regexp.MustCompile(`^\s+(\S+): .*\[P\d+, [^\]]+\]\s*$`)
)

// bdCreatedIDs reads the ids bd reports as created out of its stdout, in
// bd 1.2.2's own shapes: the single-create line, the --json object (or the
// --file array of objects, or the --graph {"ids":{key:id}} object), the
// --silent bare id, and the --graph / --file text listings. A dry run
// previews with an empty id and creates nothing; it yields none.
func bdCreatedIDs(out string) []string {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil
	}
	var ids []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id != "" && bdBeadIDRE.MatchString(id) && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var single struct {
			ID  string            `json:"id"`
			IDs map[string]string `json:"ids"`
		}
		if err := json.Unmarshal([]byte(trimmed), &single); err == nil {
			add(single.ID)
			keys := make([]string, 0, len(single.IDs))
			for k := range single.IDs {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				add(single.IDs[k])
			}
		}
		var many []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(trimmed), &many); err == nil {
			for _, m := range many {
				add(m.ID)
			}
		}
		return ids
	}
	inGraphListing, inMarkdownListing := false, false
	for _, line := range strings.Split(out, "\n") {
		if m := bdCreatedIssueRE.FindStringSubmatch(line); m != nil {
			add(m[1])
			continue
		}
		switch {
		case strings.Contains(line, "Created ") && strings.Contains(line, " issues from "):
			inMarkdownListing, inGraphListing = true, false
			continue
		case strings.HasPrefix(strings.TrimSpace(line), "Created ") && strings.Contains(line, " issues"):
			inGraphListing, inMarkdownListing = true, false
			continue
		}
		if inGraphListing {
			if m := bdGraphCreatedRE.FindStringSubmatch(line); m != nil {
				add(m[1])
				continue
			}
		}
		if inMarkdownListing {
			if m := bdMarkdownCreatedRE.FindStringSubmatch(line); m != nil {
				add(m[1])
				continue
			}
		}
		// --silent prints the bare id and nothing else.
		if bare := strings.TrimSpace(line); bare == line && bdBeadIDRE.MatchString(bare) {
			add(bare)
		}
	}
	return ids
}

// stampCreatedBeads labels each created bead owner unless it already carries
// an owner (its own, or one bd copied from its parent), reading each one back
// through the store first. A bead that came out with TWO owners — one named
// on the create, one bd copied from a parent owned by another city — keeps
// the one it was given: the parent's is removed. Every failure is named on
// stderr; the create has already happened, so nothing here changes the exit
// code.
func stampCreatedBeads(store beads.Store, ids []string, owner string, stderr io.Writer) (stamped, kept, failed int) {
	for _, id := range ids {
		bead, err := store.Get(id)
		if err != nil {
			failed++
			fmt.Fprintf(stderr, "gc bd: owner label not applied to %s: reading it back: %v\n", id, err) //nolint:errcheck // best-effort stderr
			continue
		}
		owners := federation.OwnerLabels(bead.Labels)
		switch {
		case len(owners) == 0:
			if err := store.Update(id, beads.UpdateOpts{Labels: []string{owner}}); err != nil {
				failed++
				fmt.Fprintf(stderr, "gc bd: owner label not applied to %s: %v\n", id, err) //nolint:errcheck // best-effort stderr
				continue
			}
			stamped++
		case len(owners) == 1:
			kept++
		default:
			inherited := bdInheritedOwnerLabel(store, bead)
			if inherited == "" || !slices.Contains(owners, inherited) || len(owners) != 2 {
				failed++
				fmt.Fprintf(stderr, "gc bd: %s carries %d owner labels (%s); keep one with gc bd update %s --remove-label <owner:…>\n", id, len(owners), strings.Join(owners, ", "), id) //nolint:errcheck // best-effort stderr
				continue
			}
			if err := store.Update(id, beads.UpdateOpts{RemoveLabels: []string{inherited}}); err != nil {
				failed++
				fmt.Fprintf(stderr, "gc bd: %s carries its own owner and its parent's %s; removing the parent's failed: %v\n", id, inherited, err) //nolint:errcheck // best-effort stderr
				continue
			}
			kept++
		}
	}
	return stamped, kept, failed
}

// bdInheritedOwnerLabel returns the owner label a bead's parent carries — the
// one bd copied onto the child — or "" when the bead has no readable parent
// or the parent has no owner.
func bdInheritedOwnerLabel(store beads.Store, bead beads.Bead) string {
	parentID := strings.TrimSpace(bead.ParentID)
	if parentID == "" {
		return ""
	}
	parent, err := store.Get(parentID)
	if err != nil {
		return ""
	}
	if owner := federation.OwnerOf(parent.Labels); owner != "" {
		return federation.OwnerLabelPrefix + owner
	}
	return ""
}
