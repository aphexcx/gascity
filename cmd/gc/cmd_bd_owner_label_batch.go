package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/federation"
)

// The batch half of the create-time door (gf-xid finding 1j, the Opus read's
// Major on finding 1g). bd refuses its label flags beside --graph and --file
// (rejectSingleIssueFlagsForGraph / rejectSingleIssueFlagsForMarkdown in
// cmd/bd), so finding 1g sent every batch create unowned and repaired its
// beads afterwards — leaving open, for a batch, exactly the window the single
// create had closed: a routed graph node was published unowned, and
// federation.MayClaim accepts an unowned bead, so a peer city could claim it
// before the repair landed. But a batch CAN carry the owner: a graph plan
// node has per-row labels (cmd/bd graph_apply.go, GraphApplyNode.Labels) and a
// markdown issue has a `### Labels` section (cmd/bd markdown.go), both
// persisted inside the create's own transaction. So the owner is written into
// the batch's own input before bd reads it: the plan or the markdown is
// rewritten into a temporary file with the owner on every row, by the same
// per-node rule the in-process door applies (federation.OwnerLabelsForPlan:
// an authored owner wins; a node under an in-plan parent takes that parent's
// effective owner; a node naming an existing parent takes the PARENT's owner,
// read through the store's live handle first, and an unreadable parent
// refuses the create — finding 1d's rule; every other row carries this
// city's), and bd is pointed at that file by a trailing `--graph=<tmp>` /
// `--file=<tmp>` (pflag's last value wins for a string flag, so the operator's
// own token is left in place for every other reader of the argv). The
// follow-up read-back then repairs only a row that could not carry it.

// bdBatchCreateForm names the file-backed create forms bd's create surface
// takes.
type bdBatchCreateForm int

const (
	bdBatchNone     bdBatchCreateForm = iota
	bdBatchGraph                      // --graph <plan.json>
	bdBatchMarkdown                   // --file / -f <issues.md>
)

// bdCreateBatchFile reports the file-backed form a create argv names and the
// path it names, as bd will read them: the LAST occurrence of a flag wins in
// pflag, and -f and --file are one flag, so the two spellings are resolved in
// one ordered walk of the argv (`-f old.md --file new.md` names new.md). bd
// refuses --file beside --graph, so a create naming both is handed to bd
// unchanged to refuse.
func bdCreateBatchFile(bdArgs []string) (bdBatchCreateForm, string) {
	graph, markdown := "", ""
	bdFlagTokens(bdArgs, func(i int, name, value string, inline bool) {
		switch name {
		case "--graph", "--file", "-f":
		default:
			return
		}
		if !inline {
			if i+1 >= len(bdArgs) {
				return
			}
			value = bdArgs[i+1]
		}
		if name == "--graph" {
			graph = strings.TrimSpace(value)
		} else {
			markdown = strings.TrimSpace(value)
		}
	})
	switch {
	case graph != "" && markdown != "":
		return bdBatchNone, ""
	case graph != "":
		return bdBatchGraph, graph
	case markdown != "":
		return bdBatchMarkdown, markdown
	}
	return bdBatchNone, ""
}

// bdBatchFlag is the long flag that re-points bd at the rewritten file for a
// form; the trailing `<flag>=<path>` wins over the operator's token in pflag.
func (form bdBatchCreateForm) flag() string {
	if form == bdBatchGraph {
		return "--graph"
	}
	return "--file"
}

// bdCreateArgvWithBatchFile returns bdArgs with `<flag>=<path>` appended
// before any `--` terminator, so bd reads path as the batch's file: pflag
// takes the LAST value of a string flag, and the operator's own token stays
// where it was for every other reader of the argv (the relocated-class guard,
// the trace line).
func bdCreateArgvWithBatchFile(bdArgs []string, form bdBatchCreateForm, path string) []string {
	token := form.flag() + "=" + path
	for i, tok := range bdArgs {
		if tok == "--" {
			out := make([]string, 0, len(bdArgs)+1)
			out = append(out, bdArgs[:i]...)
			out = append(out, token)
			return append(out, bdArgs[i:]...)
		}
	}
	out := make([]string, 0, len(bdArgs)+1)
	out = append(out, bdArgs...)
	return append(out, token)
}

