package beads

import (
	"fmt"
	"slices"
)

// RowDecodeError identifies a row omitted from an otherwise complete list.
// It does not authorize deletion: a safety check must retain the row's root.
type RowDecodeError struct {
	ID  string
	Err error
}

// Error reports the omitted row and the decoding failure.
func (e *RowDecodeError) Error() string { return fmt.Sprintf("decoding bead %q: %v", e.ID, e.Err) }

// Unwrap exposes the underlying decoding error.
func (e *RowDecodeError) Unwrap() error { return e.Err }

// SkippedListRowIDs returns the unique omitted IDs only when every error leaf
// is a row-decoding warning. Mixed transport/tier failures remain fatal even
// when they also wrap a PartialResultError or a row-decoding warning.
func SkippedListRowIDs(err error) []string {
	ids := make(map[string]struct{})
	var collect func(error) bool
	collect = func(err error) bool {
		switch e := err.(type) { //nolint:errorlint // Walk every error branch; errors.As could hide a joined transport failure.
		case *RowDecodeError:
			if e.ID == "" {
				return false
			}
			ids[e.ID] = struct{}{}
			return true
		case interface{ Unwrap() []error }:
			for _, child := range e.Unwrap() {
				if !collect(child) {
					return false
				}
			}
			return true
		case interface{ Unwrap() error }:
			return collect(e.Unwrap())
		default:
			return false
		}
	}
	if !collect(err) {
		return nil
	}
	var out []string
	for id := range ids {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}
