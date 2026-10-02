//go:build gascity_native_beads

package beads

// UpdateIfAssignmentMatches delegates to bd's atomic assignment guards. Unlike
// revision CAS, these guards do not need a revision from the SQL read snapshot.
func (s *DoltliteReadStore) UpdateIfAssignmentMatches(id string, expected Bead, opts UpdateOpts) (bool, error) {
	updated, err := s.BdStore.UpdateIfAssignmentMatches(id, expected, opts)
	if updated || err != nil {
		s.resetOrderRunCache()
	}
	return updated, err
}
