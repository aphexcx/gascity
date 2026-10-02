package beads

import (
	"encoding/json"
	"fmt"
	"strings"
)

var _ AssignmentConditionalWriter = (*BdStore)(nil)

// ReadAssignment reads the authoritative status, owner, and deferral state.
func (s *BdStore) ReadAssignment(id string) (Bead, error) {
	issue, err := s.assignmentIssue(id)
	if err != nil {
		return Bead{}, err
	}
	return issue.toBead(), nil
}

// UpdateIfAssignmentMatches uses bd's atomic status/assignee guards, available
// on schemas without revision tokens. Read the raw status from bd because SDK
// reads normalize blocked/deferred to open; guarding on that normalized value
// would either reject valid holds or accept a different underlying state.
func (s *BdStore) UpdateIfAssignmentMatches(id string, expected Bead, opts UpdateOpts) (bool, error) {
	if opts.ParentID != nil || len(opts.Labels) != 0 || len(opts.RemoveLabels) != 0 {
		return false, fmt.Errorf("assignment update cannot guard parent or label edits: %w", ErrConditionalWriteUnsupported)
	}
	issue, err := s.assignmentIssue(id)
	if err != nil {
		return false, err
	}
	current := issue.toBead()
	if current.Status != expected.Status || current.Assignee != expected.Assignee ||
		current.IndefinitelyDeferred != expected.IndefinitelyDeferred ||
		(current.DeferUntil == nil) != (expected.DeferUntil == nil) ||
		(current.DeferUntil != nil && !current.DeferUntil.Equal(*expected.DeferUntil)) {
		return false, nil
	}
	args := bdUpdateArgs(id, opts)
	if len(args) == 3 {
		return false, fmt.Errorf("assignment update for %q has no field edits", id)
	}
	args = append(args, "--if-status", issue.Status, "--if-assignee", issue.Assignee)
	// Do not retry an ambiguous write: a later attempt cannot prove whether
	// this attempt committed. Every mutation carries both backend guards.
	out, err := s.runner(s.dir, "bd", s.bdTransientWriteArgs(args)...)
	if err == nil {
		return true, nil
	}
	if bdExitCode(err) == bdCASPreconditionExitCode {
		return false, nil
	}
	detail := strings.TrimSpace(string(out)) + " " + err.Error()
	if isBdUnknownFlagError(detail, "--if-assignee") || isBdUnknownFlagError(detail, "--if-status") {
		return false, fmt.Errorf("bd assignment guards unavailable for %q: %w (%s)", id, ErrConditionalWriteUnsupported, strings.TrimSpace(detail))
	}
	return false, fmt.Errorf("conditionally updating assignment of %q: %w", id, err)
}

// assignmentIssue preserves the raw provider status and requires exact ID
// resolution before issuing a guarded mutation. Read errors fail closed.
func (s *BdStore) assignmentIssue(id string) (bdIssue, error) {
	out, err := s.runBDTransientRead("show", "--json", id)
	if err != nil && isBdNotFound(err) && isWispQueryableID(id) {
		out, err = s.runBDTransientRead("query", "--json", "ephemeral=true AND id="+id, "--all", "--limit", "1")
	}
	if err != nil {
		return bdIssue{}, fmt.Errorf("reading assignment of %q: %w", id, err)
	}
	var issues []bdIssue
	if err := json.Unmarshal(extractJSON(out), &issues); err != nil {
		return bdIssue{}, fmt.Errorf("reading assignment of %q: parsing JSON: %w", id, err)
	}
	if len(issues) == 0 {
		return bdIssue{}, fmt.Errorf("reading assignment of %q: %w", id, ErrNotFound)
	}
	if len(issues) != 1 || issues[0].ID != id {
		return bdIssue{}, fmt.Errorf("reading assignment of %q (resolved to %q): %w", id, issues[0].ID, ErrIDCollision)
	}
	if issues[0].Status == "" {
		return bdIssue{}, fmt.Errorf("reading assignment of %q: missing status", id)
	}
	return issues[0], nil
}
