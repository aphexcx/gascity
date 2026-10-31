package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestNudgeDeadLetterNotificationAfterCommitOnce(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	store := beads.NudgesStore{Store: beads.NewMemStore()}
	item := newQueuedNudge("worker", "founder line", now)
	if err := enqueueQueuedNudgeWithStore(dir, store, item); err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	oldNotify, oldWriter := nudgeDeadLetterNotify, nudgeWarningWriter
	t.Cleanup(func() { nudgeDeadLetterNotify, nudgeWarningWriter = oldNotify, oldWriter })
	nudgeWarningWriter = &warnings
	calls := 0
	nudgeDeadLetterNotify = func(cityPath string, dead queuedNudge) error {
		calls++
		// Reacquiring the lock proves the notification is outside it and the
		// committed dead letter is already visible to another queue reader.
		if err := nudgequeue.WithState(cityPath, func(s *nudgequeue.State) error {
			if len(s.Dead) != 1 || s.Dead[0].ID != item.ID || dead.LastError != "draft_ownership_lost" {
				t.Fatalf("notification preceded persistence: %+v", s)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return errors.New("mail unavailable")
	}
	for range 2 {
		if err := recordQueuedNudgeFailureWithStore(dir, store, []string{item.ID}, runtime.ErrNudgeDraftOwnershipLost, now); err != nil {
			t.Fatalf("mail failure blocked transition: %v", err)
		}
	}
	if calls != 1 || !strings.Contains(warnings.String(), "action required") || !strings.Contains(warnings.String(), "mail unavailable") || !strings.Contains(warnings.String(), item.ID) {
		t.Fatalf("calls=%d warnings=%q", calls, warnings.String())
	}
}

func TestNudgeDeadLetterExpiryNotification(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	item := newQueuedNudge("worker", "unowned draft", now)
	item.ExpiresAt = now
	if err := nudgequeue.WithState(dir, func(s *nudgequeue.State) error { s.Pending = []queuedNudge{item}; return nil }); err != nil {
		t.Fatal(err)
	}
	old := nudgeDeadLetterNotify
	t.Cleanup(func() { nudgeDeadLetterNotify = old })
	calls := 0
	nudgeDeadLetterNotify = func(_ string, _ queuedNudge) error { calls++; return nil }
	for range 2 {
		if err := withNudgeQueueState(dir, func(s *nudgeQueueState) error {
			return pruneExpiredQueuedNudges(s, nil, now, noMaintenanceDeadline())
		}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("expiry notifications=%d, want 1", calls)
	}
	if err := withNudgeQueueState(dir, func(s *nudgeQueueState) error {
		s.Dead = append(s.Dead, queuedNudge{ID: "uncommitted"})
		return errors.New("abort")
	}); err == nil || calls != 1 {
		t.Fatalf("failed transaction notified: calls=%d err=%v", calls, err)
	}
}

func TestNudgeDeadLetterRecipient(t *testing.T) {
	lookup := func(id string) (beads.Bead, error) {
		if id != "work-1" {
			t.Fatalf("looked up shadow instead of source bead: %s", id)
		}
		return beads.Bead{Metadata: map[string]string{"escalation_recipient": "reviewer"}}, nil
	}
	item := queuedNudge{BeadID: "shadow", Reference: &nudgeReference{Kind: "bead", ID: "work-1"}}
	if to, err := nudgeDeadLetterRecipient(item, "operator", lookup); err != nil || to != "reviewer" {
		t.Fatalf("bead recipient: to=%q err=%v", to, err)
	}
	for _, ref := range []*nudgeReference{nil, {Kind: "mail", ID: "mail-1"}, {Kind: "bead", ID: "work-1"}} {
		item.Reference = ref
		if to, err := nudgeDeadLetterRecipient(item, "operator", func(string) (beads.Bead, error) { return beads.Bead{}, nil }); err != nil || to != "operator" {
			t.Fatalf("fallback: to=%q err=%v", to, err)
		}
	}
	item.Reference = nil
	if to, err := nudgeDeadLetterRecipient(item, "", lookup); to != "" || err == nil || !strings.Contains(err.Error(), "session.park_alert_to") {
		t.Fatalf("missing recipient: to=%q err=%v", to, err)
	}
}

func TestNudgeDeadLetterMailShape(t *testing.T) {
	now := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	item := queuedNudge{ID: "n-1", Agent: "worker", SessionID: "s-1", Message: strings.Repeat("界", 300) + "\nPRIVATE SECOND LINE", FirstAttemptAt: now.Add(-time.Minute), DeadAt: now, LastError: "draft_ownership_lost"}
	subject, body := nudgeDeadLetterMail(item)
	if subject != "NUDGE DEAD LETTER session s-1 reason draft_ownership_lost item n-1" {
		t.Fatal(subject)
	}
	for _, value := range []string{"n-1", "worker", "s-1", "draft_ownership_lost", "2026-10-31T13:59:00Z", "2026-10-31T14:00:00Z", "gc nudge replay n-1"} {
		if !strings.Contains(body, value) {
			t.Fatalf("mail omitted %q: %s", value, body)
		}
	}
	if strings.Contains(body, "PRIVATE SECOND LINE") || strings.Count(body, "界") > 160 {
		t.Fatalf("unbounded paste in mail: %s", body)
	}
	item.FirstAttemptAt = time.Time{}
	_, body = nudgeDeadLetterMail(item)
	if !strings.Contains(body, "First attempt: not recorded") {
		t.Fatal(body)
	}
}

func TestNudgeReplayEnqueueAndDryRun(t *testing.T) {
	dir := t.TempDir()
	store := beads.NudgesStore{Store: beads.NewMemStore()}
	now := time.Now()
	dead := newQueuedNudgeWithOptions("worker", "first\r\nsecond\n", "sling", now.Add(-2*time.Hour), queuedNudgeOptions{ID: "n-original", SessionID: "s-1", ContinuationEpoch: "1", Reference: &nudgeReference{Kind: "bead", ID: "work-1"}})
	dead.DeadAt, dead.LastError = now.Add(-2*time.Hour), "draft_ownership_lost"
	beadID, _, err := ensureQueuedNudgeBead(store, dead)
	if err != nil {
		t.Fatal(err)
	}
	dead.BeadID = beadID
	if err := nudgequeue.WithState(dir, func(s *nudgequeue.State) error { s.Dead = []queuedNudge{dead}; return nil }); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := doNudgeReplay(dir, store, dead.ID, true, &out, &stderr); code != 0 || !strings.Contains(out.String(), dead.ID) || !strings.Contains(out.String(), "worker") {
		t.Fatalf("dry run code=%d out=%s err=%s", code, &out, &stderr)
	}
	state, err := nudgequeue.LoadState(dir)
	if err != nil || len(state.Pending) != 0 || state.Dead[0].ReplayID != "" {
		t.Fatalf("dry run wrote: %+v %v", state, err)
	}
	out.Reset()
	if code := doNudgeReplay(dir, store, dead.ID, false, &out, &stderr); code != 0 {
		t.Fatalf("replay code=%d err=%s", code, &stderr)
	}
	state, err = nudgequeue.LoadState(dir)
	if err != nil || len(state.Pending) != 1 || len(state.Dead) != 1 {
		t.Fatalf("replay state=%+v err=%v", state, err)
	}
	item := state.Pending[0]
	if item.Message != dead.Message || item.OriginID != dead.ID || state.Dead[0].ReplayID != item.ID || !strings.Contains(out.String(), item.ID) || item.SessionID != dead.SessionID || item.ContinuationEpoch != dead.ContinuationEpoch {
		t.Fatalf("replay lineage/input: %+v", state)
	}
	shadow, found, err := nudgeFrontDoor(store).FindIncludingTerminal(dead.ID)
	if err != nil || !found || !nudgequeue.IsTerminalState(shadow.State) {
		t.Fatalf("retained replay receipt prevented terminal shadow repair: %+v found=%t err=%v", shadow, found, err)
	}
	if code := doNudgeReplay(dir, store, dead.ID, false, &out, &stderr); code == 0 || !strings.Contains(stderr.String(), item.ID) {
		t.Fatalf("repeat replay code=%d err=%s", code, &stderr)
	}
	// A stale concurrent preview must be rejected by the normal enqueue's lock.
	stale, err := nudgequeue.PrepareReplay(&nudgeQueueState{Dead: []queuedNudge{dead}}, dead.ID, "n-racing", now, defaultQueuedNudgeTTL)
	if err != nil {
		t.Fatal(err)
	}
	if err := enqueueQueuedNudgeWithStore(dir, store, stale); err == nil || !strings.Contains(err.Error(), item.ID) {
		t.Fatalf("stale enqueue accepted: %v", err)
	}
}

func TestNudgeDeadLetterFirstAttemptExcludesManualDrop(t *testing.T) {
	now := time.Now().UTC()
	item := newQueuedNudge("worker", "never attempted", now)
	for _, cause := range []error{errNudgeManualDrop, errNudgeSessionFenceMismatch} {
		dead, _ := failedQueuedNudge(item, cause, now)
		if !dead.FirstAttemptAt.IsZero() {
			t.Fatalf("%v invented a delivery attempt: %s", cause, dead.FirstAttemptAt)
		}
	}
	item, _ = failedQueuedNudge(item, runtime.ErrNudgeDraftUnowned, now)
	item, _ = failedQueuedNudge(item, runtime.ErrNudgeDraftUnowned, now.Add(time.Second))
	if !item.FirstAttemptAt.Equal(now) {
		t.Fatalf("retry replaced first attempt: %+v", item)
	}
}
