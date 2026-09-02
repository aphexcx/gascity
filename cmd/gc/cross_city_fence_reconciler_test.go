package main

import (
	"bytes"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/federation"
	"github.com/gastownhall/gascity/internal/session"
)

// Reconciler side of the cross-city claim fence (jg-66rdw8 / gp-0uj).
//
// On a federated store another city's in_progress bead is assigned to a session
// this city has never heard of — exactly the orphan shape
// releaseOrphanedPoolAssignments exists to repair. Reopening it here is the
// re-route that fed the hw-57b63 loop every lease expiry: reopen → counted as
// demand → a seat spawns → its startup claim takes the bead. The same
// federation.MayClaim the hook applies must decide here, and log the same line.

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func fenceReconcilerCfg(identity string) *config.City {
	return &config.City{
		Agents:     []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}},
		Federation: config.FederationConfig{Identity: identity},
	}
}

// orphanedWork creates an in_progress pool-routed bead assigned to a session no
// open session bead names — from this city's point of view, an orphan.
func orphanedWork(t *testing.T, store beads.Store, assignee string, labels ...string) beads.Bead {
	t.Helper()
	work, err := store.Create(beads.Bead{
		Title:    "routed work",
		Type:     "task",
		Assignee: assignee,
		Labels:   labels,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
	})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("set in_progress: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatalf("reload work: %v", err)
	}
	return work
}

// TestReleaseOrphanedPoolAssignmentsRefusesAnotherCitysBead is the reconciler
// half of the loop-stopper: citadel's bead, held by citadel's worker (a session
// unknown here), is NOT reopened by jadegate's reconciler, and the refusal is
// the same greppable line the hook writes.
func TestReleaseOrphanedPoolAssignmentsRefusesAnotherCitysBead(t *testing.T) {
	logBuf := captureLog(t)
	store := beads.NewMemStore()
	work := orphanedWork(t, store, "citadel-worker-7", "owner:citadel")

	released := releaseOrphanedPoolAssignments(store, fenceReconcilerCfg("jadegate"), "", nil, []beads.Bead{work}, nil, nil, nil)

	if len(released) != 0 {
		t.Fatalf("REGRESSION jg-66rdw8: the reconciler reopened another city's bead: %v", released)
	}
	got, _ := store.Get(work.ID)
	if got.Status != "in_progress" || got.Assignee != "citadel-worker-7" {
		t.Fatalf("foreign bead was written: status=%q assignee=%q, want in_progress/citadel-worker-7 untouched", got.Status, got.Assignee)
	}
	for _, want := range []string{"cross-city-fence refused", "bead=" + work.ID, "owner=citadel", "this_identity=jadegate", "missing=" + federation.HandoffLabel("jadegate")} {
		if !strings.Contains(logBuf.String(), want) {
			t.Errorf("log lacks %q; the reconciler must log the same refusal line as the hook:\n%s", want, logBuf.String())
		}
	}
}

// TestReleaseOrphanedPoolAssignmentsStillReopensThisCitysOrphans is the
// control: the release lane keeps doing its job for own, handed-off and legacy
// work, and for everything on a non-federated city.
func TestReleaseOrphanedPoolAssignmentsStillReopensThisCitysOrphans(t *testing.T) {
	cases := []struct {
		name     string
		identity string
		labels   []string
	}{
		{"own bead", "jadegate", []string{"owner:jadegate"}},
		{"handed-off foreign bead", "jadegate", []string{"owner:citadel", "handoff:jadegate"}},
		{"legacy unlabeled bead", "jadegate", nil},
		{"not federated: foreign bead", "", []string{"owner:citadel"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logBuf := captureLog(t)
			store := beads.NewMemStore()
			work := orphanedWork(t, store, "gone-worker-3", tc.labels...)

			released := releaseOrphanedPoolAssignments(store, fenceReconcilerCfg(tc.identity), "", nil, []beads.Bead{work}, nil, nil, nil)

			if len(released) != 1 || released[0].ID != work.ID {
				t.Fatalf("want the orphan reopened, got %v\nlog: %s", released, logBuf.String())
			}
			got, _ := store.Get(work.ID)
			if got.Status != "open" || got.Assignee != "" {
				t.Fatalf("orphan not reopened: status=%q assignee=%q", got.Status, got.Assignee)
			}
			if strings.Contains(logBuf.String(), "cross-city-fence") {
				t.Fatalf("a claimable orphan must not log a refusal:\n%s", logBuf.String())
			}
		})
	}
}

