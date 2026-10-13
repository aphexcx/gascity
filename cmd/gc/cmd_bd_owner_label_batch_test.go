package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// Regression coverage for gf-xid finding 1j (the Opus read's Major on gf-pu2
// finding 1g, cmd_bd_owner_label.go:368): a --graph plan or a --file batch was
// handed to bd unowned and repaired after, although bd persists per-node and
// per-issue labels inside the create's own transaction — so a routed graph
// node was published unowned and claimable by a peer city until the repair.
// The owner now travels in the batch's own input.

// TestGcBdCreateCarriesTheOwnerInABatch: the acceptance table for the batch
// half of the create-time door. The plan or the markdown bd reads carries the
// owner on every row — this city's; the parent's for a node under another
// city's bead, in-plan or existing; an authored owner kept — and no post-create
// label write runs when bd persisted them.
func TestGcBdCreateCarriesTheOwnerInABatch(t *testing.T) {
	t.Run("a routed graph plan's nodes carry the owner in the create's own input", func(t *testing.T) {
		capture := filepath.Join(t.TempDir(), "gc-bd-args.txt")
		fakeBdCityTestSetup(t, federatedDemoCityTOML, createFakeBd)
		plan := `{
  "commit_message": "a plan field gc keeps as is",
  "nodes": [
    {"key": "root", "title": "Root", "metadata": {"gc.routed_to": "pool/worker"}, "future_field": {"kept": [1, 2]}},
    {"key": "step", "title": "Step", "parent_key": "root", "labels": ["p1"]},
    {"key": "alias", "title": "Alias", "parent": "root"},
    {"key": "explicit", "title": "Explicit", "labels": ["owner:boomtown"]},
    {"key": "child", "title": "Child of an existing bead", "parent_id": "demo-0"},
    {"key": "grandchild", "title": "Under the child", "parent_key": "child"}
  ],
  "edges": [{"from_key": "step", "to_key": "root", "type": "blocks"}]
}
`
		if err := os.WriteFile(filepath.Join(os.Getenv("GC_CITY_PATH"), "plan.json"), []byte(plan), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CAPTURE_PATH", capture)
		t.Setenv("LABELS", "owner:citadel") // bd persisted the per-node labels: every created bead reads owned
		t.Setenv("CREATE_OUT", "Created 6 issues\n  root -> demo-1\n  step -> demo-2\n  alias -> demo-3\n  explicit -> demo-4\n  child -> demo-5\n  grandchild -> demo-6")
		t.Setenv("CREATE_EXIT", "")
		t.Setenv("PARENT_UNREADABLE", "")
		t.Setenv("IGNORE_CREATE_LABELS", "")
		t.Setenv("PARENT", "")

		var stdout, stderr bytes.Buffer
		if got := doBd([]string{"create", "--graph", "plan.json", "--json"}, &stdout, &stderr); got != 0 {
			t.Fatalf("doBd = %d, want 0; stderr=%q", got, stderr.String())
		}
		calls := fakeBdCalls(t, capture)
		if len(calls) < 2 || calls[0] != "show --json demo-0" || !strings.HasPrefix(calls[1], "create --graph plan.json --json --graph=") {
			t.Fatalf("bd received %q, want the existing parent read first and then the create pointed at the rewritten plan", calls)
		}
		for _, c := range calls[2:] {
			if strings.HasPrefix(c, "update ") {
				t.Fatalf("a post-create label write ran although every node carried its owner: %q", calls)
			}
		}
		rewritten, err := os.ReadFile(capture + ".plan")
		if err != nil {
			t.Fatalf("the fake bd did not see a rewritten plan: %v", err)
		}
		var got struct {
			CommitMessage string `json:"commit_message"`
			Nodes         []struct {
				Key         string            `json:"key"`
				Labels      []string          `json:"labels"`
				Metadata    map[string]string `json:"metadata"`
				FutureField json.RawMessage   `json:"future_field"`
				ParentKey   string            `json:"parent_key"`
				Parent      string            `json:"parent"`
				ParentID    string            `json:"parent_id"`
			} `json:"nodes"`
			Edges []map[string]string `json:"edges"`
		}
		if err := json.Unmarshal(rewritten, &got); err != nil {
			t.Fatalf("rewritten plan is not JSON: %v\n%s", err, rewritten)
		}
		want := map[string][]string{
			"root":       {"owner:citadel"},
			"step":       {"p1", "owner:citadel"},
			"alias":      {"owner:citadel"},
			"explicit":   {"owner:boomtown"},
			"child":      {"owner:jadegate"},
			"grandchild": {"owner:jadegate"},
		}
		if len(got.Nodes) != len(want) {
			t.Fatalf("rewritten plan has %d nodes, want %d:\n%s", len(got.Nodes), len(want), rewritten)
		}
		for _, n := range got.Nodes {
			if !reflect.DeepEqual(n.Labels, want[n.Key]) {
				t.Errorf("node %s labels = %v, want %v", n.Key, n.Labels, want[n.Key])
			}
		}
		if got.CommitMessage != "a plan field gc keeps as is" || len(got.Edges) != 1 || got.Edges[0]["type"] != "blocks" {
			t.Errorf("plan members outside the nodes' labels must be carried through: %s", rewritten)
		}
		var future bytes.Buffer
		if err := json.Compact(&future, got.Nodes[0].FutureField); err != nil {
			t.Fatal(err)
		}
		if got.Nodes[0].Metadata["gc.routed_to"] != "pool/worker" || future.String() != `{"kept":[1,2]}` || got.Nodes[1].ParentKey != "root" || got.Nodes[2].Parent != "root" || got.Nodes[4].ParentID != "demo-0" {
			t.Errorf("node members gc does not own must be carried through byte for byte: %s", rewritten)
		}
		if _, err := os.Stat(strings.TrimPrefix(calls[1][strings.Index(calls[1], "--graph=")+len("--graph="):], "")); !os.IsNotExist(err) {
			t.Errorf("the temporary plan must be removed after the create: stat err = %v", err)
		}
	})

	t.Run("a markdown batch's issues carry the owner in their Labels section", func(t *testing.T) {
		capture := filepath.Join(t.TempDir(), "gc-bd-args.txt")
		fakeBdCityTestSetup(t, federatedDemoCityTOML, createFakeBd)
		markdown := "Preamble bd ignores.\n\n## First\nIts description.\n\n### Labels\nfoo, bar\n\n### Priority\n1\n\n## Second\nNo labels section at all.\n\n### Type\nbug\n\n## Third\n\n### labels\nowner:boomtown\n"
		if err := os.WriteFile(filepath.Join(os.Getenv("GC_CITY_PATH"), "issues.md"), []byte(markdown), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CAPTURE_PATH", capture)
		t.Setenv("LABELS", "owner:citadel")
		t.Setenv("CREATE_OUT", "✓ Created 3 issues from issues.md:\n  demo-1: First [P1, task]\n  demo-2: Second [P2, bug]\n  demo-3: Third [P2, task]")
		t.Setenv("CREATE_EXIT", "")
		t.Setenv("IGNORE_CREATE_LABELS", "")

		var stdout, stderr bytes.Buffer
		if got := doBd([]string{"create", "-qf", "issues.md"}, &stdout, &stderr); got != 0 {
			t.Fatalf("doBd = %d, want 0; stderr=%q", got, stderr.String())
		}
		calls := fakeBdCalls(t, capture)
		if len(calls) == 0 || !strings.HasPrefix(calls[0], "create -qf issues.md --file=") || !strings.HasSuffix(calls[0], "/issues.md") {
			t.Fatalf("bd received %q, want the create pointed at a rewritten issues.md (bd checks the extension)", calls)
		}
		if strings.Contains(strings.Join(calls, "\n"), "update ") {
			t.Fatalf("a post-create label write ran although every issue carried its owner: %q", calls)
		}
		rewritten, err := os.ReadFile(capture + ".md")
		if err != nil {
			t.Fatalf("the fake bd did not see a rewritten batch: %v", err)
		}
		want := "Preamble bd ignores.\n\n## First\nIts description.\n\n### Labels\nowner:citadel\nfoo, bar\n\n### Priority\n1\n\n## Second\nNo labels section at all.\n\n### Type\nbug\n\n### Labels\nowner:citadel\n\n## Third\n\n### labels\nowner:boomtown\n"
		if string(rewritten) != want {
			t.Fatalf("rewritten markdown =\n%s\nwant\n%s", rewritten, want)
		}
		if strings.Contains(stderr.String(), "owner label") {
			t.Fatalf("unexpected owner-label notice: %q", stderr.String())
		}
	})

	t.Run("a bd that dropped the per-row labels is repaired after", func(t *testing.T) {
		capture := filepath.Join(t.TempDir(), "gc-bd-args.txt")
		fakeBdCityTestSetup(t, federatedDemoCityTOML, createFakeBd)
		if err := os.WriteFile(filepath.Join(os.Getenv("GC_CITY_PATH"), "plan.json"), []byte(`{"nodes":[{"key":"root","title":"Root"}]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CAPTURE_PATH", capture)
		t.Setenv("LABELS", "") // the created bead reads unowned
		t.Setenv("CREATE_OUT", "Created 1 issues\n  root -> demo-1")
		t.Setenv("CREATE_EXIT", "")

		var stdout, stderr bytes.Buffer
		if got := doBd([]string{"create", "--graph", "plan.json"}, &stdout, &stderr); got != 0 {
			t.Fatalf("doBd = %d, want 0; stderr=%q", got, stderr.String())
		}
		if calls := strings.Join(fakeBdCalls(t, capture), "\n"); !strings.Contains(calls, "update --json demo-1 --add-label owner:citadel") {
			t.Fatalf("calls = %q, want the unowned bead repaired after the create", calls)
		}
	})

	t.Run("a plan node whose existing parent cannot be read refuses the create", func(t *testing.T) {
		capture := filepath.Join(t.TempDir(), "gc-bd-args.txt")
		fakeBdCityTestSetup(t, federatedDemoCityTOML, createFakeBd)
		if err := os.WriteFile(filepath.Join(os.Getenv("GC_CITY_PATH"), "plan.json"), []byte(`{"nodes":[{"key":"child","title":"Child","parent_id":"demo-0"}]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CAPTURE_PATH", capture)
		t.Setenv("LABELS", "")
		t.Setenv("CREATE_OUT", "")
		t.Setenv("PARENT_UNREADABLE", "1")

		var stdout, stderr bytes.Buffer
		if got := doBd([]string{"create", "--graph", "plan.json"}, &stdout, &stderr); got != 1 {
			t.Fatalf("doBd = %d, want 1 (refused); stderr=%q", got, stderr.String())
		}
		if !strings.Contains(stderr.String(), "refusing to create") || !strings.Contains(stderr.String(), "demo-0") {
			t.Fatalf("stderr = %q, want a refusal naming the parent", stderr.String())
		}
		for _, c := range fakeBdCalls(t, capture) {
			if !strings.HasPrefix(c, "show ") {
				t.Fatalf("bd ran %q; nothing but the parent read may run for a refused create", c)
			}
		}
	})

	t.Run("under a database override a node naming an existing parent is left as authored and said", func(t *testing.T) {
		capture := filepath.Join(t.TempDir(), "gc-bd-args.txt")
		fakeBdCityTestSetup(t, federatedDemoCityTOML, createFakeBd)
		if err := os.WriteFile(filepath.Join(os.Getenv("GC_CITY_PATH"), "plan.json"), []byte(`{"nodes":[{"key":"root","title":"Root"},{"key":"child","title":"Child","parent_id":"other-1"}]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CAPTURE_PATH", capture)
		t.Setenv("LABELS", "")
		t.Setenv("CREATE_OUT", "Created 2 issues\n  root -> demo-1\n  child -> demo-2")
		t.Setenv("CREATE_EXIT", "")

		var stdout, stderr bytes.Buffer
		if got := doBd([]string{"create", "--graph", "plan.json", "--database=other"}, &stdout, &stderr); got != 0 {
			t.Fatalf("doBd = %d, want 0; stderr=%q", got, stderr.String())
		}
		calls := fakeBdCalls(t, capture)
		if len(calls) != 1 || !strings.HasPrefix(calls[0], "create --graph plan.json --database=other --graph=") {
			t.Fatalf("bd calls = %q, want only the create pointed at the rewritten plan (no parent read, no repair in the configured store)", calls)
		}
		rewritten, err := os.ReadFile(capture + ".plan")
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Nodes []struct {
				Key    string   `json:"key"`
				Labels []string `json:"labels"`
			} `json:"nodes"`
		}
		if err := json.Unmarshal(rewritten, &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Nodes) != 2 || !reflect.DeepEqual(got.Nodes[0].Labels, []string{"owner:citadel"}) || len(got.Nodes[1].Labels) != 0 {
			t.Fatalf("under an override the parentless node carries this city's owner and the child stays as authored; got %s", rewritten)
		}
		if !strings.Contains(stderr.String(), "owner label not applied") || !strings.Contains(stderr.String(), "database") {
			t.Fatalf("stderr = %q, want the database-selection notice for the node that carried nothing", stderr.String())
		}
	})

	t.Run("a plan gc cannot parse reaches bd as typed", func(t *testing.T) {
		capture := filepath.Join(t.TempDir(), "gc-bd-args.txt")
		fakeBdCityTestSetup(t, federatedDemoCityTOML, createFakeBd)
		if err := os.WriteFile(filepath.Join(os.Getenv("GC_CITY_PATH"), "plan.json"), []byte(`{"nodes": [`), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CAPTURE_PATH", capture)
		t.Setenv("LABELS", "")
		t.Setenv("CREATE_OUT", "Error: reading graph plan: unexpected end of JSON input")
		t.Setenv("CREATE_EXIT", "1")

		var stdout, stderr bytes.Buffer
		if got := doBd([]string{"create", "--graph", "plan.json"}, &stdout, &stderr); got != 1 {
			t.Fatalf("doBd = %d, want bd's exit 1; stderr=%q", got, stderr.String())
		}
		if calls := fakeBdCalls(t, capture); len(calls) != 1 || calls[0] != "create --graph plan.json" {
			t.Fatalf("bd calls = %q, want the create as typed", calls)
		}
	})
}

// TestBdMarkdownBatchWithOwner pins the markdown rewrite against bd's own
// parser rules (an H2 opens an issue; an H3 opens a section, matched by name
// case-insensitively; labels split on commas, spaces and newlines).
func TestBdMarkdownBatchWithOwner(t *testing.T) {
	for _, tt := range []struct {
		name    string
		in      string
		want    string
		rewrite bool
	}{
		{"no issue", "just text\n", "", false},
		{"one issue, no sections", "## Only\n", "## Only\n\n### Labels\nowner:citadel\n", true},
		{"one issue, description only", "## Only\nSome text\n", "## Only\nSome text\n\n### Labels\nowner:citadel\n", true},
		{"labels section present", "## A\n\n### Labels\nx, y\n", "## A\n\n### Labels\nowner:citadel\nx, y\n", true},
		{"labels section empty", "## A\n### Labels\n\n### Type\nbug\n", "## A\n### Labels\nowner:citadel\n\n### Type\nbug\n", true},
		{"authored owner kept", "## A\n### LABELS\nowner:boomtown z\n", "## A\n### LABELS\nowner:boomtown z\n", true},
		{"authored owner on a later line kept", "## A\n### Labels\nx\nowner:boomtown\n", "## A\n### Labels\nx\nowner:boomtown\n", true},
		// bd replaces an issue's labels with each nonempty Labels section and
		// ignores a blank one, so the effective section is the last nonempty.
		{"a populated section followed by an empty one keeps its labels", "## A\n### Labels\nx, y\n### Labels\n\n### Type\nbug\n", "## A\n### Labels\nowner:citadel\nx, y\n### Labels\n\n### Type\nbug\n", true},
		{"an authored owner superseded by a later nonempty section", "## A\n### Labels\nowner:boomtown\n### Labels\nz\n", "## A\n### Labels\nowner:boomtown\n### Labels\nowner:citadel\nz\n", true},
		{"every section blank: the last one takes it", "## A\n### Labels\n\n### Labels\n\n", "## A\n### Labels\n\n### Labels\nowner:citadel\n\n", true},
		{"two issues, the second without a section, ends with one", "## A\n### Labels\nx\n## B\ntext\n### Design\nd\n", "## A\n### Labels\nowner:citadel\nx\n## B\ntext\n### Design\nd\n\n### Labels\nowner:citadel\n", true},
		{"no trailing newline", "## A", "## A\n\n### Labels\nowner:citadel\n", true},
		{"preamble left alone, h1 is not an issue", "# Title\n## A\n", "# Title\n## A\n\n### Labels\nowner:citadel\n", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := bdMarkdownBatchWithOwner([]byte(tt.in), "owner:citadel")
			if ok != tt.rewrite {
				t.Fatalf("rewrite = %v, want %v", ok, tt.rewrite)
			}
			if tt.rewrite && string(got) != tt.want {
				t.Fatalf("got\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

// TestBdGraphPlanWithOwner pins the generic plan rewrite: only each node's
// labels change; a plan that is not a nodes object is left to bd.
func TestBdGraphPlanWithOwner(t *testing.T) {
	for _, in := range []string{`[]`, `{"edges":[]}`, `{"nodes":"no"}`, `{"nodes":[1]}`, `not json`} {
		if got, _, err := bdGraphPlanWithOwner([]byte(in), "owner:citadel", nil); got != nil || err != nil {
			t.Errorf("bdGraphPlanWithOwner(%q) = (%s, %v), want nil (bd refuses it)", in, got, err)
		}
	}
	got, carried, err := bdGraphPlanWithOwner([]byte(`{"nodes":[{"key":"a","title":"A","priority":0,"ephemeral":true,"labels":["owner:boomtown"]},{"key":"b","title":"B","parent_key":"a"},{"key":"c","title":"C"}]}`), "owner:citadel", nil)
	if err != nil || !carried {
		t.Fatalf("carried=%v err=%v", carried, err)
	}
	// With no store to read (a database override), a node naming an existing
	// parent is left as authored, and so is every in-plan descendant without
	// an owner of its own — down to a node that names one, whose own subtree
	// resolves from it.
	unread, carriedUnread, err := bdGraphPlanWithOwner([]byte(`{"nodes":[
		{"key":"root","title":"R"},
		{"key":"child","title":"C","parent_id":"other-1"},
		{"key":"grandchild","title":"G","parent_key":"child"},
		{"key":"great","title":"GG","parent":"grandchild"},
		{"key":"explicit","title":"E","parent_key":"child","labels":["owner:boomtown"]},
		{"key":"under-explicit","title":"UE","parent_key":"explicit"}]}`), "owner:citadel", nil)
	if err != nil || carriedUnread {
		t.Fatalf("unread parent: carried=%v err=%v (a subtree under an unread parent carries nothing)", carriedUnread, err)
	}
	var unreadPlan struct {
		Nodes []struct {
			Key    string   `json:"key"`
			Labels []string `json:"labels"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(unread, &unreadPlan); err != nil {
		t.Fatal(err)
	}
	wantUnread := map[string][]string{"root": {"owner:citadel"}, "child": nil, "grandchild": nil, "great": nil, "explicit": {"owner:boomtown"}, "under-explicit": {"owner:boomtown"}}
	for _, n := range unreadPlan.Nodes {
		if !reflect.DeepEqual(n.Labels, wantUnread[n.Key]) {
			t.Errorf("unread parent: node %s labels = %v, want %v", n.Key, n.Labels, wantUnread[n.Key])
		}
	}
	// A parent_id naming another node of the same plan by its explicit id is
	// an in-plan parent: resolved in-plan (the store is never asked, which
	// would refuse the create because bd has not persisted the node yet), and
	// the owner flows down from it like any in-plan parent's.
	readCalls := 0
	inPlan, carriedInPlan, err := bdGraphPlanWithOwner([]byte(`{"nodes":[
		{"key":"root","id":"hw-root-1","title":"R","labels":["owner:boomtown"]},
		{"key":"child","title":"C","parent_id":"hw-root-1"},
		{"key":"other","title":"O","parent_id":"hw-elsewhere"}]}`), "owner:citadel", func(id string) (beads.Bead, error) {
		readCalls++
		if id != "hw-elsewhere" {
			t.Errorf("parentRead(%q): an in-plan explicit id must never be read from the store", id)
		}
		return beads.Bead{ID: id, Labels: []string{"owner:jadegate"}}, nil
	})
	if err != nil || !carriedInPlan || readCalls != 1 {
		t.Fatalf("in-plan explicit id: carried=%v err=%v reads=%d, want carried, no error, one store read (the existing parent)", carriedInPlan, err, readCalls)
	}
	var inPlanPlan struct {
		Nodes []struct {
			Key    string   `json:"key"`
			Labels []string `json:"labels"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(inPlan, &inPlanPlan); err != nil {
		t.Fatal(err)
	}
	for _, n := range inPlanPlan.Nodes {
		want := map[string][]string{"root": {"owner:boomtown"}, "child": {"owner:boomtown"}, "other": {"owner:jadegate"}}[n.Key]
		if !reflect.DeepEqual(n.Labels, want) {
			t.Errorf("in-plan explicit id: node %s labels = %v, want %v", n.Key, n.Labels, want)
		}
	}
	var plan struct {
		Nodes []map[string]json.RawMessage `json:"nodes"`
	}
	if err := json.Unmarshal(got, &plan); err != nil {
		t.Fatal(err)
	}
	compact := func(raw json.RawMessage) string {
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	for i, want := range []string{`["owner:boomtown"]`, `["owner:boomtown"]`, `["owner:citadel"]`} {
		if got := compact(plan.Nodes[i]["labels"]); got != want {
			t.Errorf("node %d labels = %s, want %s", i, got, want)
		}
	}
	if compact(plan.Nodes[0]["priority"]) != "0" || compact(plan.Nodes[0]["ephemeral"]) != "true" {
		t.Errorf("node members gc does not own must survive as bytes: %s", got)
	}
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"create", "--graph", "p.json"}, "create --graph p.json --graph=/tmp/x/p.json"},
		{[]string{"create", "--graph=p.json", "--", "x"}, "create --graph=p.json --graph=/tmp/x/p.json -- x"},
	} {
		if got := strings.Join(bdCreateArgvWithBatchFile(tt.args, bdBatchGraph, "/tmp/x/p.json"), " "); got != tt.want {
			t.Errorf("bdCreateArgvWithBatchFile(%v) = %q, want %q", tt.args, got, tt.want)
		}
	}
	for _, tt := range []struct {
		args []string
		form bdBatchCreateForm
		path string
	}{
		{[]string{"create", "--graph", "p.json"}, bdBatchGraph, "p.json"},
		{[]string{"create", "-qf", "i.md"}, bdBatchMarkdown, "i.md"},
		{[]string{"create", "--file=i.md"}, bdBatchMarkdown, "i.md"},
		{[]string{"create", "--file", "a.md", "-f", "b.md"}, bdBatchMarkdown, "b.md"},
		{[]string{"create", "-f", "old.md", "--file", "new.md"}, bdBatchMarkdown, "new.md"},
		{[]string{"create", "-fold.md", "--file=new.md"}, bdBatchMarkdown, "new.md"},
		{[]string{"create", "--graph", "a.json", "--graph=b.json"}, bdBatchGraph, "b.json"},
		{[]string{"create", "--graph", " plan.json "}, bdBatchGraph, " plan.json "}, // bd's value, byte for byte
		{[]string{"create", "--graph", "p.json", "--file", "i.md"}, bdBatchNone, ""},
		{[]string{"create", "t", "--description", "--graph"}, bdBatchNone, ""},
	} {
		if form, path := bdCreateBatchFile(tt.args); form != tt.form || path != tt.path {
			t.Errorf("bdCreateBatchFile(%v) = (%v, %q), want (%v, %q)", tt.args, form, path, tt.form, tt.path)
		}
	}
}
