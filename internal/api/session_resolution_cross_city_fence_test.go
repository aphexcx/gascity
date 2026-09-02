package api

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The API-side retired-session re-home (named-session continuity) is an
// ownership write: on a federated city it must not hand another city's bead to
// the replacement session (cross-city claim fence, jg-66rdw8), while own and
// legacy work still moves, and a non-federated city is untouched by the fence.
func TestReassignOpenWorkAssignedToSessionRefusesAnotherCitysBead(t *testing.T) {
	var logBuf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prev) })

	store := beads.NewMemStore()
	mk := func(labels ...string) beads.Bead {
		b, err := store.Create(beads.Bead{Title: "work", Type: "task", Status: "in_progress", Assignee: "retired-session", Labels: labels})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return b
	}
	foreign, own, legacy, handed := mk("owner:citadel"), mk("owner:jadegate"), mk(), mk("owner:citadel", "handoff:jadegate")

	if err := reassignOpenWorkAssignedToSession(store, "retired-session", "replacement", "jadegate"); err != nil {
		t.Fatalf("reassign: %v", err)
	}
	if got, _ := store.Get(foreign.ID); got.Assignee != "retired-session" {
		t.Fatalf("REGRESSION jg-66rdw8: another city's bead re-homed onto %q", got.Assignee)
	}
	for _, id := range []string{own.ID, legacy.ID, handed.ID} {
		if got, _ := store.Get(id); got.Assignee != "replacement" {
			t.Fatalf("%s not re-homed: assignee=%q", id, got.Assignee)
		}
	}
	for _, want := range []string{"cross-city-fence refused", "bead=" + foreign.ID, "owner=citadel", "this_identity=jadegate"} {
		if !strings.Contains(logBuf.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logBuf.String())
		}
	}

	off := beads.NewMemStore()
	b, _ := off.Create(beads.Bead{Title: "work", Type: "task", Status: "in_progress", Assignee: "retired-session", Labels: []string{"owner:citadel"}})
	if err := reassignOpenWorkAssignedToSession(off, "retired-session", "replacement", ""); err != nil {
		t.Fatalf("reassign (not federated): %v", err)
	}
	if got, _ := off.Get(b.ID); got.Assignee != "replacement" {
		t.Fatalf("not federated: fence must be off, got assignee=%q", got.Assignee)
	}
}
