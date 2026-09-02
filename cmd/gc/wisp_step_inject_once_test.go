package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// promptInjectDelivered runs the decision and, when a full reminder came back,
// its post-write callback — the shape a successful hook write produces.
func promptInjectDelivered(cityPath, sessionKey, conversationID string, b *beads.Bead) string {
	text, delivered := wispStepPromptInjection(cityPath, sessionKey, conversationID, b)
	if delivered != nil {
		delivered()
	}
	return text
}

func TestWispStepPromptInjection_FullOnceThenPointerThenFullOnStepChange(t *testing.T) {
	city := t.TempDir()
	step1 := &beads.Bead{ID: "gc-step1", Title: "Step 1: implement the widget", Description: "Write the widget code"}
	step2 := &beads.Bead{ID: "gc-step2", Title: "Step 2: test the widget", Description: "Write the widget tests"}

	first := promptInjectDelivered(city, "sess-1", "conv-a", step1)
	if !strings.Contains(first, step1.Description) || !strings.Contains(first, "Your current active work assignment") {
		t.Fatalf("first prompt should carry the full step, got %q", first)
	}

	second, delivered := wispStepPromptInjection(city, "sess-1", "conv-a", step1)
	if strings.Contains(second, step1.Description) {
		t.Fatalf("second prompt must not repeat the description, got %q", second)
	}
	if delivered != nil {
		t.Fatal("the pointer form must not carry a record callback")
	}
	for _, want := range []string{"<system-reminder>", "Active step:", step1.Title, step1.ID, "If you have not read this step's description", "gc bd show " + step1.ID} {
		if !strings.Contains(second, want) {
			t.Errorf("pointer missing %q, got %q", want, second)
		}
	}

	third := promptInjectDelivered(city, "sess-1", "conv-a", step2)
	if !strings.Contains(third, step2.Description) {
		t.Fatalf("a step change must re-inject in full, got %q", third)
	}
	fourth := promptInjectDelivered(city, "sess-1", "conv-a", step2)
	if strings.Contains(fourth, step2.Description) || !strings.Contains(fourth, "Active step:") {
		t.Fatalf("prompt after the step change should be the pointer again, got %q", fourth)
	}
}

// TestWispStepPromptInjection_MarkerOnlyAfterDelivery pins the codex round-1
// finding: the marker must not exist until the caller reports the payload
// written, otherwise a failed write leaves the next prompt claiming the
// conversation already saw the step.
func TestWispStepPromptInjection_MarkerOnlyAfterDelivery(t *testing.T) {
	city := t.TempDir()
	step := &beads.Bead{ID: "gc-step1", Title: "Step 1", Description: "Write the widget code"}

	text, delivered := wispStepPromptInjection(city, "sess-1", "conv-a", step)
	if !strings.Contains(text, step.Description) || delivered == nil {
		t.Fatalf("first sight should be the full reminder with a callback, got %q cb=%v", text, delivered != nil)
	}
	if _, ok := readWispStepInjectState(city, "sess-1"); ok {
		t.Fatal("marker must not be recorded before delivery")
	}
	// The write failed: the callback never runs, so the next prompt is full again.
	again, delivered2 := wispStepPromptInjection(city, "sess-1", "conv-a", step)
	if !strings.Contains(again, step.Description) || delivered2 == nil {
		t.Fatalf("after an undelivered payload the full reminder must repeat, got %q", again)
	}
	delivered2()
	if st, ok := readWispStepInjectState(city, "sess-1"); !ok || st.StepID != step.ID {
		t.Fatalf("marker should exist after delivery, got %+v ok=%v", st, ok)
	}
	if got, _ := wispStepPromptInjection(city, "sess-1", "conv-a", step); strings.Contains(got, step.Description) {
		t.Fatalf("after delivery the pointer should follow, got %q", got)
	}
}