// bdBatchOwnerRewrite is the outcome of writing the owner into a batch's
// input: the argv bd runs, whether every row of the batch carries an owner
// (carried), and the cleanup that removes the temporary file. A batch whose
// file gc cannot read or parse is handed to bd unchanged (bd refuses it the
// same way and mints nothing), carried=false, so the follow-up repair stands.
type bdBatchOwnerRewrite struct {
	args    []string
	carried bool
	cleanup func()
}

// bdCreateBatchWithOwner rewrites the batch file a create names so that every
// row carries its owner, and returns the argv that points bd at the rewrite.
// scopeRoot is bd's working directory for the create (a relative path is
// bd's relative to it). parentRead reads an existing bead a plan node names
// as parent through the store's live handle; nil means the configured store
// cannot be read for this create (a bd database override), and such a node
// is left as authored while the rest of the plan still carries this city's
// owner. A parent that cannot be read refuses the create: an unread parent is
// not an unowned one.
func bdCreateBatchWithOwner(bdArgs []string, scopeRoot string, form bdBatchCreateForm, path, owner string, parentRead func(id string) (beads.Bead, error)) (bdBatchOwnerRewrite, error) {
	unchanged := bdBatchOwnerRewrite{args: bdArgs, cleanup: func() {}}
	source := path
	if scopeRoot != "" && !filepath.IsAbs(source) {
		source = filepath.Join(scopeRoot, source)
	}
	data, err := os.ReadFile(source) // #nosec G304 -- the operator's own batch file
	if err != nil {
		return unchanged, nil
	}
	var rewritten []byte
	var carried bool
	switch form {
	case bdBatchGraph:
		rewritten, carried, err = bdGraphPlanWithOwner(data, owner, parentRead)
		if err != nil {
			return unchanged, err
		}
	case bdBatchMarkdown:
		rewritten, carried = bdMarkdownBatchWithOwner(data, owner)
	default:
		return unchanged, nil
	}
	if rewritten == nil {
		return unchanged, nil
	}
	dir, err := os.MkdirTemp("", "gc-bd-owner-label-*")
	if err != nil {
		return unchanged, fmt.Errorf("writing the batch with its owner label: %w", err)
	}
	// bd validates a markdown batch's extension (.md / .markdown), so the
	// rewrite keeps the operator's file name.
	target := filepath.Join(dir, filepath.Base(source))
	if err := os.WriteFile(target, rewritten, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return unchanged, fmt.Errorf("writing the batch with its owner label: %w", err)
	}
	return bdBatchOwnerRewrite{
		args:    bdCreateArgvWithBatchFile(bdArgs, form, target),
		carried: carried,
		cleanup: func() { _ = os.RemoveAll(dir) },
	}, nil
}

