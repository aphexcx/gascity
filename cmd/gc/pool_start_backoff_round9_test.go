package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// The cases codex round 9 found missing or wrong (evidence 07-codex-r9.md).

// TestWorkTriggerForStartUsesTheBuildsVerdictForNamedHolders: a configured
// named holder's start is charged to the trigger on its TemplateParams (the
// build's wake request for this tick, or nothing), never to what its bead
// says — the bind onto the bead can fail transiently. A pool seat's trigger
// is its bead's.
func TestWorkTriggerForStartUsesTheBuildsVerdictForNamedHolders(t *testing.T) {
	info := session.Info{ID: "s-1", TriggerBeadID: "gp-old", TriggerBeadStoreRef: "city"}
	named := TemplateParams{ConfiguredNamedIdentity: "test-city/solo", TriggerBeadID: "gp-new", TriggerBeadStoreRef: "rig:a"}
	if got := workTriggerForStart(named, info); got != (workTrigger{BeadID: "gp-new", StoreRef: "rig:a"}) {
		t.Fatalf("named holder: want the build's verdict, got %+v", got)
	}
	if got := workTriggerForStart(TemplateParams{ConfiguredNamedIdentity: "test-city/solo"}, info); got != (workTrigger{}) {
		t.Fatalf("named holder woken for nothing: charged to nothing, got %+v", got)
	}
	if got := workTriggerForStart(TemplateParams{TemplateName: "worker"}, info); got != (workTrigger{BeadID: "gp-old", StoreRef: "city"}) {
		t.Fatalf("pool seat: its bead's trigger, got %+v", got)
	}
}

// round9FailingWriter is a conditional writer whose fenced update fails with
// an ordinary (non-precondition) error: the work store is down for writes.
type round9FailingWriter struct {
	beads.ConditionalWriter
	err error
}

func (w round9FailingWriter) UpdateIfMatch(string, int64, beads.UpdateOpts) error { return w.err }

func round9OpenSessionInfos(t *testing.T, store beads.Store) []session.Info {
	t.Helper()
	rows, err := store.List(beads.ListQuery{Type: sessionBeadType})
	if err != nil {
		t.Fatal(err)
	}
	var open []beads.Bead
	for _, b := range rows {
		if b.Status != "closed" {
			open = append(open, b)
		}
	}
	return sessionInfosFromBeads(open)
}

