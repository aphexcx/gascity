package proctable

import "testing"

func TestHasSessionProcessRequiresIdentity(t *testing.T) {
	if present, err := HasSessionProcess(""); present || err == nil {
		t.Fatalf("empty identity lookup = %v, %v; must not scan all sessions", present, err)
	}
}
