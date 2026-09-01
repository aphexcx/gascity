package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/federation"
)

// ApplyGraphPlan creates a bead graph via a single hidden bd command so the
// full graph becomes visible only after the underlying transaction commits.
func (s *BdStore) ApplyGraphPlan(ctx context.Context, plan *GraphApplyPlan) (*GraphApplyResult, error) {
	return s.ApplyGraphPlanWithStorage(ctx, plan, StorageDefault)
}

// ApplyGraphPlanWithStorage creates a bead graph in a storage tier selected by
// policy middleware.
func (s *BdStore) ApplyGraphPlanWithStorage(_ context.Context, plan *GraphApplyPlan, storage StorageClass) (*GraphApplyResult, error) {
	if plan == nil {
		return nil, fmt.Errorf("graph apply plan is nil")
	}
	ephemeral, noHistory, err := graphStorageFlags(storage)
	if err != nil {
		return nil, fmt.Errorf("bd create --graph: %w", err)
	}

	// A graph apply is a bulk create, so the owner label this store stamps on
	// a single create lands on every node here too — on a copy, since the
	// plan is the caller's and ValidateGraphApplyResult reads it back.
	data, err := json.Marshal(s.stampGraphPlanOwner(plan))
	if err != nil {
		return nil, fmt.Errorf("marshaling graph apply plan: %w", err)
	}

	tmpDir := filepath.Join(s.dir, ".gc", "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating graph apply temp dir: %w", err)
	}

	f, err := os.CreateTemp(tmpDir, "graph-apply-*.json")
	if err != nil {
		return nil, fmt.Errorf("creating graph apply temp file: %w", err)
	}
	tmpPath := f.Name()
	defer os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("writing graph apply temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("closing graph apply temp file: %w", err)
	}

	args := []string{"create", "--graph", tmpPath, "--json"}
	if ephemeral {
		args = append(args, "--ephemeral")
	}
	if noHistory {
		args = append(args, "--no-history")
	}
	out, err := s.runner(s.dir, "bd", args...)
	if err != nil {
		return nil, fmt.Errorf("bd create --graph: %w", err)
	}

	var result GraphApplyResult
	if err := json.Unmarshal(extractJSON(out), &result); err != nil {
		return nil, fmt.Errorf("bd create --graph: parsing JSON: %w", err)
	}
	if err := ValidateGraphApplyResult(plan, &result); err != nil {
		return nil, fmt.Errorf("bd create --graph: %w", err)
	}
	return &result, nil
}

func graphStorageFlags(storage StorageClass) (ephemeral bool, noHistory bool, err error) {
	switch storage {
	case StorageDefault, StorageHistory:
		return false, false, nil
	case StorageNoHistory:
		return false, true, nil
	case StorageEphemeral:
		return true, false, nil
	default:
		return false, false, fmt.Errorf("unknown storage class %q", storage)
	}
}

// SupportsEphemeralGraphApply reports whether this store can apply a whole
// graph directly into ephemeral storage.
func (s *BdStore) SupportsEphemeralGraphApply() bool {
	return true
}

// stampGraphPlanOwner returns plan with the store's owner label on every node
// that names no owner of its own, or plan itself when there is no owner label
// to stamp. The caller's plan is never mutated.
func (s *BdStore) stampGraphPlanOwner(plan *GraphApplyPlan) *GraphApplyPlan {
	if s.ownerLabel == "" || plan == nil {
		return plan
	}
	stamped := *plan
	stamped.Nodes = make([]GraphApplyNode, len(plan.Nodes))
	labels := federation.OwnerLabelsForPlan(graphPlanOwnerNodes(plan, s.ownerParentLabels), s.ownerLabel)
	for i, node := range plan.Nodes {
		node.Labels = labels[i]
		stamped.Nodes[i] = node
	}
	return &stamped
}

// graphPlanOwnerNodes projects a plan onto the owner rule's inputs: each
// node's authored labels, its in-plan parent by key, and the labels of an
// existing parent bead read through parentLabels (memoized per id).
func graphPlanOwnerNodes(plan *GraphApplyPlan, parentLabels func(id string) []string) []federation.PlanNode {
	index := make(map[string]int, len(plan.Nodes))
	for i, node := range plan.Nodes {
		if node.Key != "" {
			index[node.Key] = i
		}
	}
	read := make(map[string][]string)
	nodes := make([]federation.PlanNode, len(plan.Nodes))
	for i, node := range plan.Nodes {
		pn := federation.PlanNode{Labels: node.Labels, ParentIndex: -1}
		if node.ParentKey != "" {
			if p, ok := index[node.ParentKey]; ok {
				pn.ParentIndex = p
			}
		} else if id := strings.TrimSpace(node.ParentID); id != "" && parentLabels != nil {
			if _, seen := read[id]; !seen {
				read[id] = parentLabels(id)
			}
			pn.ParentLabels = read[id]
		}
		nodes[i] = pn
	}
	return nodes
}