// TestReassignWorkAssignedToRetiredSessionRefusesAnotherCitysBead covers the
// reconciler's second ownership write: a retired session that held another
// city's bead (the state a pre-fence claim left behind) must not hand it on to
// its successor. Own work is still re-homed.
func TestReassignWorkAssignedToRetiredSessionRefusesAnotherCitysBead(t *testing.T) {
	store := beads.NewMemStore()
	mk := func(labels ...string) beads.Bead {
		work, err := store.Create(beads.Bead{Title: "work", Type: "task", Status: "in_progress", Assignee: "retired-session", Labels: labels})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return work
	}
	foreign := mk("owner:citadel")
	own := mk("owner:jadegate")
	legacy := mk()

	var stderr bytes.Buffer
	reassignWorkAssignedToRetiredSessionBead("", fenceReconcilerCfg("jadegate"), store, nil, beads.Bead{ID: "retired-session"}, "replacement-session", &stderr)

	if got, _ := store.Get(foreign.ID); got.Assignee != "retired-session" {
		t.Fatalf("REGRESSION jg-66rdw8: another city's bead was re-homed onto %q", got.Assignee)
	}
	for _, id := range []string{own.ID, legacy.ID} {
		if got, _ := store.Get(id); got.Assignee != "replacement-session" {
			t.Fatalf("own/legacy work %s not re-homed: assignee=%q (the sweep must still run)\nstderr: %s", id, got.Assignee, stderr.String())
		}
	}
	for _, want := range []string{"cross-city-fence refused", "bead=" + foreign.ID, "owner=citadel", "this_identity=jadegate"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
		}
	}

	// The Info twin applies the same rule.
	info := beads.NewMemStore()
	fi, _ := info.Create(beads.Bead{Title: "work", Type: "task", Status: "in_progress", Assignee: "retired-session", Labels: []string{"owner:citadel"}})
	oi, _ := info.Create(beads.Bead{Title: "work", Type: "task", Status: "in_progress", Assignee: "retired-session", Labels: []string{"owner:jadegate"}})
	var infoErr bytes.Buffer
	reassignWorkAssignedToRetiredSessionInfo("", fenceReconcilerCfg("jadegate"), info, nil, session.Info{ID: "retired-session"}, "replacement-session", &infoErr)
	if got, _ := info.Get(fi.ID); got.Assignee != "retired-session" {
		t.Fatalf("Info variant: another city's bead re-homed onto %q", got.Assignee)
	}
	if got, _ := info.Get(oi.ID); got.Assignee != "replacement-session" {
		t.Fatalf("Info variant: own work not re-homed (assignee=%q)\nstderr: %s", got.Assignee, infoErr.String())
	}
	if !strings.Contains(infoErr.String(), "cross-city-fence refused bead="+fi.ID) {
		t.Fatalf("Info variant must log the shared line:\n%s", infoErr.String())
	}

	// Not federated: the retired session's foreign-labeled bead is re-homed like any other.
	off := beads.NewMemStore()
	work, err := off.Create(beads.Bead{Title: "work", Type: "task", Status: "in_progress", Assignee: "retired-session", Labels: []string{"owner:citadel"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	reassignWorkAssignedToRetiredSessionBead("", fenceReconcilerCfg(""), off, nil, beads.Bead{ID: "retired-session"}, "replacement-session", io.Discard)
	if got, _ := off.Get(work.ID); got.Assignee != "replacement-session" {
		t.Fatalf("not federated: fence must be off, got assignee=%q", got.Assignee)
	}
}
