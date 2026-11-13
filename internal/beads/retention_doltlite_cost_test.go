//go:build gascity_native_beads

package beads_test

import "github.com/gastownhall/gascity/internal/beads"

func init() { retentionExtraFixtures["doltlite"] = beads.NewDoltliteRetentionFixtureForTest }