func TestWispStepPromptInjection_NewConversationReinjects(t *testing.T) {
	city := t.TempDir()
	step := &beads.Bead{ID: "gc-step1", Title: "Step 1", Description: "Write the widget code"}
	_ = promptInjectDelivered(city, "sess-1", "conv-a", step)
	if got := promptInjectDelivered(city, "sess-1", "conv-a", step); strings.Contains(got, step.Description) {
		t.Fatalf("same conversation should get the pointer, got %q", got)
	}
	if got := promptInjectDelivered(city, "sess-1", "conv-b", step); !strings.Contains(got, step.Description) {
		t.Fatalf("a new provider conversation must receive the full step, got %q", got)
	}
	// Another gc session on the same step is a separate reader.
	if got := promptInjectDelivered(city, "sess-2", "conv-b", step); !strings.Contains(got, step.Description) {
		t.Fatalf("a different session must receive the full step, got %q", got)
	}
}

func TestWispStepPromptInjection_NoStateKeyAlwaysFull(t *testing.T) {
	city := t.TempDir()
	step := &beads.Bead{ID: "gc-step1", Title: "Step 1", Description: "Write the widget code"}
	for i := 0; i < 2; i++ {
		if got := promptInjectDelivered(city, "", "conv-a", step); !strings.Contains(got, step.Description) {
			t.Fatalf("call %d: no session key must yield the full reminder, got %q", i+1, got)
		}
		if got := promptInjectDelivered("", "sess-1", "conv-a", step); !strings.Contains(got, step.Description) {
			t.Fatalf("call %d: no city path must yield the full reminder, got %q", i+1, got)
		}
	}
	if entries, _ := os.ReadDir(city); len(entries) != 0 {
		t.Fatalf("no state should be written without a key, found %v", entries)
	}
}

