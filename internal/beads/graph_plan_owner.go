package beads

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/federation"
)

// GraphPlanParent is one parent relationship of a bulk create's node: an
// in-plan node by its index (Index >= 0), or an existing bead by its id.
type GraphPlanParent struct {
	Index int
	ID    string
}

// GraphPlanParents returns, per node of plan, every parent relationship bd's
// graph apply accepts for a typed plan, in the order bd wires them (cmd/bd
// graph_apply.go: the node's own parent first, then the edges): the node's
// parent_key, else its parent_id (bd's effective parent — parent_key wins);
// then every edge of type parent-child whose child is the node (from_key;
// a from_id names an existing bead, not a plan node — a typed plan node has
// no explicit id), its parent to_id (an existing bead) or else to_key. A
// relationship naming the node itself, or a key bd would refuse as unknown,
// is skipped.
func GraphPlanParents(plan *GraphApplyPlan) [][]GraphPlanParent {
	if plan == nil {
		return nil
	}
	index := make(map[string]int, len(plan.Nodes))
	for i, node := range plan.Nodes {
		if node.Key != "" {
			index[node.Key] = i
		}
	}
	parents := make([][]GraphPlanParent, len(plan.Nodes))
	add := func(child int, parent GraphPlanParent, ok bool) {
		if !ok || parent.Index == child {
			return
		}
		parents[child] = append(parents[child], parent)
	}
	for i, node := range plan.Nodes {
		switch {
		case node.ParentKey != "":
			p, ok := index[node.ParentKey]
			add(i, GraphPlanParent{Index: p}, ok)
		case strings.TrimSpace(node.ParentID) != "":
			add(i, GraphPlanParent{Index: -1, ID: strings.TrimSpace(node.ParentID)}, true)
		}
	}
	for _, edge := range plan.Edges {
		if edge.Type != "parent-child" || edge.FromKey == "" {
			continue
		}
		child, ok := index[edge.FromKey]
		if !ok {
			continue
		}
		switch {
		case edge.ToID != "":
			add(child, GraphPlanParent{Index: -1, ID: edge.ToID}, true)
		case edge.ToKey != "":
			p, ok := index[edge.ToKey]
			add(child, GraphPlanParent{Index: p}, ok)
		}
	}
	return parents
}

// GraphPlanOwnerLabels resolves the owner label of every node of a bulk
// create from its authored labels and its parent relationships — the
// per-node rule of a single create (federation.OwnerLabelForChild) applied in
// dependency order, as federation.OwnerLabelsForPlan applies it for one
// parent: an authored owner wins; a node under in-plan parents takes their
// EFFECTIVE owner; a node under an existing parent takes that bead's owner,
// read through parentLabels (memoized per id; a read error refuses the
// create); every other node carries owner. A node whose parent relationships
// resolve to DIFFERENT lanes names no one lane: the create is refused,
// naming the node by names[i]. A nil parentLabels means the store cannot be
// read for this create: a node under an existing parent is left as authored,
// and so is every in-plan descendant of it without an owner of its own —
// their lane is the unread parent's, which stamping this city's owner would
// hand to the wrong city — and unresolved reports those nodes.
func GraphPlanOwnerLabels(labels [][]string, parents [][]GraphPlanParent, names []string, owner string, parentLabels func(id string) ([]string, error)) (out [][]string, unresolved []bool, err error) {
	n := len(labels)
	read := make(map[string][]string)
	unresolved = make([]bool, n)
	nodes := make([]federation.PlanNode, n)
	for i := 0; i < n; i++ {
		pn := federation.PlanNode{Labels: labels[i], ParentIndex: -1}
		for j, parent := range parentsOf(parents, i) {
			if parent.Index < 0 {
				if parentLabels == nil {
					unresolved[i] = !federation.HasOwnerLabel(labels[i])
					continue
				}
				if _, seen := read[parent.ID]; !seen {
					got, err := parentLabels(parent.ID)
					if err != nil {
						return nil, nil, fmt.Errorf("reading parent %q for the owner label: %w", parent.ID, err)
					}
					read[parent.ID] = got
				}
			}
			if j > 0 {
				continue // the first relationship resolves the node; the rest must agree below
			}
			if parent.Index >= 0 {
				pn.ParentIndex = parent.Index
			} else {
				pn.ParentLabels = read[parent.ID]
			}
		}
		nodes[i] = pn
	}
	// An unresolved lane reaches every in-plan descendant that names no owner
	// of its own, through any parent relationship.
	for changed := true; changed; {
		changed = false
		for i := 0; i < n; i++ {
			if unresolved[i] || federation.HasOwnerLabel(labels[i]) {
				continue
			}
			for _, parent := range parentsOf(parents, i) {
				if parent.Index >= 0 && parent.Index < n && unresolved[parent.Index] {
					unresolved[i], changed = true, true
					break
				}
			}
		}
	}
	out = federation.OwnerLabelsForPlan(nodes, owner)
	thisLane := federation.OwnerOf([]string{owner})
	lane := func(l []string) string {
		if o := federation.OwnerOf(l); o != "" {
			return o
		}
		return thisLane
	}
	for i := 0; i < n; i++ {
		if unresolved[i] {
			out[i] = labels[i]
			continue
		}
		if federation.HasOwnerLabel(labels[i]) {
			continue
		}
		for _, parent := range parentsOf(parents, i)[min(1, len(parentsOf(parents, i))):] {
			parentLane := ""
			if parent.Index >= 0 && parent.Index < n {
				parentLane = lane(out[parent.Index])
			} else {
				parentLane = lane(read[parent.ID])
			}
			if nodeLane := lane(out[i]); parentLane != nodeLane {
				name := fmt.Sprintf("%d", i)
				if i < len(names) && names[i] != "" {
					name = names[i]
				}
				return nil, nil, fmt.Errorf("node %q names parents in different lanes (owner %q and owner %q): the owner label cannot be resolved", name, nodeLane, parentLane)
			}
		}
	}
	return out, unresolved, nil
}

func parentsOf(parents [][]GraphPlanParent, i int) []GraphPlanParent {
	if i < len(parents) {
		return parents[i]
	}
	return nil
}
