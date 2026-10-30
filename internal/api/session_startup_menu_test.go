package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestSessionResponseShowsStartupMenu(t *testing.T) {
	sp := runtime.NewFake()
	sp.SetPeekOutput("worker", "Hooks need review\n› 1. Review hooks\n  2. Trust all and continue\n  3. Continue without trusting\nPress enter to confirm")
	info := session.Info{ID: "s-1", State: session.StateActive, SessionName: "worker"}
	response := sessionResponseWithReason(info, session.PersistedResponse{Status: "open", Metadata: map[string]string{"state": "active"}}, nil, sp, false)
	if response.State != "parked" || response.Reason != "Hooks need review" {
		t.Fatalf("state=%q reason=%q, want parked with menu heading", response.State, response.Reason)
	}
}
