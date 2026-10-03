package beads

// AssignmentConditionalWriter updates fields only while status and assignee
// still match the expected snapshot. The comparison and mutation are atomic;
// false means the precondition failed and nothing was written. This is a
// narrower guarantee than revision CAS: concurrent metadata edits do not cause
// a conflict. Implementations also reject a deferral change observed while
// obtaining the authoritative assignment, but do not fence deferral timestamps.
// Label and parent edits are unsupported because not every backend can include
// those edits in its guarded transaction.
type AssignmentConditionalWriter interface {
	// ReadAssignment returns an authoritative snapshot including deferral
	// state, even when the store's optimized Get projection omits those fields.
	ReadAssignment(id string) (Bead, error)
	UpdateIfAssignmentMatches(id string, expected Bead, opts UpdateOpts) (bool, error)
}

// AssignmentConditionalWriterFor resolves the optional atomic assignment
// capability, following only explicitly declared wrapper resolution targets.
// There is no unconditional fallback or revision-CAS capability promotion.
func AssignmentConditionalWriterFor(store Store) (AssignmentConditionalWriter, bool) {
	if store == nil {
		return nil, false
	}
	writer, ok := followConditionalWritesResolveTarget(store).(AssignmentConditionalWriter)
	return writer, ok
}
