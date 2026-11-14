package beadstest

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// RetentionRows supplies the shared small, equal-timestamp, and skewed-history
// fixtures used to compare backing fetch cost across adapters and callers.
func RetentionRows(n, groups int) []beads.Bead {
	counts := make([]int, groups)
	for i := 0; i < n; i++ {
		counts[i%groups]++
	}
	if n == 61988 && groups == 67 {
		copy(counts, []int{4957, 3432, 1719, 1624, 1392})
		remaining := n
		for _, count := range counts[:5] {
			remaining -= count
		}
		for i := 5; i < groups; i++ {
			counts[i] = min(800, remaining)
			remaining -= counts[i]
		}
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := make([]beads.Bead, 0, n)
	for group, count := range counts {
		name := fmt.Sprintf("sample-%02d", group)
		for i := 0; i < count; i++ {
			created := at
			if groups > 1 {
				created = at.Add(-time.Duration(i) * time.Hour)
			}
			rows = append(rows, beads.Bead{ID: fmt.Sprintf("sample-%06d", len(rows)), Title: "order:" + name, Status: "closed", Type: "task", CreatedAt: created, Ephemeral: true, Labels: []string{"order-tracking", "order-run:" + name}})
		}
	}
	return rows
}
