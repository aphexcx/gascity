package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// planNodeLabels decodes a rewritten plan the way bd does (encoding/json,
// case-insensitive keys) and returns each node's labels by key.
func planNodeLabels(t *testing.T, plan []byte) map[string][]string {
	t.Helper()
	var decoded struct {
		Nodes []struct {
			Key    string   `json:"key"`
			Labels []string `json:"labels"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(plan, &decoded); err != nil {
		t.Fatalf("rewritten plan is not JSON: %v\n%s", err, plan)
	}
	out := make(map[string][]string, len(decoded.Nodes))
	for _, n := range decoded.Nodes {
		out[n.Key] = n.Labels
	}
	return out
}

// TestBdGraphPlanWithOwnerFollowsEveryParentRelationshipBdAccepts pins gf-c7n
// finding 1l (both gf-xid re-gates' Major at cmd_bd_owner_label_batch.go:237):
// the rewrite resolved a node's parent through parent, parent_key and
// parent_id only, while bd's graph apply (cmd/bd graph_apply.go) also makes a
// node the child of another through an `edges` entry of type parent-child
// (from = child, to = parent, each by key or id) and through a node's own
// `deps` entry of that type (target: a plan key first, then an issue id). A
// child linked either way beneath an owner:other parent got THIS city's
// owner, so it was claimable by the wrong city and refused by its parent's.
func TestBdGraphPlanWithOwnerFollowsEveryParentRelationshipBdAccepts(t *testing.T) {
	plan := `{"nodes":[
		{"key":"foreign-root","id":"hw-root","title":"R","labels":["owner:boomtown"]},
		{"key":"edge-child","title":"E"},
		{"key":"edge-child-existing","title":"EE"},
		{"key":"edge-child-by-id","title":"EI"},
		{"key":"edge-from-id","id":"hw-ef","title":"EF"},
		{"key":"dep-child","title":"D","deps":[{"type":"parent-child","target":"foreign-root"}]},
		{"key":"dep-child-existing","title":"DE","deps":[{"type":"parent-child","target":"hw-elsewhere"}]},
		{"key":"dep-child-by-id","title":"DI","deps":[{"type":"parent-child","target":"hw-root"}]},
		{"key":"blocks-only","title":"B","deps":[{"target":"foreign-root"},{"type":"blocks","target":"hw-elsewhere"}]},
		{"key":"grandchild","title":"G","parent_key":"edge-child"},
		{"key":"agreeing","title":"A","parent_key":"foreign-root","deps":[{"type":"parent-child","target":"hw-root"}]}
	],"edges":[
		{"from_key":"edge-child","to_key":"foreign-root","type":"parent-child"},
		{"from_key":"edge-child-existing","to_id":"hw-elsewhere","type":"parent-child"},
		{"from_key":"edge-child-by-id","to_id":"hw-root","type":"parent-child"},
		{"from_id":"hw-ef","to_key":"foreign-root","type":"parent-child"},
		{"from_key":"blocks-only","to_key":"foreign-root","type":"blocks"},
		{"from_key":"blocks-only","to_key":"foreign-root"}
	]}`
	reads := map[string]int{}
	got, carried, err := bdGraphPlanWithOwner([]byte(plan), "owner:citadel", func(id string) (beads.Bead, error) {
		reads[id]++
		if id != "hw-elsewhere" {
			t.Errorf("parentRead(%q): only the existing parent may be read; an in-plan explicit id never is", id)
		}
		return beads.Bead{ID: id, Labels: []string{"owner:jadegate"}}, nil
	})
	if err != nil || !carried {
		t.Fatalf("carried=%v err=%v, want every node to carry its owner", carried, err)
	}
	if reads["hw-elsewhere"] != 1 || len(reads) != 1 {
		t.Errorf("store reads = %v, want hw-elsewhere read once (memoized across the edge and the dep)", reads)
	}
	want := map[string][]string{
		"foreign-root":        {"owner:boomtown"},
		"edge-child":          {"owner:boomtown"}, // an edges entry of type parent-child, to_key
		"edge-child-existing": {"owner:jadegate"}, // to_id naming an existing bead, read first
		"edge-child-by-id":    {"owner:boomtown"}, // to_id naming an in-plan explicit id
		"edge-from-id":        {"owner:boomtown"}, // the child named by its explicit id
		"dep-child":           {"owner:boomtown"}, // a node dep of type parent-child, a plan key
		"dep-child-existing":  {"owner:jadegate"}, // a node dep naming an existing bead
		"dep-child-by-id":     {"owner:boomtown"}, // a node dep naming an in-plan explicit id
		"blocks-only":         {"owner:citadel"},  // blocks edges and deps are not parents
		"grandchild":          {"owner:boomtown"}, // under the edge-linked child, by parent_key
		"agreeing":            {"owner:boomtown"}, // two parent relationships in one lane
	}
	labels := planNodeLabels(t, got)
	if len(labels) != len(want) {
		t.Fatalf("rewritten plan has %d nodes, want %d:\n%s", len(labels), len(want), got)
	}
	for key, w := range want {
		if !reflect.DeepEqual(labels[key], w) {
			t.Errorf("node %s labels = %v, want %v", key, labels[key], w)
		}
	}

	// A node whose parent relationships name parents in DIFFERENT lanes names
	// no one lane: the create is refused before bd runs, nothing is stamped.
	for _, tt := range []struct{ name, plan string }{
		{
			"parent_key in this city's lane, a parent-child edge to a foreign in-plan parent",
			`{"nodes":[{"key":"local","title":"L"},{"key":"foreign","title":"F","labels":["owner:boomtown"]},{"key":"torn","title":"T","parent_key":"local"}],"edges":[{"from_key":"torn","to_key":"foreign","type":"parent-child"}]}`,
		},
		{
			"a parent-child dep to an existing foreign bead beside an in-plan parent of another lane",
			`{"nodes":[{"key":"foreign","title":"F","labels":["owner:boomtown"]},{"key":"torn","title":"T","parent_key":"foreign","deps":[{"type":"parent-child","target":"hw-elsewhere"}]}]}`,
		},
	} {
		out, _, err := bdGraphPlanWithOwner([]byte(tt.plan), "owner:citadel", func(id string) (beads.Bead, error) {
			return beads.Bead{ID: id, Labels: []string{"owner:jadegate"}}, nil
		})
		if err == nil || out != nil || !strings.Contains(err.Error(), `"torn"`) {
			t.Errorf("%s: got (%s, %v), want a refusal naming node torn", tt.name, out, err)
		}
	}
	// An authored owner resolves the node whatever its parents' lanes.
	authored := `{"nodes":[{"key":"local","title":"L"},{"key":"foreign","title":"F","labels":["owner:boomtown"]},{"key":"own","title":"T","parent_key":"local","labels":["owner:jadegate"]}],"edges":[{"from_key":"own","to_key":"foreign","type":"parent-child"}]}`
	if out, carried, err := bdGraphPlanWithOwner([]byte(authored), "owner:citadel", nil); err != nil || !carried || !reflect.DeepEqual(planNodeLabels(t, out)["own"], []string{"owner:jadegate"}) {
		t.Errorf("authored owner under parents of other lanes: carried=%v err=%v labels=%v", carried, err, planNodeLabels(t, out))
	}

	// With no store to read (a database override) a child of an existing
	// parent through an edge or a dep is left as authored, like one through
	// parent_id, and so is its in-plan subtree without an owner of its own.
	unread := `{"nodes":[
		{"key":"edge-child","title":"E"},
		{"key":"under-edge","title":"UE","parent_key":"edge-child"},
		{"key":"dep-child","title":"D","deps":[{"type":"parent-child","target":"hw-elsewhere"}]},
		{"key":"root","title":"R"}
	],"edges":[{"from_key":"edge-child","to_id":"hw-elsewhere","type":"parent-child"}]}`
	out, carried, err := bdGraphPlanWithOwner([]byte(unread), "owner:citadel", nil)
	if err != nil || carried {
		t.Fatalf("unread parents: carried=%v err=%v, want the subtree under an unread parent to carry nothing", carried, err)
	}
	for key, w := range map[string][]string{"edge-child": nil, "under-edge": nil, "dep-child": nil, "root": {"owner:citadel"}} {
		if got := planNodeLabels(t, out)[key]; !reflect.DeepEqual(got, w) {
			t.Errorf("unread parents: node %s labels = %v, want %v", key, got, w)
		}
	}
}
