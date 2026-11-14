package beads

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

func TestSkippedListRowIDsRejectsIncompleteReads(t *testing.T) {
	decode := &RowDecodeError{ID: "bad", Err: errors.New("metadata is not an object")}
	for _, test := range []struct {
		name string
		err  error
		want []string
	}{
		{"wrapped", fmt.Errorf("retention: %w", &PartialResultError{Op: "list", Err: errors.Join(decode, decode)}), []string{"bad"}},
		{"mixed", errors.Join(decode, errors.New("tier unavailable")), nil},
		{"partial transport", &PartialResultError{Op: "list", Err: errors.New("tier unavailable")}, nil},
		{"nil", nil, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := SkippedListRowIDs(test.err); !slices.Equal(got, test.want) {
				t.Fatalf("IDs=%v, want %v", got, test.want)
			}
		})
	}
}
