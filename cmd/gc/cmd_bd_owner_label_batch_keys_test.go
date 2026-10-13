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

// TestBdGraphPlanWithOwnerMatchesKeysAsBdDoes pins gf-c7n finding 1m (the
// gf-xid re-gate's Major at cmd_bd_owner_label_batch.go:204): bd decodes the
// plan with encoding/json, which binds a key to a field case-insensitively
// and lets the LAST matching key of an object win, but the rewrite looked up
// "nodes" and "labels" by exact spelling and handed any other plan to bd
// unchanged — a valid plan spelled "Nodes" was published UNOWNED. The rewrite
// now binds keys as bd does, and a plan it cannot stamp is refused before the
// create instead of passed through.
func TestBdGraphPlanWithOwnerMatchesKeysAsBdDoes(t *testing.T) {
	got, carried, err := bdGraphPlanWithOwner([]byte(`{"Nodes":[{"Key":"a","Title":"A","Labels":["p1"]},{"KEY":"b","title":"B","Parent_Key":"a"},{"key":"c","title":"C","PARENT":"a"}],"Edges":[],"Commit_Message":"kept"}`), "owner:citadel", nil)
	if err != nil || !carried {
		t.Fatalf("case-variant keys: carried=%v err=%v", carried, err)
	}
	if labels := planNodeLabels(t, got); !reflect.DeepEqual(labels, map[string][]string{"a": {"p1", "owner:citadel"}, "b": {"owner:citadel"}, "c": {"owner:citadel"}}) {
		t.Errorf("case-variant keys: labels = %v", labels)
	}
	var kept struct {
		CommitMessage string `json:"commit_message"`
	}
	if err := json.Unmarshal(got, &kept); err != nil || kept.CommitMessage != "kept" {
		t.Errorf("a plan member spelled its own way must be carried through: %s", got)
	}

	// Inside a node the last key bd binds wins (a []string is decoded afresh
	// each time), so only the bound labels member is rewritten and kept.
	got, carried, err = bdGraphPlanWithOwner([]byte(`{"nodes":[{"key":"y","title":"Y","labels":["owner:boomtown"],"Labels":["p1"]}]}`), "owner:citadel", nil)
	if err != nil || !carried {
		t.Fatalf("shadowed node keys: carried=%v err=%v", carried, err)
	}
	if labels := planNodeLabels(t, got); !reflect.DeepEqual(labels, map[string][]string{"y": {"p1", "owner:citadel"}}) {
		t.Errorf("shadowed node keys: labels = %v, want y with its LAST labels member (p1) plus this city's owner", labels)
	}
	if n := strings.Count(strings.ToLower(string(got)), `"labels"`); n != 1 {
		t.Errorf("shadowed node keys: the rewritten node must hold one labels member, got %d:\n%s", n, got)
	}
	// Two plan members bd binds to one field are REFUSED (codex round 1 on
	// 1m): for the plan's arrays encoding/json decodes the later member into
	// the elements the earlier one built, so a node member the later object
	// omits (an owner) survives in bd's plan — neither member alone is what
	// bd reads, and stamping from one would change ownership.
	for _, in := range []string{
		`{"nodes":[{"key":"x","title":"X","labels":["owner:boomtown"]}],"Nodes":[{"key":"x","title":"X"}]}`,
		`{"nodes":[{"key":"x","title":"X"}],"edges":[],"Edges":[{"from_key":"x","to_id":"hw-1","type":"parent-child"}]}`,
		`{"commit_message":"a","Commit_Message":"b","nodes":[]}`,
	} {
		if got, carried, err := bdGraphPlanWithOwner([]byte(in), "owner:citadel", nil); err == nil || got != nil || carried || !strings.Contains(err.Error(), "twice") {
			t.Errorf("bdGraphPlanWithOwner(%q) = (%s, %v, %v), want a refusal naming the twice-spelled member", in, got, carried, err)
		}
	}

	// Whatever bd would refuse — and whatever gc cannot stamp — is refused
	// before the create rather than handed to bd unowned.
	for _, in := range []string{`[]`, `{"edges":[]}`, `{"nodes":"no"}`, `{"nodes":null}`, `{"nodes":[1]}`, `not json`, `{"nodes":[]} trailing`, `{"nodes":[],"edges":"no"}`, `{"nodes":[{"key":"a","title":"A","deps":"no"}]}`} {
		if got, carried, err := bdGraphPlanWithOwner([]byte(in), "owner:citadel", nil); err == nil || got != nil || carried {
			t.Errorf("bdGraphPlanWithOwner(%q) = (%s, %v, %v), want a refusal", in, got, carried, err)
		}
	}
	if got, carried, err := bdGraphPlanWithOwner([]byte(`{"nodes":[]}`), "owner:citadel", nil); err != nil || !carried || got == nil {
		t.Errorf("an empty plan is stampable (nothing to stamp): (%s, %v, %v)", got, carried, err)
	}
}

