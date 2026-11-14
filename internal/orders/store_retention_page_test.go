package orders

import (
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

type boundedRetentionSpy struct{ *listSpyStore }

func (s boundedRetentionSpy) BoundsListPages(beads.ListQuery) bool { return true }

// Retention must visit every row across equal-timestamp page boundaries without
// asking the backing store for an unlimited result.
func TestClosedRetentionReadsBoundedPages(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const count = 2503
	var rows []beads.Bead
	for i := 0; i < count; i++ {
		rows = append(rows, beads.Bead{ID: fmt.Sprintf("test-%05d", i), Title: "order:sample", Status: "closed", CreatedAt: at, UpdatedAt: at, Labels: []string{"order-tracking", "order-run:sample"}, Ephemeral: i%2 == 0})
	}
	mem := beads.NewMemStoreFrom(count, rows, nil)
	spy := boundedRetentionSpy{&listSpyStore{Store: mem}}
	runs, err := NewStore(beads.OrdersStore{Store: spy}).ClosedRunsForRetention()
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, run := range runs {
		if seen[run.ID] {
			t.Fatalf("duplicate %s", run.ID)
		}
		seen[run.ID] = true
	}
	if len(seen) != count {
		t.Fatalf("got %d rows, want %d", len(seen), count)
	}
	for i, q := range spy.queries {
		if q.Limit <= 0 || q.Limit > 1000 {
			t.Fatalf("query %d has unbounded limit %d", i, q.Limit)
		}
	}
}