// TestConfirmedStartLeavesAnOwedResetMarkerUntilTheClearLands: through the
// real reconciler, the batch that confirms a start stamps the session bead
// with the work bead the start ran for; the clear that follows lifts the
// marker. When the work store refuses the clear the marker stays, and a
// later tick's settleOwedStartResets clears the record and lifts it —
// nothing about the reset lives in process memory. A session re-pointed to
// other work afterwards carries no marker, so that work's record is never
// touched by an old confirmation.
func TestConfirmedStartLeavesAnOwedResetMarkerUntilTheClearLands(t *testing.T) {
	h := newStartBackoffHarness(t, 5)
	// Two failed starts: the record reads 2.
	if starts := h.advance(15*time.Second, errPreStartFailure); starts != 2 {
		t.Fatalf("starts = %d, want 2\nstderr:\n%s", starts, h.env.stderr.String())
	}
	if got := readWorkStartFailureState(h.reload().Metadata).Failures; got != 2 {
		t.Fatalf("record = %d, want 2", got)
	}
	h.env.clk.Time = h.env.clk.Time.Add(time.Minute)
	// The work store refuses the clear the confirming start owes.
	realWriter, _ := beads.ConditionalWriterFor(h.env.store)
	h.policy.resolveWriter = func(beads.Store) (beads.ConditionalWriter, error) {
		return round9FailingWriter{ConditionalWriter: realWriter, err: errors.New("work store: write timed out")}, nil
	}
	if !h.tick(nil) {
		t.Fatal("the successful start must be planned")
	}
	if got := readWorkStartFailureState(h.reload().Metadata).Failures; got != 2 {
		t.Fatalf("the refused clear must leave the record at 2, got %d", got)
	}
	infos := round9OpenSessionInfos(t, h.env.store)
	if len(infos) != 1 {
		t.Fatalf("one open session, got %d", len(infos))
	}
	confirmed := infos[0]
	if confirmed.PendingCreateClaim || strings.TrimSpace(confirmed.CreationCompleteAt) == "" {
		t.Fatalf("the start must be confirmed: %+v", confirmed)
	}
	if confirmed.StartResetOwedBeadID != h.work.ID {
		t.Fatalf("the confirming batch must stamp the owed reset with the work bead, got %q (stderr:\n%s)", confirmed.StartResetOwedBeadID, h.env.stderr.String())
	}
	owed := owedStartResets(h.env.cfg, infos)
	if len(owed) != 1 || owed[0].SessionID != confirmed.ID || owed[0].Trigger.BeadID != h.work.ID || owed[0].Trigger.StoreRef != "city" {
		t.Fatalf("owedStartResets = %+v", owed)
	}
	// Still down: the marker stays.
	h.policy.settleOwedStartResets(owed, h.env.store)
	if got := readWorkStartFailureState(h.reload().Metadata).Failures; got != 2 {
		t.Fatalf("a settle that fails must leave the record, got %d", got)
	}
	if infos := round9OpenSessionInfos(t, h.env.store); infos[0].StartResetOwedBeadID != h.work.ID {
		t.Fatal("a settle that fails must keep the marker")
	}
	// The store is back (a fresh policy, as after a controller restart):
	// the marker is settled from the session bead alone.
	h.installPolicy()
	h.policy.settleOwedStartResets(owedStartResets(h.env.cfg, round9OpenSessionInfos(t, h.env.store)), h.env.store)
	if got := h.reload(); len(workStartFailureClearPatch(got.Metadata)) != 0 {
		t.Fatalf("the settled reset must clear the record: %v\nstderr:\n%s", got.Metadata, h.env.stderr.String())
	}
	infos = round9OpenSessionInfos(t, h.env.store)
	if infos[0].StartResetOwedBeadID != "" || infos[0].StartResetOwedStoreRef != "" {
		t.Fatalf("the settled marker must be lifted: %+v", infos[0])
	}
	if !strings.Contains(h.env.stderr.String(), "start-failure record cleared") {
		t.Fatalf("the clear is logged:\n%s", h.env.stderr.String())
	}
	// Re-pointed to other work with a record, no restart: the old
	// confirmation says nothing about it — there is no marker to settle.
	other, err := h.env.store.Create(beads.Bead{Title: "other work", Type: "task", Metadata: map[string]string{
		beadmeta.RoutedToMetadataKey:      backoffHarnessTemplate,
		beadmeta.StartFailuresMetadataKey: "4",
		beadmeta.StartFailedAtMetadataKey: h.env.clk.Now().UTC().Format(time.RFC3339),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.env.store.SetMetadataBatch(confirmed.ID, map[string]string{beadmeta.TriggerBeadIDMetadataKey: other.ID}); err != nil {
		t.Fatal(err)
	}
	if owed := owedStartResets(h.env.cfg, round9OpenSessionInfos(t, h.env.store)); len(owed) != 0 {
		t.Fatalf("a re-pointed session owes nothing: %+v", owed)
	}
	row, _ := h.env.store.Get(other.ID)
	if readWorkStartFailureState(row.Metadata).Failures != 4 {
		t.Fatalf("the other bead's record is not the old confirmation's to clear: %v", row.Metadata)
	}
	// A clean confirmed start lifts its own marker in the same commit.
	h.env.clk.Time = h.env.clk.Time.Add(time.Minute)
	if err := h.env.store.SetMetadataBatch(confirmed.ID, map[string]string{"state": "closed", "status": "closed"}); err != nil {
		t.Fatal(err)
	}
	if err := h.env.store.Close(confirmed.ID); err != nil {
		t.Fatal(err)
	}
	if !h.tick(nil) {
		t.Fatal("the next start must be planned")
	}
	for _, s := range round9OpenSessionInfos(t, h.env.store) {
		if s.StartResetOwedBeadID != "" {
			t.Fatalf("a confirmed start whose clear landed lifts its marker in the same commit: %+v", s)
		}
	}
}

// TestOwedStartResetsListsOnlyMarkedSessions: the sweep's input is the
// marker alone — a session with a trigger but no marker owes nothing.
func TestOwedStartResetsListsOnlyMarkedSessions(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "helper", MaxActiveSessions: intPtr(1)}}}
	infos := []session.Info{
		{ID: "s-marked", Template: "helper", TriggerBeadID: "gp-now", StartResetOwedBeadID: "gp-then", StartResetOwedStoreRef: "rig:a"},
		{ID: "s-plain", Template: "helper", TriggerBeadID: "gp-now"},
		{ID: "", Template: "helper", StartResetOwedBeadID: "gp-noid"},
	}
	owed := owedStartResets(cfg, infos)
	if len(owed) != 1 || owed[0].SessionID != "s-marked" || owed[0].Trigger != (workTrigger{BeadID: "gp-then", StoreRef: "rig:a"}) || owed[0].Template == "" {
		t.Fatalf("owedStartResets = %+v", owed)
	}
	var none *workStartFailurePolicy
	none.settleOwedStartResets(owed, beads.NewMemStore()) // no policy: nothing to settle, no panic
	if !none.recordStartSuccess(workTrigger{BeadID: "gp-then"}, "helper") {
		t.Fatal("a nil policy owes nothing")
	}
}

