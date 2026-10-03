package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestPoolDemandShippedReleaseDoesNotCreateDuplicate(t *testing.T) {
	const template = "builder"
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	store := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID: "work-1", Type: "task", Status: "open",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:    template,
			beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeShipped,
		},
	}}, nil)
	counts, demand, _, errs := defaultScaleCheckCountsAndDemand(nil, []defaultScaleCheckTarget{{
		template: template, storeKey: "city", store: store,
	}}, newWorkStartDeferralPass(now, nil))
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if counts[template] != 0 || len(demand[template].WorkBeadIDs) != 0 {
		t.Fatalf("shipped release reentered demand: counts=%v demand=%v", counts, demand)
	}
}

func TestPoolDemandDrainedSessionDoesNotConsumeCapacity(t *testing.T) {
	for _, state := range []string{"drained", "asleep"} {
		t.Run(state, func(t *testing.T) {
			cfg := &config.City{Agents: []config.Agent{poolAgent("builder", "", intPtr(1), 0)}}
			stale := sessionBead("session-old", "open")
			stale.Metadata = map[string]string{
				"template": "builder", "session_name": "builder-old",
				"state": state, "sleep_reason": "drained",
				poolManagedMetadataKey: "true",
				"pending_create_claim": "true",
			}
			states := ComputePoolDesiredStates(cfg, nil, nil, sessionInfosFromBeads([]beads.Bead{stale}), map[string]int{"builder": 1})
			if len(states) != 1 || len(states[0].Requests) != 1 {
				t.Fatalf("desired=%+v, want one new request within max=1", states)
			}
			request := states[0].Requests[0]
			if request.Tier != "new" || request.SessionBeadID != "" {
				t.Fatalf("request=%+v, want fresh capacity without drained holder", request)
			}
		})
	}
}

func TestPoolDemandFreshWakePreservesUsageLimitBackoff(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	agent := poolAgent("builder", "", intPtr(1), 0)
	agent.WakeMode = "fresh"
	cfg := &config.City{Agents: []config.Agent{agent}}
	seat := sessionBead("session-limited", "open")
	seat.Metadata = map[string]string{
		"template": "builder", "session_name": "builder-limited",
		"state": "asleep", "sleep_reason": "rate_limit",
		"quarantined_until":    now.Add(30 * time.Minute).Format(time.RFC3339),
		poolManagedMetadataKey: "true",
	}
	work := []beads.Bead{workBead("work-limited", "builder", seat.ID, "in_progress", 2)}
	for _, tc := range []struct {
		name        string
		at          time.Time
		wantSession string
	}{
		{"during backoff", now, seat.ID},
		{"after backoff", now.Add(30 * time.Minute), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			states := ComputePoolDesiredStatesAt(cfg, work, sessionInfosFromBeads([]beads.Bead{seat}), nil, tc.at)
			if len(states) != 1 || len(states[0].Requests) != 1 {
				t.Fatalf("desired=%+v, want one request for owned work", states)
			}
			if request := states[0].Requests[0]; request.SessionBeadID != tc.wantSession {
				t.Fatalf("request=%+v, want session %q so fresh wake respects backoff", request, tc.wantSession)
			}
		})
	}
}
