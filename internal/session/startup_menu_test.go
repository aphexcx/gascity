package session

import "testing"

func TestStartupMenuDisplayPreservesLifecycle(t *testing.T) {
	for _, state := range []State{StateActive, StateAsleep, StateClosed} {
		info := Info{State: state}
		got, reason := DisplayWithStartupMenu(info, "session", "Hooks need review")
		if state == StateActive {
			if got != "parked" || reason != "Hooks need review" {
				t.Fatalf("display=%q reason=%q", got, reason)
			}
		} else if got != string(state) || reason != "session" {
			t.Fatalf("inactive state changed: %q %q", got, reason)
		}
		if info.State != state {
			t.Fatal("display mutated lifecycle")
		}
	}
}
