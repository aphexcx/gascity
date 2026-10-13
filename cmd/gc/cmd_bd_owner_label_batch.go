package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
// one ordered walk of the argv (`-f old.md --file new.md` names new.md); the
// value is bd's byte for byte, never trimmed (` plan.json` is a file of that
// name). bd refuses --file beside --graph, so a create naming both is handed
// to bd unchanged to refuse.
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
			graph = value
		} else {
			markdown = value
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
// file gc cannot read is handed to bd unchanged (bd cannot read it either,
// refuses it and mints nothing), carried=false, so the follow-up repair
// stands; a graph plan gc read but cannot stamp is REFUSED before bd runs
// (bdGraphPlanWithOwner), never handed over unowned.
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
// not an unowned one. A plan gc cannot stamp refuses the create too.
func bdCreateBatchWithOwner(bdArgs []string, scopeRoot string, form bdBatchCreateForm, path, owner string, parentRead func(id string) (beads.Bead, error)) (bdBatchOwnerRewrite, error) {
	unchanged := bdBatchOwnerRewrite{args: bdArgs, cleanup: func() {}}
	if form == bdBatchMarkdown {
		// bd's own checks on the path (validateMarkdownPath, cmd/bd
		// markdown.go:107-121), applied to the OPERATOR's path: the rewrite
		// hands bd a temporary copy, which would otherwise let a path bd
		// refuses (a `..` component, a non-markdown extension) through.
		if clean := filepath.Clean(path); strings.Contains(clean, "..") {
			return unchanged, fmt.Errorf("invalid file path: directory traversal not allowed (bd refuses %q)", path)
		}
		if ext := strings.ToLower(filepath.Ext(path)); ext != ".md" && ext != ".markdown" {
			return unchanged, fmt.Errorf("invalid file type: only .md and .markdown files are supported (bd refuses %q)", path)
		}
	}
	source := path
	if scopeRoot != "" && !filepath.IsAbs(source) {
		source = filepath.Join(scopeRoot, source)
	}
	data, err := os.ReadFile(source) // #nosec G304 -- the operator's own batch file
	if err != nil {
		// bd reads the same path from the same directory as the same user, so
		// a file gc cannot read is one bd cannot read either: bd refuses it
		// and mints nothing, and the operator sees bd's own error.
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
		rewritten, carried, err = bdMarkdownBatchWithOwner(data, owner)
		if err != nil {
			return unchanged, err
		}
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

// jsonMember is one member of a JSON object, in document order.
type jsonMember struct {
	key   string
	value json.RawMessage
}

// decodeJSONObject decodes data as one JSON object into its members in
// document order — the order encoding/json binds them in, so the member bd
// reads for a field (the LAST whose key matches it, see bdBoundMember) can be
// told from the ones it shadows. Anything but a single JSON object, trailing
// data included, is an error, as it is for bd's json.Unmarshal.
func decodeJSONObject(data []byte) ([]jsonMember, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("not a JSON object")
	}
	var members []jsonMember
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("object key %v is not a string", keyTok)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		members = append(members, jsonMember{key: key, value: value})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after the JSON object")
	}
	return members, nil
}

// encodeJSONObject renders members as a compact JSON object in their order.
func encodeJSONObject(members []jsonMember) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, m := range members {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := json.Marshal(m.key)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(m.value)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// bdBoundMember returns the index of the member bd's parser binds to the
// field named name, or -1. bd decodes a plan with json.Unmarshal into
// GraphApplyPlan (cmd/bd graph_apply.go, createIssuesFromGraph): encoding/json
// takes a key that matches the field's name exactly OR case-insensitively
// (bd's own unknownKeys helper folds case for that reason), and when several
// keys of one object match, the last one in document order wins.
func bdBoundMember(members []jsonMember, name string) int {
	bound := -1
	for i, m := range members {
		if strings.EqualFold(m.key, name) {
			bound = i
		}
	}
	return bound
}

// bdDuplicateMember returns the name of a field two members of the plan both
// bind to, or "". For a scalar the last member wins outright, but for the
// plan's arrays encoding/json decodes the later member INTO the elements the
// earlier one built (a node's members the later object omits survive from
// the earlier one), so the plan bd ends up with is neither member — such a
// plan is refused rather than stamped from one of them.
func bdDuplicateMember(members []jsonMember) string {
	for i, m := range members {
		for _, other := range members[i+1:] {
			if strings.EqualFold(m.key, other.key) {
				return m.key
			}
		}
	}
	return ""
}

// bdRebindMember returns members with value as the member bd binds to name:
// the bound member keeps its own spelling and takes the value, the members it
// shadowed (bd never read them) are dropped, and a field with no member gets
// one spelled name at the end.
func bdRebindMember(members []jsonMember, name string, value json.RawMessage) []jsonMember {
	bound := bdBoundMember(members, name)
	out := make([]jsonMember, 0, len(members)+1)
	for i, m := range members {
		switch {
		case i == bound:
			out = append(out, jsonMember{key: m.key, value: value})
		case strings.EqualFold(m.key, name):
			continue
		default:
			out = append(out, m)
		}
	}
	if bound < 0 {
		out = append(out, jsonMember{key: name, value: value})
	}
	return out
}

// bdGraphPlanWithOwner returns the plan with the owner label on every node by
// the per-node rule of every create door (beads.GraphPlanOwnerLabels), and
// whether every node carries an owner afterwards. The plan is read the way
// bd reads it — its members bound by name as encoding/json binds them, case
// aside, the last of several winning (bdBoundMember) — and rewritten
// GENERICALLY: only the member bd binds to each node's "labels" is replaced,
// in its own spelling and place; every other member of the plan and of its
// nodes, known to gc or not, is carried through as the bytes bd will read,
// so a plan field gc does not know is never dropped on the way. A plan gc
// cannot stamp — not one JSON object, no nodes array, a node that is not an
// object, edges or deps bd could not decode — is REFUSED with an error:
// handed to bd it would reach the store unowned, and bd refuses the same
// shapes itself. A node's parents are every relationship bd's graph apply
// wires (cmd/bd graph_apply.go): its parent_key, or bd's `parent` alias
// (parent_key wins), else its parent_id; an `edges` entry of type
// parent-child whose child is the node (from_key, or from_id naming the
// node's explicit id) with the parent in to_id or to_key; and a `deps` entry
// of that type on the node (target: a plan key first, then an issue id). A
// parent_id, to_id or dep target that names another node of the SAME plan
// by its explicit "id" is an in-plan parent (bd creates the nodes before it
// wires them) and is resolved in-plan, never read; one naming an existing
// bead is read through parentRead, and a read failure refuses the create. A
// node whose parents sit in different lanes refuses the create. With
// parentRead nil (the configured store cannot be read for this create) a
// node under an existing parent is left as authored — and so is every
// in-plan descendant of it that names no owner of its own.
func bdGraphPlanWithOwner(data []byte, owner string, parentRead func(id string) (beads.Bead, error)) ([]byte, bool, error) {
	plan, err := decodeJSONObject(data)
	if err != nil {
		return nil, false, fmt.Errorf("the graph plan is not one bd can stamp: %w", err)
	}
	if dup := bdDuplicateMember(plan); dup != "" {
		return nil, false, fmt.Errorf("the graph plan names %q twice (bd merges the two): spell each plan member once", dup)
	}
	nodesAt := bdBoundMember(plan, "nodes")
	if nodesAt < 0 {
		return nil, false, fmt.Errorf("the graph plan has no nodes member")
	}
	var rawNodes []json.RawMessage
	if err := json.Unmarshal(plan[nodesAt].value, &rawNodes); err != nil || rawNodes == nil {
		return nil, false, fmt.Errorf("the graph plan's nodes are not a JSON array")
	}
	type nodeView struct {
		Key       string   `json:"key"`
		ID        string   `json:"id"`
		Labels    []string `json:"labels"`
		Parent    string   `json:"parent"`
		ParentKey string   `json:"parent_key"`
		ParentID  string   `json:"parent_id"`
		Deps      []struct {
			Type   string `json:"type"`
			Target string `json:"target"`
		} `json:"deps"`
	}
	type edgeView struct {
		FromKey string `json:"from_key"`
		FromID  string `json:"from_id"`
		ToKey   string `json:"to_key"`
		ToID    string `json:"to_id"`
		Type    string `json:"type"`
	}
	views := make([]nodeView, len(rawNodes))
	members := make([][]jsonMember, len(rawNodes))
	index := make(map[string]int, len(rawNodes)) // by key
	byID := make(map[string]int, len(rawNodes))  // by explicit id
	for i, raw := range rawNodes {
		// json.Unmarshal into the view binds the node's members as bd's own
		// decode of GraphApplyNode does, case and shadowing included.
		if err := json.Unmarshal(raw, &views[i]); err != nil {
			return nil, false, fmt.Errorf("graph plan node %d: %w", i, err)
		}
		if members[i], err = decodeJSONObject(raw); err != nil {
			return nil, false, fmt.Errorf("graph plan node %d is not a JSON object: %w", i, err)
		}
		if views[i].Key != "" {
			index[views[i].Key] = i
		}
		if views[i].ID != "" { // raw, as bd keys the created issue (graph_apply.go:539)
			byID[views[i].ID] = i
		}
	}
	var edges []edgeView
	if edgesAt := bdBoundMember(plan, "edges"); edgesAt >= 0 {
		if err := json.Unmarshal(plan[edgesAt].value, &edges); err != nil {
			return nil, false, fmt.Errorf("graph plan edges: %w", err)
		}
	}
	// ref resolves an id the way bd's apply does: another node of this plan
	// by its explicit id, else an existing bead.
	ref := func(id string) beads.GraphPlanParent {
		if p, ok := byID[id]; ok {
			return beads.GraphPlanParent{Index: p}
		}
		return beads.GraphPlanParent{Index: -1, ID: id}
	}
	parents := make([][]beads.GraphPlanParent, len(rawNodes))
	add := func(child int, parent beads.GraphPlanParent, ok bool) {
		if !ok || parent.Index == child {
			return
		}
		parents[child] = append(parents[child], parent)
	}
	for i, v := range views {
		parentKey := v.ParentKey
		if parentKey == "" {
			parentKey = v.Parent // bd's alias: parent_key wins, then parent
		}
		switch {
		case parentKey != "":
			p, ok := index[parentKey]
			add(i, beads.GraphPlanParent{Index: p}, ok)
		case v.ParentID != "": // raw, as bd wires it (graph_apply.go:1008)
			add(i, ref(v.ParentID), true)
		}
	}
	for _, e := range edges {
		if e.Type != "parent-child" {
			continue
		}
		child, ok := -1, false
		switch {
		case e.FromID != "":
			child, ok = byID[e.FromID] // an existing bead as the child is not a plan node
		case e.FromKey != "":
			child, ok = index[e.FromKey]
		}
		if !ok {
			continue
		}
		switch {
		case e.ToID != "":
			add(child, ref(e.ToID), true)
		case e.ToKey != "":
			p, ok := index[e.ToKey]
			add(child, beads.GraphPlanParent{Index: p}, ok)
		}
	}
	for i, v := range views {
		for _, d := range v.Deps {
			if d.Type != "parent-child" || d.Target == "" {
				continue
			}
			if p, ok := index[d.Target]; ok { // a plan key first, then an issue id
				add(i, beads.GraphPlanParent{Index: p}, true)
				continue
			}
			add(i, ref(d.Target), true)
		}
	}
	authored := make([][]string, len(views))
	names := make([]string, len(views))
	for i, v := range views {
		authored[i], names[i] = v.Labels, v.Key
	}
	var read func(id string) ([]string, error)
	if parentRead != nil {
		read = func(id string) ([]string, error) {
			parent, err := parentRead(id)
			if err != nil {
				return nil, err
			}
			return parent.Labels, nil
		}
	}
	labels, unresolved, err := beads.GraphPlanOwnerLabels(authored, parents, names, owner, read)
	if err != nil {
		return nil, false, err
	}
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
		if rawNodes[i], err = encodeJSONObject(bdRebindMember(members[i], "labels", encoded)); err != nil {
			return nil, false, fmt.Errorf("encoding plan node: %w", err)
		}
	}
	var nodes bytes.Buffer
	nodes.WriteByte('[')
	for i, raw := range rawNodes {
		if i > 0 {
			nodes.WriteByte(',')
		}
		nodes.Write(raw)
	}
	nodes.WriteByte(']')
	compact, err := encodeJSONObject(bdRebindMember(plan, "nodes", nodes.Bytes()))
	if err != nil {
		return nil, false, fmt.Errorf("encoding plan: %w", err)
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", "  "); err != nil {
		return nil, false, fmt.Errorf("encoding plan: %w", err)
	}
	out.WriteByte('\n')
	return out.Bytes(), carried, nil
}

// bdMarkdownListFields splits one line of a Labels (or Dependencies) section
// the way bd's parseStringList does (cmd/bd markdown.go:43-57): the content
// is split on commas, spaces and newlines — a TAB is NOT a separator — and
// each field is TrimSpace'd, empty ones dropped. So `foo<TAB>owner:x` is one
// label to bd, which carries no owner, while `<TAB>owner:x` is the owner
// after the trim. gc judges an authored owner by exactly that reading (the
// Opus read's MAJOR 1(b) on gf-xid: gc had split on tabs too and believed an
// owner was present that bd never stored).
func bdMarkdownListFields(line string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
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
// its LAST NONEMPTY Labels section, split on commas, spaces and newlines
// (bdMarkdownListFields; never on tabs). The
// owner goes as a new line right after that section's header (its other lines
// untouched); an issue whose Labels sections are all blank gets it in the last
// of them (they were nothing to bd); an issue without one gets a `### Labels`
// section appended at the END of its block, after its last section, so no
// existing line changes the section it was in. Text before the first H2 is
// bd's to ignore and is left alone. A file with no issue is REFUSED: there is
// nothing gc can rewrite the way bd would accept, and bd creates nothing
// from it either.
func bdMarkdownBatchWithOwner(data []byte, owner string) ([]byte, bool, error) {
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
		return nil, false, fmt.Errorf("the markdown batch names no issue bd would create (bd expects '## Issue Title'; it refuses the file the same way)")
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
			for _, l := range bdMarkdownListFields(lines[i]) {
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
	return buf.Bytes(), true, nil
}