// TestGcBdCreateCarriesTheOwnerInABatchSpelledAsBdReadsIt drives the real
// create through the fake bd with a plan spelled the way bd's parser binds it
// but the round-4 rewrite did not ("Nodes", "Labels", "Parent_Key", a
// parent-child edge to an existing foreign bead): every node reaches bd with
// its owner, the edge-linked child in its parent's lane.
func TestGcBdCreateCarriesTheOwnerInABatchSpelledAsBdReadsIt(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "gc-bd-args.txt")
	fakeBdCityTestSetup(t, federatedDemoCityTOML, createFakeBd)
	plan := `{"Nodes": [
  {"Key": "root", "Title": "Root", "Labels": ["p1"]},
  {"key": "step", "title": "Step", "Parent_Key": "root"},
  {"key": "under-existing", "title": "Under an existing foreign bead"}
], "Edges": [{"from_key": "under-existing", "to_id": "demo-0", "type": "parent-child"}]}
`
	if err := os.WriteFile(filepath.Join(os.Getenv("GC_CITY_PATH"), "plan.json"), []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("LABELS", "owner:citadel")
	t.Setenv("CREATE_OUT", "Created 3 issues\n  root -> demo-1\n  step -> demo-2\n  under-existing -> demo-3")
	t.Setenv("CREATE_EXIT", "")
	t.Setenv("PARENT_UNREADABLE", "")
	t.Setenv("IGNORE_CREATE_LABELS", "")
	t.Setenv("PARENT", "")

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"create", "--graph", "plan.json"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd = %d, want 0; stderr=%q", got, stderr.String())
	}
	calls := fakeBdCalls(t, capture)
	if len(calls) < 2 || calls[0] != "show --json demo-0" || !strings.HasPrefix(calls[1], "create --graph plan.json --graph=") {
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
	want := map[string][]string{"root": {"p1", "owner:citadel"}, "step": {"owner:citadel"}, "under-existing": {"owner:jadegate"}}
	if labels := planNodeLabels(t, rewritten); !reflect.DeepEqual(labels, want) {
		t.Errorf("rewritten plan labels = %v, want %v:\n%s", labels, want, rewritten)
	}
	if !strings.Contains(string(rewritten), `"Nodes"`) || !strings.Contains(string(rewritten), `"Parent_Key"`) {
		t.Errorf("the plan's own spellings must be carried through:\n%s", rewritten)
	}
}

// TestBdCreateBatchWithOwnerAppliesBdsPathChecksToTheOperatorsPath pins the
// Opus read's MINOR 2 (taken in part): the rewrite hands bd a temporary copy,
// so bd's own checks on the markdown path (validateMarkdownPath: a cleaned
// path with `..`, an extension other than .md/.markdown) are applied by gc to
// the OPERATOR's path first, and such a create is refused as bd would refuse
// it — never read, never copied.
func TestBdCreateBatchWithOwnerAppliesBdsPathChecksToTheOperatorsPath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.md"), []byte("## A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "x.txt"), []byte("## A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scope := filepath.Join(root, "scope")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ path, want string }{
		{"../x.md", "directory traversal"},
		{filepath.Join(root, "x.txt"), "only .md and .markdown"},
	} {
		rewrite, err := bdCreateBatchWithOwner([]string{"create", "-f", tt.path}, scope, bdBatchMarkdown, tt.path, "owner:citadel", nil)
		if err == nil || !strings.Contains(err.Error(), tt.want) || !reflect.DeepEqual(rewrite.args, []string{"create", "-f", tt.path}) {
			t.Errorf("path %q: err = %v (args %v), want a refusal mentioning %q and the argv untouched", tt.path, err, rewrite.args, tt.want)
		}
	}
	// The same file through a plain path is rewritten as before.
	rewrite, err := bdCreateBatchWithOwner([]string{"create", "-f", "../x.md"}, root, bdBatchMarkdown, filepath.Join(root, "x.md"), "owner:citadel", nil)
	if err != nil || !rewrite.carried {
		t.Fatalf("plain path: err = %v carried = %v", err, rewrite.carried)
	}
	rewrite.cleanup()
}

// TestBdGraphPlanWithOwnerUsesIDsRawLikeBd pins the Opus read's NIT 1: bd
// keys a created issue by its node "id" and wires "parent_id" as written
// (graph_apply.go:539, :1008), so gc resolves an in-plan explicit id on the
// raw bytes too — a whitespace-padded parent_id names no in-plan node and is
// read from the store, as bd would wire it.
func TestBdGraphPlanWithOwnerUsesIDsRawLikeBd(t *testing.T) {
	reads := 0
	got, carried, err := bdGraphPlanWithOwner([]byte(`{"nodes":[{"key":"root","id":"hw-root","title":"R","labels":["owner:boomtown"]},{"key":"padded","title":"P","parent_id":" hw-root "},{"key":"exact","title":"E","parent_id":"hw-root"}]}`), "owner:citadel", func(id string) (beads.Bead, error) {
		reads++
		if id != " hw-root " {
			t.Errorf("parentRead(%q): only the padded id is a store read", id)
		}
		return beads.Bead{ID: id, Labels: []string{"owner:jadegate"}}, nil
	})
	if err != nil || !carried || reads != 1 {
		t.Fatalf("carried=%v err=%v reads=%d", carried, err, reads)
	}
	if labels := planNodeLabels(t, got); !reflect.DeepEqual(labels, map[string][]string{"root": {"owner:boomtown"}, "padded": {"owner:jadegate"}, "exact": {"owner:boomtown"}}) {
		t.Errorf("labels = %v", labels)
	}
}
