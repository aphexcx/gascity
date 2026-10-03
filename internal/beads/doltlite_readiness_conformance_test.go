//go:build gascity_native_beads

package beads_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

func TestDoltliteReadStoreShippedReadyConformance(t *testing.T) {
	beadstest.RunShippedReadyConformance(t, func() beads.Store {
		return beads.NewDoltliteReadinessFixture(t)
	})
}