// bdGraphPlanWithOwner returns the plan with the owner label on every node by
// the per-node rule (federation.OwnerLabelsForPlan), and whether every node
// carries an owner afterwards. The plan is rewritten GENERICALLY — only each
// node's "labels" member is replaced; every other member of the plan and of
// its nodes, known to gc or not, is carried through as the bytes bd will
// read — so a plan field gc does not know is never dropped on the way. A plan
// that is not a JSON object with a "nodes" array is returned as nil: bd parses
// it the same way, refuses it and mints nothing. An existing parent a node
// names (parent_id) is read through parentRead; a read failure refuses the
// create. With parentRead nil (the configured store cannot be read for this
// create) a node naming an existing parent is left as authored — and so is
// every in-plan descendant of it that names no owner of its own: their lane
// is the unread parent's, which this create cannot know, and stamping this
// city's owner on them would make a foreign subtree claimable here.
func bdGraphPlanWithOwner(data []byte, owner string, parentRead func(id string) (beads.Bead, error)) ([]byte, bool, error) {
	var plan map[string]json.RawMessage
	if err := json.Unmarshal(data, &plan); err != nil || plan == nil {
		return nil, false, nil
	}
	var rawNodes []json.RawMessage
	if err := json.Unmarshal(plan["nodes"], &rawNodes); err != nil {
		return nil, false, nil
	}
	type nodeView struct {
		Key       string   `json:"key"`
		Labels    []string `json:"labels"`
		Parent    string   `json:"parent"`
		ParentKey string   `json:"parent_key"`
		ParentID  string   `json:"parent_id"`
	}
	views := make([]nodeView, len(rawNodes))
	members := make([]map[string]json.RawMessage, len(rawNodes))
	index := make(map[string]int, len(rawNodes))
	for i, raw := range rawNodes {
		if err := json.Unmarshal(raw, &views[i]); err != nil {
			return nil, false, nil
		}
		if err := json.Unmarshal(raw, &members[i]); err != nil || members[i] == nil {
			return nil, false, nil
		}
		if views[i].Key != "" {
			index[views[i].Key] = i
		}
	}
	read := make(map[string][]string)
	nodes := make([]federation.PlanNode, len(rawNodes))
	unresolved := make([]bool, len(rawNodes)) // left as authored: under an unreadable scope's parent
	for i, v := range views {
		pn := federation.PlanNode{Labels: v.Labels, ParentIndex: -1}
		parentKey := v.ParentKey
		if parentKey == "" {
			parentKey = v.Parent // bd's alias: parent_key wins, then parent
		}
		switch {
		case parentKey != "":
			if p, ok := index[parentKey]; ok {
				pn.ParentIndex = p
			}
		case strings.TrimSpace(v.ParentID) != "":
			id := strings.TrimSpace(v.ParentID)
			if parentRead == nil {
				unresolved[i] = !federation.HasOwnerLabel(v.Labels)
				break
			}
			if _, seen := read[id]; !seen {
				parent, err := parentRead(id)
				if err != nil {
					return nil, false, fmt.Errorf("reading parent %q for the owner label: %w", id, err)
				}
				read[id] = parent.Labels
			}
			pn.ParentLabels = read[id]
		}
		nodes[i] = pn
	}
	// An unresolved lane reaches every in-plan descendant that names no owner
	// of its own; a node with an authored owner resolves its own subtree.
	for changed := true; changed; {
		changed = false
		for i, pn := range nodes {
			if unresolved[i] || federation.HasOwnerLabel(pn.Labels) || pn.ParentIndex < 0 || pn.ParentIndex == i {
				continue
			}
			if unresolved[pn.ParentIndex] {
				unresolved[i], changed = true, true
			}
		}
	}
	labels := federation.OwnerLabelsForPlan(nodes, owner)
	carried := true
	for i := range rawNodes {
		if unresolved[i] {
			carried = false
			continue
		}
		encoded, err := json.Marshal(labels[i])
		if err != nil {
			return nil, false, fmt.Errorf("encoding node labels: %w", err)
		}
		members[i]["labels"] = encoded
		node, err := json.Marshal(members[i])
		if err != nil {
			return nil, false, fmt.Errorf("encoding plan node: %w", err)
		}
		rawNodes[i] = node
	}
	encodedNodes, err := json.Marshal(rawNodes)
	if err != nil {
		return nil, false, fmt.Errorf("encoding plan nodes: %w", err)
	}
	plan["nodes"] = encodedNodes
	out, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return nil, false, fmt.Errorf("encoding plan: %w", err)
	}
	return append(out, '\n'), carried, nil
}

var (
	// bdMarkdownIssueRE and bdMarkdownSectionRE are bd's own h2Regex and
	// h3Regex (cmd/bd markdown.go): an H2 opens an issue, an H3 opens a
	// section of it, matched by name case-insensitively.
	bdMarkdownIssueRE   = regexp.MustCompile(`^##\s+(.+)$`)
	bdMarkdownSectionRE = regexp.MustCompile(`^###\s+(.+)$`)
)

