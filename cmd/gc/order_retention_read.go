package main

import (
	"log"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// retentionRowWarnings collects identified decode failures across history and
// descendant reads. It accepts no other partial failure: a failed tier or
// interrupted query cannot establish the retention floor.
type retentionRowWarnings map[string]struct{}

func (w retentionRowWarnings) accept(err error) bool {
	if err == nil {
		return true
	}
	ids := beads.SkippedListRowIDs(err)
	if len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		w[id] = struct{}{}
	}
	return true
}

func (w retentionRowWarnings) report() {
	if len(w) == 0 {
		return
	}
	ids := make([]string, 0, len(w))
	for id := range w {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	log.Printf("order-tracking retention: skipped %d unreadable row(s); their roots are retained when descendant safety cannot be established: %s", len(ids), strings.Join(ids, ","))
}
