package beads

// ReadAssignment bypasses cached projections so callers validate complete,
// authoritative hold state before attempting an assignment update.
func (c *CachingStore) ReadAssignment(id string) (Bead, error) {
	writer, ok := AssignmentConditionalWriterFor(c.conditionalBacking())
	if !ok {
		return Bead{}, ErrConditionalWriteUnsupported
	}
	return writer.ReadAssignment(id)
}

// UpdateIfAssignmentMatches forwards the narrow assignment capability while
// preserving conditional-write cache eviction and change notifications.
func (c *CachingStore) UpdateIfAssignmentMatches(id string, expected Bead, opts UpdateOpts) (bool, error) {
	writer, ok := AssignmentConditionalWriterFor(c.conditionalBacking())
	if !ok {
		return false, ErrConditionalWriteUnsupported
	}
	updated, err := writer.UpdateIfAssignmentMatches(id, expected, opts)
	if err != nil {
		c.applyConditionalWriteFailure(id, err)
		return updated, err
	}
	if !updated {
		c.evictForConditionalWrite(id)
		return false, nil
	}
	fresh, refreshed := c.refreshBeadAfterWrite(id, "refresh bead after conditional assignment update")
	c.evictForConditionalWrite(id)
	if refreshed {
		c.notifyChange("bead.updated", fresh)
	}
	return true, nil
}