// bdMarkdownBatchWithOwner returns the markdown batch with the owner label in
// every issue's effective `### Labels` section, and whether every issue
// carries an owner afterwards (always, once rewritten: a markdown row names no
// parent, so each row either names its own owner, which is kept, or takes
// this city's). The rewrite is the one bd's parser reads: an issue is an H2
// header and everything to the next H2; a section is an H3 header, matched by
// name case-insensitively; a repeated section REPLACES the earlier one unless
// its content is blank, which bd ignores — so an issue's effective labels are
// its LAST NONEMPTY Labels section, split on commas, spaces and newlines. The
// owner goes as a new line right after that section's header (its other lines
// untouched); an issue whose Labels sections are all blank gets it in the last
// of them (they were nothing to bd); an issue without one gets a `### Labels`
// section appended at the END of its block, after its last section, so no
// existing line changes the section it was in. Text before the first H2 is
// bd's to ignore and is left alone. A file with no issue is returned as nil.
func bdMarkdownBatchWithOwner(data []byte, owner string) ([]byte, bool) {
	lines := strings.Split(string(data), "\n")
	if strings.HasSuffix(string(data), "\n") {
		lines = lines[:len(lines)-1]
	}
	type labelsSection struct {
		headerAt int
		nonempty bool
		hasOwner bool
	}
	type issue struct {
		start, end int // [start, end): the H2 line through the line before the next H2
		sections   []labelsSection
	}
	var issues []issue
	for i, line := range lines {
		if bdMarkdownIssueRE.MatchString(line) {
			if n := len(issues); n > 0 {
				issues[n-1].end = i
			}
			issues = append(issues, issue{start: i, end: len(lines)})
		}
	}
	if len(issues) == 0 {
		return nil, false
	}
	for k := range issues {
		it := &issues[k]
		inLabels := false
		for i := it.start + 1; i < it.end; i++ {
			if m := bdMarkdownSectionRE.FindStringSubmatch(lines[i]); m != nil {
				inLabels = strings.ToLower(strings.TrimSpace(m[1])) == "labels"
				if inLabels {
					it.sections = append(it.sections, labelsSection{headerAt: i})
				}
				continue
			}
			if !inLabels || strings.TrimSpace(lines[i]) == "" {
				continue
			}
			sec := &it.sections[len(it.sections)-1]
			sec.nonempty = true
			for _, l := range strings.FieldsFunc(lines[i], func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
				if federation.HasOwnerLabel([]string{l}) {
					sec.hasOwner = true
				}
			}
		}
	}
	// effective picks the Labels section bd's parser ends up with: the last
	// nonempty one; -1 when every one is blank or there is none.
	effective := func(it issue) int {
		for s := len(it.sections) - 1; s >= 0; s-- {
			if it.sections[s].nonempty {
				return s
			}
		}
		return -1
	}
	var out []string
	for k, it := range issues {
		if k == 0 {
			out = append(out, lines[:it.start]...)
		}
		target := effective(it)
		switch {
		case target >= 0 && it.sections[target].hasOwner:
			out = append(out, lines[it.start:it.end]...)
		case target >= 0 || len(it.sections) > 0:
			if target < 0 {
				target = len(it.sections) - 1 // every section blank: the last is as good as any
			}
			at := it.sections[target].headerAt
			out = append(out, lines[it.start:at+1]...)
			out = append(out, owner)
			out = append(out, lines[at+1:it.end]...)
		default:
			out = append(out, lines[it.start:it.end]...)
			if strings.TrimSpace(lines[it.end-1]) != "" {
				out = append(out, "")
			}
			out = append(out, "### Labels", owner)
			if k+1 < len(issues) {
				out = append(out, "")
			}
		}
	}
	var buf bytes.Buffer
	buf.WriteString(strings.Join(out, "\n"))
	buf.WriteString("\n")
	return buf.Bytes(), true
}