// TestWriteWorkRecordFailsClosedWhenTheFenceIsRefusedUnderRequire: the
// capability probe passed when the writer was resolved, but the store
// refuses the fence at write time (beads.ErrConditionalWriteUnsupported).
// Under beads.conditional_writes = "require" the record is NOT written
// unfenced; under any other mode the plain re-read path is taken as before.
func TestWriteWorkRecordFailsClosedWhenTheFenceIsRefusedUnderRequire(t *testing.T) {
	for _, require := range []bool{true, false} {
		cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}}}
		store := beads.NewMemStore()
		work, err := store.Create(beads.Bead{Title: "work", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
		if err != nil {
			t.Fatal(err)
		}
		realWriter, _ := beads.ConditionalWriterFor(store)
		var stderr bytes.Buffer
		policy := &workStartFailurePolicy{
			workStore:         store,
			templateOf:        func(b beads.Bead) string { return poolTemplateForWorkBead(cfg, b) },
			canonicalTemplate: func(template string) string { return normalizeAgentTemplateIdentity(cfg, template) },
			limitFor:          func(string) int { return 5 },
			resolveWriter: func(beads.Store) (beads.ConditionalWriter, error) {
				return round9FailingWriter{ConditionalWriter: realWriter, err: beads.ErrConditionalWriteUnsupported}, nil
			},
			requireFenced: func(beads.Store) bool { return require },
			stderr:        &stderr,
		}
		policy.recordStartFailure(workTrigger{BeadID: work.ID, StoreRef: "city"}, "worker", errPreStartFailure, time.Now().UTC())
		row, _ := store.Get(work.ID)
		got := readWorkStartFailureState(row.Metadata).Failures
		switch {
		case require && got != 0:
			t.Fatalf("require: the refused fence must not fall back to an unfenced write, record=%d\nstderr:\n%s", got, stderr.String())
		case require && !strings.Contains(stderr.String(), "conditional_writes=require"):
			t.Fatalf("require: the refusal is said:\n%s", stderr.String())
		case !require && got != 1:
			t.Fatalf("auto/off: the plain path still charges, record=%d\nstderr:\n%s", got, stderr.String())
		}
	}
}

// round9ErrStore fails every read.
type round9ErrStore struct {
	beads.Store
	err error
}

func (s round9ErrStore) Get(string) (beads.Bead, error) { return beads.Bead{}, s.err }

// TestTriggerBeadLookupErrorsAreSaidNotSwallowed: a store that fails to
// answer is not the bead's absence — the dropped charge is logged, and a
// reset that could not find its bead stays owed.
func TestTriggerBeadLookupErrorsAreSaidNotSwallowed(t *testing.T) {
	var stderr bytes.Buffer
	policy := &workStartFailurePolicy{
		workStore: round9ErrStore{Store: beads.NewMemStore(), err: errors.New("backend unavailable")},
		limitFor:  func(string) int { return 5 },
		stderr:    &stderr,
	}
	policy.recordStartFailure(workTrigger{BeadID: "gp-somewhere", StoreRef: "city"}, "worker", errPreStartFailure, time.Now().UTC())
	if !strings.Contains(stderr.String(), "could not be read") || !strings.Contains(stderr.String(), "backend unavailable") {
		t.Fatalf("a lookup failure must be said:\n%s", stderr.String())
	}
	if policy.recordStartSuccess(workTrigger{BeadID: "gp-somewhere", StoreRef: "city"}, "worker") {
		t.Fatal("a reset whose bead could not be read is still owed")
	}
	// A bead in no store at all (every store answers not-found) is settled.
	quiet := &workStartFailurePolicy{workStore: beads.NewMemStore(), limitFor: func(string) int { return 5 }, stderr: &stderr}
	if !quiet.recordStartSuccess(workTrigger{BeadID: "gp-nowhere", StoreRef: "city"}, "worker") {
		t.Fatal("no such bead: nothing owed")
	}
}
