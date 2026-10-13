package main

import (
	"bytes"
	"context"
	"encoding/csv"
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

// The argv door of the federation owner label (internal/federation) writes
// the owner INTO the create, and reads back what bd created:
//
//  1. A single create carries `--labels=owner:<identity>` right after the
//     verb (bdCreateOwnerLabel, bdCreateArgvWithLabel); a --graph plan or a
//     --file batch is rewritten into a temporary copy with the owner on every
//     node or issue and bd is pointed at the copy (cmd_bd_owner_label_batch.go).
//     Only what the label needs is read from bd's create surface — the label
//     flags, the parents (--parent and the --deps entries of type
//     parent-child), the batch file, the inheritance switch, the value
//     flags whose values must be skipped — by the generated flag manifests
//     (internal/bdflags); everything else reaches bd as typed, minus the
//     gc-owned opt-out.
//  2. bd's stdout is teed and the ids it reports as created are read out of
//     it (its single-create line, its --json object, its --silent bare id,
//     its --graph and --file listings).
//  3. Each created bead is read back through the store and labeled
//     owner:<identity> only if it came out unowned (a batch file gc could not
//     read, a bd that dropped the flag): the repair, never the door.
//
// A create whose output names no bead (a format gc does not know) and that
// carried nothing is said out loud, once, on stderr, whatever bd's exit code —
// a failed batch may have persisted beads before it failed; a --dry-run
// creates nothing and is left alone.

// bdFlagTokens walks the flag tokens of a create argv before a `--`
// terminator, skipping the VALUE of every value flag bd's create surface and
// its globals know (`--description --dry-run` is a description, not a dry
// run). A token is visited as (index, name, value, inline). A shorthand
// cluster is walked with pflag's rules, the way bd reads it: each boolean
// shorthand is visited on its own (`-qv`), a value shorthand takes the rest
// of the token as its inline value (`-lowner:x`, `-l=owner:x`, `-qlowner:x`)
// or, when nothing is attached, the next token (`-qf issues.md`); a shorthand
// gc does not know ends the walk of that token.
func bdFlagTokens(bdArgs []string, visit func(i int, name, value string, inline bool)) {
	bdCommandFlagTokens(bdArgs, "create", visit)
}

// bdCommandFlagTokens walks flags using the selected command's pflag types.
func bdCommandFlagTokens(bdArgs []string, command string, visit func(i int, name, value string, inline bool)) {
	valueFlags := bdflags.ValueFlags(command)
	if valueFlags == nil {
		valueFlags = bdflags.GlobalValueFlags()
	}
	if command == "create" {
		for flag := range bdCreateHiddenValueFlags {
			valueFlags[flag] = true
		}
	}
	boolFlags := bdflags.BoolFlags(command)
	if boolFlags == nil {
		boolFlags = bdflags.GlobalBoolFlags()
	}
	for i := 0; i < len(bdArgs); i++ {
		tok := bdArgs[i]
		if tok == "--" {
			return
		}
		if !strings.HasPrefix(tok, "-") || tok == "-" {
			continue
		}
		if strings.HasPrefix(tok, "--") {
			name, value, inline := strings.Cut(tok, "=")
			visit(i, name, value, inline)
			if !inline && valueFlags[name] {
				i++ // its value is not a flag
			}
			continue
		}
		for j := 1; j < len(tok); j++ {
			short := "-" + tok[j:j+1]
			switch {
			case valueFlags[short]:
				if rest := strings.TrimPrefix(tok[j+1:], "="); rest != "" || j+1 < len(tok) {
					visit(i, short, rest, true)
				} else {
					visit(i, short, "", false)
					i++ // its value is the next token
				}
				j = len(tok)
			case boolFlags[short]:
				if j+1 < len(tok) && tok[j+1] == '=' {
					visit(i, short, tok[j+2:], true) // `-v=false`: an inline value ends the cluster
					j = len(tok)
				} else {
					visit(i, short, "", false)
				}
			default:
				visit(i, short, "", false)
				j = len(tok) // a shorthand gc does not know: bd's business
			}
		}
	}
}

// stripBdNoOwnerLabel removes the gc-owned opt-out in every boolean spelling
// bd's flag parser accepts (bare, =true, =false, =1, =0 …) and reports whether
// the operator opted out — which `=false` does not. A repeated flag resolves
// as pflag resolves every boolean: the last occurrence wins, so an explicit
// final `=false` restores the label. A value it cannot parse is left in place
// for bd to reject, never read as an opt-out.
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
			drop[i], optOut = true, v
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
// `-C=/path`), a non-empty `=value` after a boolean ending the cluster
// (`-v=false`; a bare `-v=` leaves `=` as a shorthand pflag rejects). The
// one place the two disagree is a cluster whose LAST letter takes a value with
// nothing attached (`-vC dir`): pflag would take dir, cobra reads it as the
// verb, and bd runs what cobra found.
func bdVerbAfterGlobals(bdArgs []string) (verb string, hidden bool) {
	i, hidden := bdVerbIndexAfterGlobals(bdArgs)
	if hidden || i < 0 {
		return "", hidden
	}
	return bdArgs[i], false
}

// bdVerbIndexAfterGlobals is bdVerbAfterGlobals returning the verb's index in
// bdArgs (-1 when the argv names no verb) instead of the verb itself, for the
// caller that has to insert a flag right after it.
func bdVerbIndexAfterGlobals(bdArgs []string) (index int, hidden bool) {
	valueFlags := bdflags.GlobalValueFlags()
	boolFlags := bdflags.GlobalBoolFlags()
	for i := 0; i < len(bdArgs); i++ {
		tok := bdArgs[i]
		switch {
		case tok == "--":
			return -1, false // bd's flags end here; nothing after is a verb
		case tok == "-":
			continue // skipped by the search, a positional to the verb's parser
		case !strings.HasPrefix(tok, "-"):
			return i, false
		case strings.HasPrefix(tok, "--"):
			name, _, inline := strings.Cut(tok, "=")
			switch {
			case boolFlags[name]:
			case valueFlags[name]:
				if !inline {
					i++
				}
			default:
				return -1, true
			}
		default:
			for j := 1; j < len(tok); j++ {
				short := "-" + tok[j:j+1]
				switch {
				case boolFlags[short] && j+1 < len(tok) && tok[j+1] == '=':
					if j+2 == len(tok) {
						return -1, true // `-v=`: pflag reads the bare `=` as a shorthand it does not know
					}
					j = len(tok) // `-v=false`: an inline value ends the cluster
				case boolFlags[short]:
				case valueFlags[short] && j+1 < len(tok):
					j = len(tok) // `-C/path`, `-C=/path`: the rest is its value
				case valueFlags[short] && len(tok) == 2:
					i++ // `-C path`: the next token is its value
				case valueFlags[short]:
					// `-vC path`: cobra reads path as the verb (see above)
				default:
					return -1, true
				}
			}
		}
	}
	return -1, false
}

// The create-time door of the owner label (gf-jr0 round 3, finding 1g; the
// batch half gf-xid finding 1j). The post-create stamp left a window: bd
// published the bead before gc's follow-up label write, and
// federation.MayClaim accepts an unowned bead, so a concurrent city could
// claim a routed bead in that window — and a label written afterwards does
// not undo a claim, while an interrupted gc left the bead unowned for good.
// The owner label now travels IN the create: a single create's own argument
// list, through bd's label flag; a --graph plan's nodes or a --file batch's
// issues, through their per-row labels (bd refuses the flag beside either).
// bd never publishes a bead unowned; the follow-up write stays only as the
// repair for a row that could not carry it or whose bd ignored it.
//
// What the carried label is follows the same rule as the in-process door
// (federation.OwnerLabelForChild): an owner the operator named wins and
// nothing is added; a child of a parent owned by another city carries the
// PARENT's owner, read through the store's live handle before the create (a
// parent that cannot be read refuses the create — an unread parent is not an
// unowned one, finding 1d); any other create carries this city's.

// bdCreateLabelFlags are bd create's label flags, every spelling.
var bdCreateLabelFlags = map[string]bool{"-l": true, "--label": true, "--labels": true}

// bdCreateHiddenValueFlags are bd create's hidden value-taking aliases (the
// description's --body / --message / -m and --description-file), which the
// generated manifest (from `bd create --help`) does not list. Their values
// must be skipped like any other value's: `--body '--parent=x'` is a
// description, not a parent.
var bdCreateHiddenValueFlags = map[string]bool{"--body": true, "--message": true, "-m": true, "--description-file": true}

// bdCreateValueFlag returns the value a create argv gives one value flag,
// inline (`--parent=x`) or as the next token (`--parent x`); "" when the flag
// is absent. The last occurrence wins, as pflag's string flags do.
func bdCreateValueFlag(bdArgs []string, flag string) string {
	value := ""
	bdFlagTokens(bdArgs, func(i int, name, inlineValue string, inline bool) {
		if name != flag {
			return
		}
		switch {
		case inline:
			value = inlineValue
		case i+1 < len(bdArgs):
			value = bdArgs[i+1]
		}
	})
	return value
}

// bdCreateLabelValues returns the labels a create argv names through bd's
// label flags (comma-separated, every spelling), as bd will read them.
func bdCreateLabelValues(bdArgs []string) []string {
	var labels []string
	bdFlagTokens(bdArgs, func(i int, name, value string, inline bool) {
		if !bdCreateLabelFlags[name] {
			return
		}
		if !inline {
			if i+1 >= len(bdArgs) {
				return
			}
			value = bdArgs[i+1]
		}
		for _, l := range bdSliceFlagValues(value) {
			if l = strings.TrimSpace(l); l != "" {
				labels = append(labels, l)
			}
		}
	})
	return labels
}

// bdSliceFlagValues splits one value of a pflag StringSlice flag the way
// pflag does — as a CSV record, so `"owner:x"` is owner:x and a quoted field
// may hold a comma — falling back to a plain split when it is not valid CSV.
func bdSliceFlagValues(value string) []string {
	fields, err := csv.NewReader(strings.NewReader(value)).Read()
	if err != nil {
		return strings.Split(value, ",")
	}
	return fields
}

// bdCreateArgvWithLabel returns bdArgs with `--labels=<label>` inserted right
// after the verb — before any positional, before a `--` terminator — so bd
// reads it as one more label of the create (pflag appends a repeated slice
// flag). An argv whose verb cannot be located is returned unchanged.
func bdCreateArgvWithLabel(bdArgs []string, label string) []string {
	i, hidden := bdVerbIndexAfterGlobals(bdArgs)
	if hidden || i < 0 {
		return bdArgs
	}
	out := make([]string, 0, len(bdArgs)+1)
	out = append(out, bdArgs[:i+1]...)
	out = append(out, "--labels="+label)
	out = append(out, bdArgs[i+1:]...)
	return out
}

// bdCreateOwnerLabel decides the owner label a federated city's create
// carries in its own argument list, and whether it carries one at all:
//
//   - an owner the operator named (any label flag) is respected: nothing is
//     carried, the follow-up read-back keeps today's resolution;
//   - a --graph plan or a --file batch is not this function's: bd refuses the
//     label flag beside either (rejectSingleIssueFlagsForGraph /
//     rejectSingleIssueFlagsForMarkdown in cmd/bd), so their rows carry the
//     owner in the batch file instead (bdCreateBatchWithOwner);
//   - a create under a parent carries the PARENT's owner when the parent has
//     one, read through parentRead — the store's live handle — before
//     anything is created. A parent is every relationship bd's create wires
//     (bdCreateParents): the --parent, and each --deps entry of type
//     parent-child, the latter resolved first through resolveDep — the
//     scoped bd's own resolver, as bd's create resolves it — to the id the
//     live handle reads exactly. A parent that cannot be read refuses the create with
//     that error (finding 1d's rule); parents whose lanes differ name no one
//     lane and refuse the create, as a plan node's do (finding 1l; an
//     unowned parent is in this city's lane); a nil parentRead means the
//     configured store cannot be read for this create (a bd database
//     override), and such a create carries nothing; --no-inherit-labels
//     switches the parents' lane off too, and the child is this city's;
//   - every other create carries this city's owner.
func bdCreateOwnerLabel(parentRead func(id string) (beads.Bead, error), resolveDep func(id string) (string, error), bdArgs []string, owner string) (label string, carry bool, err error) {
	if len(federation.OwnerLabels(bdCreateLabelValues(bdArgs))) > 0 {
		return "", false, nil
	}
	if form, _ := bdCreateBatchFile(bdArgs); form != bdBatchNone {
		return "", false, nil
	}
	parents := bdCreateParents(bdArgs)
	if len(parents) == 0 || bdHasBoolFlag(bdArgs, "--no-inherit-labels") {
		// No parent, or the operator switched inheritance off for this
		// create: the child is this city's, as the follow-up stamp always
		// made it when bd copied nothing.
		return owner, true, nil
	}
	if parentRead == nil {
		return "", false, nil
	}
	thisLane := federation.OwnerOf([]string{owner})
	var first beads.Bead
	lane := ""
	for i, p := range parents {
		parent, err := bdReadCreateParent(parentRead, resolveDep, p)
		if err != nil {
			return "", false, fmt.Errorf("reading parent %q for the owner label: %w", p.ID, err)
		}
		parentLane := federation.OwnerOf(parent.Labels)
		if parentLane == "" {
			parentLane = thisLane
		}
		if i == 0 {
			first, lane = parent, parentLane
			continue
		}
		if parentLane != lane {
			return "", false, fmt.Errorf("the create names parents in different lanes (%q owner %q, %q owner %q): the owner label cannot be resolved", parents[0].ID, lane, p.ID, parentLane)
		}
	}
	owners := federation.OwnerLabels(federation.OwnerLabelForChild(nil, first.Labels, owner))
	if len(owners) != 1 {
		return owner, true, nil
	}
	return owners[0], true, nil
}

// bdDepParentChild is bd's parent-child dependency type
// (internal/types types.DepParentChild), the one spelling of it `bd create
// --deps` accepts: parseDepSpec's alias map (cmd/bd create_deps.go) folds
// depends-on and blocked-by into blocks and passes every other type through
// unchanged.
const bdDepParentChild = "parent-child"

// bdCreateParent is one parent a single create names: its id as typed, and
// whether bd resolves that id before wiring the edge. bd's create takes the
// --parent id literally (cmd/bd create.go: GetNextChildID and addParentEdge
// on the value as given), while every --deps target goes through bd's
// partial-id resolver first (create_deps.go resolveDepSpecTargets →
// utils.ResolvePartialID: the exact id, else a unique leading abbreviation
// of the hash under the configured or a known prefix), so the parent bd
// wires for `--deps parent-child:a3f8` is the bead a3f8 abbreviates.
type bdCreateParent struct {
	ID       string
	Resolved bool
}

// bdCreateParents returns every parent a single create names, in the order
// bd wires them (cmd/bd create_atomic.go: the --parent edge, then the --deps
// edges): the --parent value, then each --deps entry of type parent-child in
// every spelling bd's parseDepSpec accepts — the flag inline or with the
// next token, its value split as pflag's StringSlice splits it (a CSV
// record; a repeated flag appends), each entry TrimSpace'd, its type and its
// id TrimSpace'd around the first colon (cmd/bd create_deps.go parseDepSpecs
// / parseDepSpec). A bare id (blocks) or an entry of any other type is not a
// parent; an `external:` target, which bd wires literally without resolving
// (resolveDepSpecTargets), names no bead of this store and is not one
// either. Duplicates are dropped; an empty id is no parent.
func bdCreateParents(bdArgs []string) []bdCreateParent {
	var parents []bdCreateParent
	add := func(id string, resolved bool) {
		if id = strings.TrimSpace(id); id == "" || strings.HasPrefix(id, "external:") {
			return
		}
		if slices.ContainsFunc(parents, func(p bdCreateParent) bool { return p.ID == id }) {
			return
		}
		parents = append(parents, bdCreateParent{ID: id, Resolved: resolved})
	}
	add(bdCreateValueFlag(bdArgs, "--parent"), false)
	bdFlagTokens(bdArgs, func(i int, name, value string, inline bool) {
		if name != "--deps" {
			return
		}
		if !inline {
			if i+1 >= len(bdArgs) {
				return
			}
			value = bdArgs[i+1]
		}
		for _, raw := range bdSliceFlagValues(value) {
			depType, target, ok := strings.Cut(strings.TrimSpace(raw), ":")
			if !ok || strings.TrimSpace(depType) != bdDepParentChild {
				continue
			}
			add(target, true)
		}
	})
	return parents
}

// bdReadCreateParent reads one parent of a create through parentRead, the
// store's live handle, which reads an id EXACTLY whatever the store (the
// bd-backed store's Get guards against bd's resolver answering with another
// bead; the native Dolt and DoltLite readers look the id up as given). A
// --parent id is read as typed, as bd's create wires it. A --deps target is
// first resolved through resolveDep — the scoped bd's own resolver
// (bdResolveCreateDepTarget), the one bd's create runs on that target — and
// the id bd names is the one read; a target bd cannot resolve (absent,
// ambiguous, a bd that cannot answer) refuses the create as an unreadable
// parent does. (The lane's codex rounds 2 and 3 on finding 1n: the exact-id
// guard had refused a create bd itself accepts, and a fallback on the
// bd-backed store's collision report served only that store.)
func bdReadCreateParent(parentRead func(id string) (beads.Bead, error), resolveDep func(id string) (string, error), p bdCreateParent) (beads.Bead, error) {
	id := p.ID
	if p.Resolved && resolveDep != nil {
		resolved, err := resolveDep(p.ID)
		if err != nil {
			return beads.Bead{}, err
		}
		if resolved != "" {
			id = resolved
		}
	}
	return parentRead(id)
}

// bdShowIDRow is the part of a `bd show --json` row the resolver reads.
type bdShowIDRow struct {
	ID string `json:"id"`
}

// bdResolveCreateDepTarget resolves one --deps target the way bd's create
// does: by asking the scoped bd — the binary and environment the create
// itself will run under, with the create's leading global flags so the
// same database answers — to show the target, positionally, so bd's own
// partial-id resolver (utils.ResolvePartialID: the exact id, else a unique
// leading abbreviation under a known prefix) names the bead; the id of the
// row bd returns is the parent. bd's refusal (no match, an ambiguous
// abbreviation, a store it cannot reach) is the error, with bd's stderr.
func bdResolveCreateDepTarget(ctx context.Context, run bdWatchRunFunc, globals []string, target string) (string, error) {
	args := append(append([]string{}, globals...), "show", "--json", target)
	var out, errOut bytes.Buffer
	if code := run(ctx, args, &out, &errOut); code != 0 {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = fmt.Sprintf("bd show exited %d", code)
		}
		return "", fmt.Errorf("resolving --deps target %q through bd: %s", target, msg)
	}
	var rows []bdShowIDRow
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &rows); err != nil || len(rows) == 0 || strings.TrimSpace(rows[0].ID) == "" {
		return "", fmt.Errorf("resolving --deps target %q through bd: no bead in bd's answer", target)
	}
	return strings.TrimSpace(rows[0].ID), nil
}

