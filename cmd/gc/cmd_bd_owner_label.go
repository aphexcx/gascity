package main

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/federation"
)

// bdNoOwnerLabelFlag is the gc-owned opt-out on `gc bd create`: leave this
// one create unlabeled. It never reaches bd.
const bdNoOwnerLabelFlag = "--no-owner-label"

// bdOwnerLabelNotAppliedNotice is the one stderr line an operator sees when a
// federated city's create ran but gc could not find the created bead in bd's
// output to label it. The create itself is untouched.
const bdOwnerLabelNotAppliedNotice = "gc bd: owner label not applied (could not read the created bead id from bd's output)"

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
// out loud, once, on stderr; a --dry-run creates nothing and is left alone.

// stripBdNoOwnerLabel removes the gc-owned opt-out in every spelling bd's flag
// parser would accept for a boolean (bare, =true, =false, =1, =0 …) from the
// tokens before a `--` terminator, and reports whether the operator opted out
// — which `=false` does not.
func stripBdNoOwnerLabel(bdArgs []string) ([]string, bool) {
	out := make([]string, 0, len(bdArgs))
	optOut := false
	seenTerminator := false
	for _, tok := range bdArgs {
		if seenTerminator || tok == "--" {
			seenTerminator = true
			out = append(out, tok)
			continue
		}
		name, value, inline := strings.Cut(tok, "=")
		if name != bdNoOwnerLabelFlag {
			out = append(out, tok)
			continue
		}
		if !inline {
			optOut = true
			continue
		}
		if v, err := strconv.ParseBool(value); err != nil || v {
			optOut = true // an unparseable value: bd would have rejected it; treat as the bare flag
		}
	}
	return out, optOut
}

// bdLooksLikeCreate reports whether the argv may be a create: `create` or its
// alias `new` anywhere before a `--` terminator. Generous on purpose — a
// global flag gc does not know may sit before the verb and hide it — because
// a false positive costs one scan of output that names no created bead, while
// a miss leaves a bead unlabeled.
func bdLooksLikeCreate(bdArgs []string) bool {
	for _, tok := range bdArgs {
		if tok == "--" {
			return false
		}
		if tok == "create" || tok == "new" {
			return true
		}
	}
	return false
}

// bdHasBoolFlag reports whether a boolean flag is set in the tokens before a
// `--` terminator, honoring an inline value.
func bdHasBoolFlag(bdArgs []string, flag string) bool {
	for _, tok := range bdArgs {
		if tok == "--" {
			return false
		}
		name, value, inline := strings.Cut(tok, "=")
		if name != flag {
			continue
		}
		if !inline {
			return true
		}
		if v, err := strconv.ParseBool(value); err != nil || v {
			return true
		}
	}
	return false
}

// bdBeadIDRE is the shape of a bead id (<prefix>-<suffix>, with an optional
// class or wisp segment); anything else in bd's output is not an id.
var bdBeadIDRE = regexp.MustCompile(`^[A-Za-z0-9]+(?:-[A-Za-z0-9]+)+$`)

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
// through the store first. Every failure is named on stderr; the create has
// already happened, so nothing here changes the exit code.
func stampCreatedBeads(store beads.Store, ids []string, owner string, stderr io.Writer) (stamped, kept, failed int) {
	for _, id := range ids {
		bead, err := store.Get(id)
		if err != nil {
			failed++
			fmt.Fprintf(stderr, "gc bd: owner label not applied to %s: reading it back: %v\n", id, err) //nolint:errcheck // best-effort stderr
			continue
		}
		if federation.HasOwnerLabel(bead.Labels) {
			kept++
			continue
		}
		if err := store.Update(id, beads.UpdateOpts{Labels: []string{owner}}); err != nil {
			failed++
			fmt.Fprintf(stderr, "gc bd: owner label not applied to %s: %v\n", id, err) //nolint:errcheck // best-effort stderr
			continue
		}
		stamped++
	}
	return stamped, kept, failed
}
