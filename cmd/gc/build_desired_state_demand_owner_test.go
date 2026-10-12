package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/federation"
)

// TestDefaultScaleCheckCountsAndDemandAppliesTheOwnershipFence pins the
// gf-4c9 round-2 gate finding at cmd/gc/build_desired_state.go:2048 (finding
// 1c): the in-process demand counter excluded parked work but never applied
// federation.MayClaim, so another city's routed task (owner:jadegate, no
// handoff) counted as this city's demand, a seat was minted for it, the
// hook's claim fence refused the very row that started the session, and the
// pool cycled start/drain forever. The counter now applies the same rule the
// fence applies: foreign-owned work is not demand; own, unowned and
// handed-off work still is; without an identity the fence is off.
func TestDefaultScaleCheckCountsAndDemandAppliesTheOwnershipFence(t *testing.T) {
	const template = "hello-world/worker"
	foreign, _ := federation.OwnerLabel("jadegate")
	own, _ := federation.OwnerLabel("citadel")
	tests := []struct {
		name     string
		identity string
		labels   []string
		want     int
	}{
		{"foreign-owned, no handoff: not demand", "citadel", []string{foreign}, 0},
		{"own work counts", "citadel", []string{own}, 1},
		{"unowned work counts", "citadel", nil, 1},
		{"foreign-owned with a handoff to this city counts", "citadel", []string{foreign, federation.HandoffLabel("citadel")}, 1},
		{"no identity: the fence is off, foreign counts", "", []string{foreign}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.City{
				Workspace:  config.Workspace{Name: "test-city"},
				Federation: config.FederationConfig{Identity: tt.identity},
				Agents: []config.Agent{
					{Name: "worker", Dir: "hello-world"},
				},
			}
			store := beads.NewMemStore()
			work, err := store.Create(beads.Bead{
				Title:    "routed task",
				Type:     "task",
				Status:   "open",
				Labels:   tt.labels,
				Metadata: map[string]string{beadmeta.RoutedToMetadataKey: template},
			})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			counts, demand, partial, errs := defaultScaleCheckCountsAndDemand(cfg, []defaultScaleCheckTarget{{
				template: template,
				storeKey: "rig:hello-world",
				store:    store,
			}}, newWorkStartDeferralPass(time.Now(), nil))
			if len(errs) != 0 || len(partial) != 0 {
				t.Fatalf("errs = %v, partial = %v", errs, partial)
			}
			if got := counts[template]; got != tt.want {
				t.Fatalf("counts[%q] = %d, want %d (labels %v, identity %q)", template, got, tt.want, tt.labels, tt.identity)
			}
			ids := demand[template].WorkBeadIDs
			if tt.want == 0 && len(ids) != 0 {
				t.Fatalf("a foreign-owned row was listed as demand: %v", ids)
			}
			if tt.want == 1 && (len(ids) != 1 || ids[0] != work.ID) {
				t.Fatalf("WorkBeadIDs = %v, want [%s]", ids, work.ID)
			}
		})
	}
}