// bdHasBoolFlag reports whether a boolean flag is set before a `--`
// terminator, honoring an inline value and skipping value flags' values. A
// repeated flag resolves as pflag does: the last occurrence wins
// (`--dry-run --dry-run=false` is not a dry run).
func bdHasBoolFlag(bdArgs []string, flag string) bool {
	set := false
	bdFlagTokens(bdArgs, func(_ int, name, value string, inline bool) {
		if name != flag {
			return
		}
		if !inline {
			set = true
			return
		}
		v, err := strconv.ParseBool(value)
		set = err != nil || v
	})
	return set
}

// bdOwnerLabelDatabaseOverride reports selections that a reopen of the scope's
// configured store cannot safely reproduce: bd's database flags, its other
// destination selectors (`--repo` opens another repository's store, `--global`
// the shared server's global database), and the environment's database
// selection. Inspect the actual child environment (after gc's projections),
// not the ambient environment. Conservatively skip the follow-up stamp — and
// the parent read before the create — even when an explicit override happens
// to name the same store. A directory selector is safe only when its final
// destination has been verified to resolve to the opened rig scope.
func bdOwnerLabelDatabaseOverride(bdArgs, env []string, directoryMatchesScope bool) bool {
	overridden := false
	bdFlagTokens(bdArgs, func(_ int, name, value string, inline bool) {
		switch name {
		case "--database", "--db", "--repo":
			overridden = true
		case "-C", "--directory":
			overridden = overridden || !directoryMatchesScope
		case "--global":
			if !inline {
				overridden = true
				return
			}
			if v, err := strconv.ParseBool(value); err != nil || v {
				overridden = true
			}
		}
	})
	if overridden {
		return true
	}
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		switch name {
		case "BEADS_DB", "BD_DB", "BEADS_DOLT_SERVER_DATABASE":
			if value != "" {
				return true
			}
		}
	}
	return false
}

// bdBeadIDRE is the shape of a bead id — <prefix>-<suffix>, with an optional
// class or wisp segment, and bd's dotted hierarchical children (ci-root.1);
// anything else in bd's output is not an id.
var bdBeadIDRE = regexp.MustCompile(`^[A-Za-z0-9]+(?:[-.][A-Za-z0-9]+)+$`)

var (
	// bdCreatedIssueRE matches bd's single-create success line:
	//   ✓ Created issue: <id> — <title>
	bdCreatedIssueRE = regexp.MustCompile(`^\s*(?:✓\s+)?Created issue: (\S+)(?:\s|$)`)
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
		// Consume batch records before inspecting text that can be a title.
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
		if m := bdCreatedIssueRE.FindStringSubmatch(line); m != nil {
			add(m[1])
			continue
		}
		header := strings.TrimPrefix(strings.TrimSpace(line), "✓ ")
		switch {
		case strings.HasPrefix(header, "Created ") && strings.Contains(header, " issues from "):
			inMarkdownListing, inGraphListing = true, false
			continue
		case strings.HasPrefix(header, "Created ") && strings.Contains(header, " issues"):
			inGraphListing, inMarkdownListing = true, false
			continue
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