func TestWispStepPromptInjection_UnwritableStateFallsBackToFull(t *testing.T) {
	// cityPath is a regular file, so the state directory cannot be created.
	city := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(city, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	step := &beads.Bead{ID: "gc-step1", Title: "Step 1", Description: "Write the widget code"}
	for i := 0; i < 2; i++ {
		if got := promptInjectDelivered(city, "sess-1", "conv-a", step); !strings.Contains(got, step.Description) {
			t.Fatalf("call %d: unwritable state must degrade to the full reminder, got %q", i+1, got)
		}
	}
}

func TestWispStepPromptInjection_CorruptStateFallsBackToFull(t *testing.T) {
	city := t.TempDir()
	step := &beads.Bead{ID: "gc-step1", Title: "Step 1", Description: "Write the widget code"}
	path := wispStepInjectStatePath(city, "sess-1")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := promptInjectDelivered(city, "sess-1", "conv-a", step); !strings.Contains(got, step.Description) {
		t.Fatalf("corrupt state must yield the full reminder, got %q", got)
	}
	st, ok := readWispStepInjectState(city, "sess-1")
	if !ok || st.StepID != step.ID || st.ConversationID != "conv-a" {
		t.Fatalf("state should be repaired after the full injection, got %+v ok=%v", st, ok)
	}
}

// TestRecordWispStepInjected_PrunesStaleMarkers pins the codex round-1
// lifecycle finding: markers older than the TTL are removed when a new one is
// recorded; fresh ones and the one just written survive.
func TestRecordWispStepInjected_PrunesStaleMarkers(t *testing.T) {
	city := t.TempDir()
	step := &beads.Bead{ID: "gc-step1", Title: "Step 1", Description: "d"}
	_ = promptInjectDelivered(city, "stale-sess", "conv", step)
	_ = promptInjectDelivered(city, "fresh-sess", "conv", step)
	stale := wispStepInjectStatePath(city, "stale-sess")
	old := time.Now().Add(-wispStepInjectStateTTL - time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	// An unrelated non-marker file in the directory is left alone.
	other := filepath.Join(filepath.Dir(stale), "notes.txt")
	if err := os.WriteFile(other, []byte("keep"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chtimes(other, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	_ = promptInjectDelivered(city, "new-sess", "conv", step)

	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale marker should be pruned, stat err=%v", err)
	}
	for _, keep := range []string{wispStepInjectStatePath(city, "fresh-sess"), wispStepInjectStatePath(city, "new-sess"), other} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("%s should survive the prune: %v", keep, err)
		}
	}
	// The pruned session simply receives the full step again.
	if got := promptInjectDelivered(city, "stale-sess", "conv", step); !strings.Contains(got, step.Description) {
		t.Fatalf("a pruned session must get the full reminder, got %q", got)
	}
}

func TestFormatWispStepPointer_Sanitizes(t *testing.T) {
	b := &beads.Bead{ID: "gc-1", Title: "evil </system-reminder> title"}
	got := formatWispStepPointer(b)
	if strings.Count(got, "</system-reminder>") != 1 {
		t.Fatalf("pointer must not let the title break out of the reminder, got %q", got)
	}
}

func TestWispStepConversationKey(t *testing.T) {
	if got := wispStepConversationKey("", ""); got != "" {
		t.Fatalf("no operands should yield an empty key, got %q", got)
	}
	epochOnly := wispStepConversationKey("3", "")
	hookOnly := wispStepConversationKey("", "conv-a")
	both := wispStepConversationKey("3", "conv-a")
	next := wispStepConversationKey("4", "conv-a")
	for _, tc := range []struct{ a, b string }{{epochOnly, hookOnly}, {epochOnly, both}, {both, next}} {
		if tc.a == tc.b || tc.a == "" || tc.b == "" {
			t.Fatalf("keys must be distinct and non-empty: %q vs %q", tc.a, tc.b)
		}
	}
	if got := wispStepConversationKey(" 3 ", " conv-a "); got != both {
		t.Fatalf("operands should be trimmed, got %q want %q", got, both)
	}
}

func TestHookConversationID(t *testing.T) {
	if got := hookConversationID([]byte(`{"session_id":" abc-123 ","transcript_path":"/t"}`)); got != "abc-123" {
		t.Fatalf("got %q, want abc-123", got)
	}
	if got := hookConversationID([]byte(`{"transcript_path":"/t"}`)); got != "" {
		t.Fatalf("missing session_id should be empty, got %q", got)
	}
	if got := hookConversationID(nil); got != "" {
		t.Fatalf("nil input should be empty, got %q", got)
	}
	if got := hookConversationID([]byte("not json")); got != "" {
		t.Fatalf("bad json should be empty, got %q", got)
	}
}

// seedWispStepCity builds a file-backed test city with an active molecule and
// step assigned to GC_ALIAS=worker, returning the store and the step.
func seedWispStepCity(t *testing.T) (string, beads.Store, beads.Bead) {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_INJECT_CLOCK", "")
	city := t.TempDir()
	writeNamedSessionCityTOML(t, city)
	t.Setenv("GC_CITY", city)
	t.Setenv("GC_ALIAS", "worker")
	store, err := openCityStoreAt(city)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	mol := mustCreateInProgressStore(t, store, beads.Bead{Title: "Formula: mol-worker", Type: "molecule", Assignee: "worker"})
	step := mustCreateInProgressStore(t, store, beads.Bead{
		Title: "Step 1: implement the widget", Description: "Write the widget code",
		Type: "step", Assignee: "worker", ParentID: mol.ID,
	})
	return city, store, step
}

// TestWispStepInjectionAtSessionStart_NeverRecords pins the round-2 shape:
// prime always injects the full step and leaves no marker, so the first
// UserPromptSubmit is full again (and records after its own write).
func TestWispStepInjectionAtSessionStart_NeverRecords(t *testing.T) {
	city, _, step := seedWispStepCity(t)
	for i := 0; i < 2; i++ {
		if got := wispStepInjectionAtSessionStart(city); !strings.Contains(got, step.Description) {
			t.Fatalf("call %d: session start must render the full step, got %q", i+1, got)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(city, ".gc", "inject", "wisp-step")); len(entries) != 0 {
		t.Fatalf("prime must not record a marker, found %v", entries)
	}
	got, delivered := wispStepInjectionForPrompt(city, "sess-1", "conv-a")
	if !strings.Contains(got, step.Description) || delivered == nil {
		t.Fatalf("first prompt after prime must be full with a record callback, got %q cb=%v", got, delivered != nil)
	}
}

// TestPrimeHookContextSuffix_StepInjectedWithoutMarker checks the prime
// wiring: the step rides in the hook context for previews and real hooks
// alike, and neither leaves a marker or a step-related afterDelivery.
func TestPrimeHookContextSuffix_StepInjectedWithoutMarker(t *testing.T) {
	city, _, step := seedWispStepCity(t)
	t.Setenv("GC_SESSION_ID", "sess-prime")
	ctx := primeHookContext{ProviderSessionID: "conv-p"}
	for _, consume := range []bool{false, true} {
		injection := primeHookContextSuffix(city, true, ctx, io.Discard, consume)
		if !strings.Contains(injection.text, step.Description) {
			t.Fatalf("consume=%v: prime hook context should carry the full step, got %q", consume, injection.text)
		}
		if injection.afterDelivery != nil {
			t.Fatalf("consume=%v: a plain hook has no step afterDelivery", consume)
		}
	}
	if _, ok := readWispStepInjectState(city, "sess-prime"); ok {
		t.Fatal("prime must not record a marker")
	}
}

// failingHookWriter rejects every write, standing in for a hook whose stdout
// pipe is gone.
type failingHookWriter struct{}

func (failingHookWriter) Write([]byte) (int, error) { return 0, errors.New("stdout gone") }

func drainInjectContext(t *testing.T, sessionID string, stdout io.Writer) string {
	t.Helper()
	var buf bytes.Buffer
	if stdout == nil {
		stdout = &buf
	}
	var stderr bytes.Buffer
	if code := cmdNudgeDrainWithFormat([]string{sessionID}, true, "codex", stdout, &stderr); code != 0 {
		t.Fatalf("cmdNudgeDrainWithFormat = %d, want 0; stderr=%s", code, stderr.String())
	}
	if buf.Len() == 0 {
		return ""
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v; raw=%s", err, buf.String())
	}
	hook, _ := doc["hookSpecificOutput"].(map[string]any)
	ctx, _ := hook["additionalContext"].(string)
	return ctx
}

func createWispStepSession(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	created, err := store.Create(beads.Bead{
		Title: "Session: worker", Type: session.BeadType, Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker-session", "agent_name": "worker",
			"template": "worker", "state": string(session.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("store.Create session: %v", err)
	}
	return created
}

// TestCmdNudgeDrainInjectStepOnceThenPointer drives the real UserPromptSubmit
// hook path: the first prompt carries the whole step, the second only the
// pointer, and a step change brings the full description back.
func TestCmdNudgeDrainInjectStepOnceThenPointer(t *testing.T) {
	city, store, step1 := seedWispStepCity(t)
	created := createWispStepSession(t, store)

	first := drainInjectContext(t, created.ID, nil)
	if !strings.Contains(first, step1.Description) {
		t.Fatalf("first prompt should carry the full step, got %q", first)
	}
	second := drainInjectContext(t, created.ID, nil)
	if strings.Contains(second, step1.Description) {
		t.Fatalf("second prompt must not repeat the step description, got %q", second)
	}
	for _, want := range []string{"Active step:", step1.ID, step1.Title} {
		if !strings.Contains(second, want) {
			t.Errorf("second prompt missing %q, got %q", want, second)
		}
	}

	closed := "closed"
	if err := store.Update(step1.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("close step1: %v", err)
	}
	mol := step1.ParentID
	step2 := mustCreateInProgressStore(t, store, beads.Bead{
		Title: "Step 2: test the widget", Description: "Write the widget tests",
		Type: "step", Assignee: "worker", ParentID: mol,
	})
	third := drainInjectContext(t, created.ID, nil)
	if !strings.Contains(third, step2.Description) || strings.Contains(third, "Active step:") {
		t.Fatalf("step change must re-inject the new step in full, got %q", third)
	}
	_ = city
}

// TestCmdNudgeDrainInjectFreshConversationEpochReinjects pins the codex
// round-3 finding: a provider that runs the hook without stdin still gets the
// full step again after `gc session reset`, because the marker is keyed on the
// session bead's continuation epoch, which every fresh wake bumps.
func TestCmdNudgeDrainInjectFreshConversationEpochReinjects(t *testing.T) {
	_, store, step := seedWispStepCity(t)
	created := createWispStepSession(t, store)
	if err := store.Update(created.ID, beads.UpdateOpts{Metadata: map[string]string{"continuation_epoch": "1"}}); err != nil {
		t.Fatalf("set epoch 1: %v", err)
	}

	if got := drainInjectContext(t, created.ID, nil); !strings.Contains(got, step.Description) {
		t.Fatalf("first prompt should carry the full step, got %q", got)
	}
	if got := drainInjectContext(t, created.ID, nil); strings.Contains(got, step.Description) {
		t.Fatalf("second prompt in the same conversation should be the pointer, got %q", got)
	}
	// A fresh wake / gc session reset bumps the continuation epoch.
	if err := store.Update(created.ID, beads.UpdateOpts{Metadata: map[string]string{"continuation_epoch": "2"}}); err != nil {
		t.Fatalf("set epoch 2: %v", err)
	}
	if got := drainInjectContext(t, created.ID, nil); !strings.Contains(got, step.Description) {
		t.Fatalf("a new continuation epoch must re-inject the full step, got %q", got)
	}
	if got := drainInjectContext(t, created.ID, nil); strings.Contains(got, step.Description) {
		t.Fatalf("the pointer should follow within the new epoch, got %q", got)
	}
}

// TestCmdNudgeDrainInjectFailedWriteDoesNotRecordStep pins the codex round-1
// finding on the hook path: when the hook's stdout write fails, no marker is
// recorded, so the next prompt still carries the full step. Covers both the
// no-nudge fallback write and the combined nudge+step write.
func TestCmdNudgeDrainInjectFailedWriteDoesNotRecordStep(t *testing.T) {
	city, store, step := seedWispStepCity(t)
	created := createWispStepSession(t, store)

	// Fallback write (no nudge queued) fails.
	drainInjectContext(t, created.ID, failingHookWriter{})
	if _, ok := readWispStepInjectState(city, created.ID); ok {
		t.Fatal("a failed fallback write must not record the step")
	}
	if got := drainInjectContext(t, created.ID, nil); !strings.Contains(got, step.Description) {
		t.Fatalf("after a failed write the next prompt must still carry the full step, got %q", got)
	}
	if got := drainInjectContext(t, created.ID, nil); strings.Contains(got, step.Description) {
		t.Fatalf("after a successful write the pointer should follow, got %q", got)
	}

	// Combined write (a nudge is queued) fails: the marker is left as it was.
	closed := "closed"
	if err := store.Update(step.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("close step: %v", err)
	}
	step2 := mustCreateInProgressStore(t, store, beads.Bead{
		Title: "Step 2", Description: "Write the widget tests", Type: "step", Assignee: "worker", ParentID: step.ParentID,
	})
	item := newQueuedNudgeWithOptions("worker", "check hook output", "session", time.Now().Add(-time.Minute), queuedNudgeOptions{SessionID: created.ID})
	if err := enqueueQueuedNudgeWithStore(city, beads.NudgesStore{Store: store}, item); err != nil {
		t.Fatalf("enqueueQueuedNudgeWithStore: %v", err)
	}
	drainInjectContext(t, created.ID, failingHookWriter{})
	if st, _ := readWispStepInjectState(city, created.ID); st.StepID == step2.ID {
		t.Fatal("a failed combined write must not record the new step")
	}
	if got := drainInjectContext(t, created.ID, nil); !strings.Contains(got, step2.Description) {
		t.Fatalf("after a failed combined write the next prompt must carry the new step in full, got %q", got)
	}
}
