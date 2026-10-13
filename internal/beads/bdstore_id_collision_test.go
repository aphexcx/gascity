package beads_test

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestGetReportsWhatBdResolvedOnACollision (gf-d7b, the lane's codex round 2
// on finding 1n): when bd's resolver answers a Get for one id with a bead of
// another, the guard's refusal carries BOTH ids as a beads.IDCollisionError —
// so a caller that asked bd to resolve an abbreviation on purpose (the way bd
// itself resolves a `--deps` target) can fetch the resolved bead exactly —
// while errors.Is(err, ErrIDCollision) and errors.Is(err, ErrNotFound) stay
// true for every caller that treats the collision as "absent".
func TestGetReportsWhatBdResolvedOnACollision(t *testing.T) {
	runner := &releaseVerbRunner{
		show: func(string) ([]byte, error) { return []byte(`[{"id":"bd-42-wisp-7"}]`), nil },
	}
	_, err := beads.NewBdStore("/city", runner.run).Get("bd-42")
	var collision *beads.IDCollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("Get = %v, want a *beads.IDCollisionError", err)
	}
	if collision.Requested != "bd-42" || collision.Resolved != "bd-42-wisp-7" {
		t.Fatalf("collision = %+v, want requested bd-42 resolved bd-42-wisp-7", collision)
	}
	if !errors.Is(err, beads.ErrIDCollision) || !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("err = %v, want it to wrap ErrIDCollision and ErrNotFound", err)
	}
}
