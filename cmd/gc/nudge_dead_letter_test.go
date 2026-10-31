package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
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
	nudgeDeadLetterNotify = func(cityPath string, dead queuedNudge, _ io.Writer) error {
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
	nudgeDeadLetterNotify = func(_ string, _ queuedNudge, _ io.Writer) error { calls++; return nil }
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

func TestNudgeDeadLetterCapReportsLossAfterCommit(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	var warnings bytes.Buffer
	oldWriter, oldLossNotify := nudgeWarningWriter, nudgeDeadLetterLossNotify
	nudgeWarningWriter = &warnings
	t.Cleanup(func() { nudgeWarningWriter, nudgeDeadLetterLossNotify = oldWriter, oldLossNotify })
	calls := 0
	nudgeDeadLetterLossNotify = func(cityPath string, item queuedNudge, _ io.Writer) error {
		calls++
		state, err := nudgequeue.LoadState(cityPath)
		if err != nil || len(state.Dead) != 0 {
			return fmt.Errorf("loss mail preceded commit: %+v: %w", state, err)
		}
		subject, body := nudgeDeadLetterLossMail(item)
		if !strings.Contains(subject, "LOST") || !strings.Contains(body, "n-lost") || strings.Contains(body, "gc nudge replay") {
			return fmt.Errorf("loss mail promises recovery: %s %s", subject, body)
		}
		return nil
	}
	if err := nudgequeue.WithState(dir, func(s *nudgequeue.State) error {
		s.Dead = []queuedNudge{
			{ID: "n-lost", Agent: "worker", Message: "unreplayed input", DeadAt: now.Add(-8 * 24 * time.Hour), LastError: "expired"},
			{ID: "n-replayed", ReplayID: "successor", DeadAt: now.Add(-8 * 24 * time.Hour)},
			{ID: "n-dismissed", DismissedAt: now.Add(-time.Hour), DeadAt: now.Add(-8 * 24 * time.Hour)},
			{ID: "n-replaced", LastError: "superseded", DeadAt: now.Add(-8 * 24 * time.Hour)},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := withNudgeQueueState(dir, func(s *nudgeQueueState) error {
		if err := pruneDeadQueuedNudges(s, nil, now, noMaintenanceDeadline()); err != nil {
			return err
		}
		return errors.New("abort removal")
	}); err == nil || calls != 0 || warnings.Len() != 0 {
		t.Fatalf("uncommitted removal notified: calls=%d err=%v warnings=%s", calls, err, &warnings)
	}
	for range 2 {
		if err := withNudgeQueueState(dir, func(s *nudgeQueueState) error {
			return pruneDeadQueuedNudges(s, nil, now, noMaintenanceDeadline())
		}); err != nil {
			t.Fatal(err)
		}
	}
	state, err := nudgequeue.LoadState(dir)
	if err != nil || len(state.Dead) != 0 || calls != 1 || strings.Count(warnings.String(), "retention cap") != 1 || !strings.Contains(warnings.String(), "n-lost") || strings.Contains(warnings.String(), "mail failed") {
		t.Fatalf("cap loss state=%+v err=%v warnings=%s", state, err, &warnings)
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
	item.Reference = &nudgeReference{Kind: "bead", ID: "missing"}
	failedLookup := func(string) (beads.Bead, error) { return beads.Bead{}, errors.New("store unavailable") }
	if to, err := nudgeDeadLetterRecipient(item, "", failedLookup); to != "" || err == nil || !strings.Contains(err.Error(), "session.park_alert_to") {
		t.Fatalf("failed lookup omitted missing fallback: to=%q err=%v", to, err)
	}
}

func TestNudgeDeadLetterSlowMailCannotStallQueue(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	oldNotify, oldWriter := nudgeDeadLetterNotify, nudgeWarningWriter
	t.Cleanup(func() { nudgeDeadLetterNotify, nudgeWarningWriter = oldNotify, oldWriter })
	var warnings bytes.Buffer
	nudgeWarningWriter = &warnings
	items := []queuedNudge{{ID: "slow-1"}, {ID: "slow-2"}, {ID: "slow-3"}}
	release := make(chan struct{})
	entered := make(chan struct{}, len(items))
	finished := make(chan struct{}, len(items))
	nudgeDeadLetterNotify = func(_ string, _ queuedNudge, log io.Writer) error {
		entered <- struct{}{}
		select {
		case <-release:
		case <-time.After(2 * nudgeDeadLetterMailBudget):
		}
		_, _ = io.WriteString(log, "late provider diagnostic\n")
		finished <- struct{}{}
		return nil
	}
	dir := t.TempDir()
	now := time.Now()
	for i := range items {
		items[i].Agent = "expired"
		items[i].ExpiresAt = now.Add(-time.Second)
	}
	live := newQueuedNudge("worker", "deliver without waiting for unrelated mail", now)
	if err := nudgequeue.WithState(dir, func(s *nudgeQueueState) error {
		s.Pending = append(append([]queuedNudge{}, items...), live)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	claimed, err := claimDueQueuedNudgesMatching(dir, now, func(item queuedNudge) bool { return item.Agent == "worker" })
	close(release)
	if elapsed := time.Since(start); elapsed > nudgeDeadLetterMailBudget+time.Second {
		t.Fatalf("mail blocked queue for %s", elapsed)
	}
	if err != nil || len(claimed) != 1 || claimed[0].ID != live.ID || !claimed[0].LeaseUntil.After(time.Now()) {
		t.Fatalf("claim did not return with a live lease: items=%+v err=%v", claimed, err)
	}
	if len(entered) != len(items) {
		t.Errorf("attempted %d notifications, want %d", len(entered), len(items))
	}
	before := warnings.String()
	for _, item := range items {
		if !strings.Contains(before, item.ID) || !strings.Contains(before, "deadline exceeded") {
			t.Errorf("missing timeout diagnostic for %s: %s", item.ID, before)
		}
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatal("provider did not finish after release")
		}
	}
	if warnings.String() != before {
		t.Fatalf("provider wrote caller diagnostics after timeout: %s", &warnings)
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
	// Routine maintenance must preserve the recovery command beyond one hour.
	if err := withNudgeQueueState(dir, func(s *nudgeQueueState) error {
		return pruneDeadQueuedNudges(s, nudgeFrontDoor(store), now, noMaintenanceDeadline())
	}); err != nil {
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

func TestNudgeDeadLetterSupersessionDoesNotAlertOrReplay(t *testing.T) {
	dir := t.TempDir()
	store := beads.NudgesStore{Store: beads.NewMemStore()}
	now := time.Now()
	oldNotify, oldWriter := nudgeDeadLetterNotify, nudgeWarningWriter
	t.Cleanup(func() { nudgeDeadLetterNotify, nudgeWarningWriter = oldNotify, oldWriter })
	var warnings bytes.Buffer
	nudgeWarningWriter = &warnings
	calls := 0
	nudgeDeadLetterNotify = func(string, queuedNudge, io.Writer) error { calls++; return nil }
	old := newQueuedNudgeWithOptions("worker", "old reminder", "sling", now, queuedNudgeOptions{ID: "n-old", Reference: &nudgeReference{Kind: "bead", ID: "work-1"}})
	newer := old
	newer.ID, newer.Message = "n-new", "new reminder"
	for _, item := range []queuedNudge{old, newer} {
		if err := enqueueQueuedNudgeWithStore(dir, store, item); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 0 || warnings.Len() != 0 {
		t.Errorf("supersession sent failure notification: calls=%d warnings=%s", calls, &warnings)
	}
	var out, stderr bytes.Buffer
	if code := doNudgeReplay(dir, store, old.ID, false, &out, &stderr); code == 0 || !strings.Contains(stderr.String(), "superseded") || !strings.Contains(stderr.String(), old.ID) {
		t.Errorf("superseded replay code=%d err=%s", code, &stderr)
	}
	state, err := nudgequeue.LoadState(dir)
	if err != nil || len(state.Pending) != 1 || state.Pending[0].ID != newer.ID || state.Pending[0].Message != newer.Message {
		t.Fatalf("newer reminder displaced: %+v err=%v", state, err)
	}
}

func TestNudgeReplayDismissalAndDryRun(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	if err := nudgequeue.WithState(dir, func(s *nudgequeue.State) error {
		s.Dead = []queuedNudge{{ID: "n-dismiss", DeadAt: now.Add(-2 * time.Hour), LastError: "expired"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	for _, dryRun := range []bool{true, false} {
		if code := doNudgeDismiss(dir, "n-dismiss", dryRun, &out, &stderr); code != 0 {
			t.Fatalf("dismiss dryRun=%t code=%d: %s", dryRun, code, &stderr)
		}
		state, err := nudgequeue.LoadState(dir)
		if err != nil || len(state.Dead) != 1 || len(state.Pending) != 0 || state.Dead[0].DismissedAt.IsZero() != dryRun {
			t.Fatalf("dismiss dryRun=%t state=%+v err=%v", dryRun, state, err)
		}
	}
	if code := doNudgeReplay(dir, beads.NudgesStore{}, "n-dismiss", true, &out, &stderr); code == 0 || !strings.Contains(stderr.String(), "dismissed") {
		t.Fatalf("dismissed item replayed: code=%d err=%s", code, &stderr)
	}
}

func TestNudgeReplayStartsOnlyLegacyPoller(t *testing.T) {
	for _, mode := range []string{"legacy", "supervisor"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			configText := "[workspace]\nname = \"replay-test\"\n[daemon]\nnudge_dispatcher = \"" + mode + "\"\n"
			if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(configText), 0o600); err != nil {
				t.Fatal(err)
			}
			store := beads.NudgesStore{Store: beads.NewMemStore()}
			owner, err := store.Create(beads.Bead{
				Type: session.BeadType, Labels: []string{session.LabelSession},
				Metadata: map[string]string{"session_name": "worker-runtime", "template": "worker", "provider": "codex"},
			})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			dead := newQueuedNudgeWithOptions("worker", "replay", "session", now, queuedNudgeOptions{SessionID: owner.ID})
			dead.DeadAt = now
			if err := nudgequeue.WithState(dir, func(s *nudgeQueueState) error { s.Dead = []queuedNudge{dead}; return nil }); err != nil {
				t.Fatal(err)
			}
			oldStart, starts := startNudgePoller, 0
			t.Cleanup(func() { startNudgePoller = oldStart })
			startNudgePoller = func(cityPath, agentName, sessionName string) error {
				starts++
				if cityPath != dir || agentName != owner.ID || sessionName != "worker-runtime" {
					t.Fatalf("unexpected poller destination: %q %q %q", cityPath, agentName, sessionName)
				}
				return nil
			}
			var out, stderr bytes.Buffer
			if code := doNudgeReplay(dir, store, dead.ID, true, &out, &stderr); code != 0 || starts != 0 {
				t.Fatalf("dry run started delivery: code=%d starts=%d stderr=%s", code, starts, &stderr)
			}
			if code := doNudgeReplay(dir, store, dead.ID, false, &out, &stderr); code != 0 {
				t.Fatalf("replay failed: %s", &stderr)
			}
			want := 0
			if mode == "legacy" {
				want = 1
			}
			if starts != want {
				t.Fatalf("poller starts=%d, want %d; stderr=%s", starts, want, &stderr)
			}
		})
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

func TestNudgeDeadLetterOperatorDropIsDisposition(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	store := beads.NudgesStore{Store: beads.NewMemStore()}
	item := newQueuedNudge("worker", "intentionally discarded", now)
	if err := enqueueQueuedNudgeWithStore(dir, store, item); err != nil {
		t.Fatal(err)
	}
	oldNotify, oldWriter := nudgeDeadLetterNotify, nudgeWarningWriter
	t.Cleanup(func() { nudgeDeadLetterNotify, nudgeWarningWriter = oldNotify, oldWriter })
	var warnings bytes.Buffer
	nudgeWarningWriter = &warnings
	calls := 0
	nudgeDeadLetterNotify = func(string, queuedNudge, io.Writer) error { calls++; return nil }
	if err := recordQueuedNudgeFailureWithStore(dir, store, []string{item.ID}, errNudgeManualDrop, now); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || warnings.Len() != 0 {
		t.Errorf("operator drop sent failure notification: calls=%d warnings=%s", calls, &warnings)
	}
	state, err := nudgequeue.LoadState(dir)
	if err != nil || len(state.Dead) != 1 || !state.Dead[0].DismissedAt.Equal(now) || state.Dead[0].UnresolvedDeadLetter() {
		t.Errorf("drop disposition not persisted: %+v err=%v", state, err)
	}
	var out, stderr bytes.Buffer
	if code := doNudgeReplay(dir, store, item.ID, false, &out, &stderr); code == 0 || !strings.Contains(stderr.String(), "dropped") {
		t.Fatalf("operator drop was replayable: code=%d err=%s", code, &stderr)
	}
}

func TestNudgeDeadLetterExpiredDropRepairsWaitBeforePruning(t *testing.T) {
	for _, delayedRecovery := range []bool{false, true} {
		t.Run(fmt.Sprintf("delayed-recovery=%t", delayedRecovery), func(t *testing.T) {
			dir, now, store, wait := droppedWaitNudgeFixture(t)
			nudges := beads.NudgesStore{Store: store}
			maintain := func(at time.Time) {
				t.Helper()
				if err := withNudgeQueueState(dir, func(state *nudgeQueueState) error {
					return pruneDeadQueuedNudges(state, nudgeFrontDoor(nudges), at, noMaintenanceDeadline())
				}); err != nil {
					t.Fatal(err)
				}
			}
			later := now.Add(3 * time.Hour)
			if delayedRecovery {
				maintain(now.Add(2 * time.Hour))
				state, err := nudgequeue.LoadState(dir)
				if err != nil || len(state.Dead) != 1 {
					t.Fatalf("receipt lost before repair: state=%+v err=%v", state, err)
				}
				maintain(now.Add(nudgequeue.DeadLetterRetention))
				later = now.Add(nudgequeue.DeadLetterRetention + time.Hour)
			}
			store.failID = ""
			maintain(later)
			state, err := nudgequeue.LoadState(dir)
			if err != nil || len(state.Dead) != 0 {
				t.Fatalf("repaired receipt not pruned: state=%+v err=%v", state, err)
			}
			sessFront := cliSessionFrontDoor(store, nil, dir)
			done, err := finalizeReadyWaitFromNudge(sessFront, nudges, wait, later, &state)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sweepStaleNudgeMail(nudges, beads.MailStore{Store: store}, &state, later, time.Minute, 0, 0); err != nil {
				t.Fatal(err)
			}
			if err := dispatchReadyWaitNudgesWithSnapshot(dir, nil, sessFront, nudges, later, nil); err != nil {
				t.Fatal(err)
			}
			state, err = nudgequeue.LoadState(dir)
			if err != nil || len(state.Pending) != 0 || len(state.InFlight) != 0 {
				t.Fatalf("dropped wait was re-enqueued: dispatch count=%d (want 1), finalized=%t state=%+v err=%v", 1+len(state.Pending)+len(state.InFlight), done, state, err)
			}
			if !done {
				t.Fatal("dropped wait not finalized after delayed repair")
			}
		})
	}
}

func droppedWaitNudgeFixture(t *testing.T) (string, time.Time, *failingTerminalNudgeStore, session.WaitInfo) {
	t.Helper()
	dir := t.TempDir()
	now := time.Now().UTC()
	store := &failingTerminalNudgeStore{MemStore: beads.NewMemStore(), terminalOnly: true}
	nudges := beads.NudgesStore{Store: store}
	sessionBead, err := store.Create(beads.Bead{
		Type: sessionBeadType, Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{"session_name": "worker", "agent_name": "worker", "continuation_epoch": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitBead, err := store.Create(beads.Bead{
		Type: waitBeadType, Labels: []string{waitBeadLabel, "session:" + sessionBead.ID},
		Metadata: map[string]string{
			"session_id": sessionBead.ID, "session_name": "worker", "kind": "deps",
			"state": waitStateReady, "registered_epoch": "1", "delivery_attempt": "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wait := session.WaitInfoFromBead(waitBead)
	item := newQueuedNudgeWithOptions("worker", "intentionally dropped wait", "wait", now, queuedNudgeOptions{
		ID: waitNudgeID(wait), SessionID: sessionBead.ID, ContinuationEpoch: wait.RegisteredEpoch,
		Reference: &nudgeReference{Kind: "bead", ID: waitBead.ID},
	})
	if err := enqueueQueuedNudgeWithStore(dir, nudges, item); err != nil {
		t.Fatal(err)
	}
	state, err := nudgequeue.LoadState(dir)
	if err != nil || len(state.Pending) != 1 {
		t.Fatalf("initial dispatch count: state=%+v err=%v", state, err)
	}
	store.failID = state.Pending[0].BeadID
	if err := recordQueuedNudgeFailureWithStore(dir, nudges, []string{item.ID}, errNudgeManualDrop, now); err != nil {
		t.Fatal(err)
	}
	if store.terminalFailures == 0 {
		t.Fatal("terminal write failure was not exercised")
	}
	return dir, now, store, wait
}

func TestNudgeDeadLetterCapWaitDispatchBeforeRepair(t *testing.T) {
	dir, now, store, wait := droppedWaitNudgeFixture(t)
	nudges := beads.NudgesStore{Store: store}
	sessFront := cliSessionFrontDoor(store, nil, dir)
	capTime := now.Add(nudgequeue.DeadLetterRetention)
	beforeCap, err := nudgequeue.LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := sweepStaleNudgeMail(nudges, beads.MailStore{Store: store}, &beforeCap, capTime, time.Minute, 0, 0); err != nil || result.NudgeClosed != 0 {
		t.Fatalf("sweep overwrote terminalization intent before cap conversion: %+v err=%v", result, err)
	}
	var warnings bytes.Buffer
	oldWriter, oldNotify := nudgeWarningWriter, nudgeDeadLetterLossNotify
	t.Cleanup(func() { nudgeWarningWriter, nudgeDeadLetterLossNotify = oldWriter, oldNotify })
	nudgeWarningWriter = &warnings
	mailCalls := 0
	nudgeDeadLetterLossNotify = func(string, queuedNudge, io.Writer) error { mailCalls++; return nil }
	if err := withNudgeQueueState(dir, func(state *nudgeQueueState) error {
		return pruneDeadQueuedNudges(state, nil, capTime, noMaintenanceDeadline())
	}); err != nil {
		t.Fatal(err)
	}
	state, err := nudgequeue.LoadState(dir)
	if err != nil || len(state.Dead) != 0 {
		t.Fatalf("cap did not persist payload deletion: state=%+v err=%v", state, err)
	}
	if len(state.Tombstones) != 1 || !state.Tombstones[0].RepairPending || !state.Tombstones[0].DeletedAt.Equal(capTime) || !nudgeQueueHasWork(&state) {
		t.Fatalf("pending repair not persisted or maintenance would skip it: %+v", state)
	}
	if !queuedNudgeExists(&state, waitNudgeID(wait)) {
		t.Fatal("enqueue deduplication ignores the tombstone")
	}
	if _, err := nudgequeue.PrepareReplay(&state, waitNudgeID(wait), "replay-expired", capTime, time.Hour); err == nil {
		t.Fatal("expired payload can still be replayed")
	}
	data, err := os.ReadFile(nudgequeue.StatePath(dir))
	if err != nil || bytes.Contains(data, []byte("intentionally dropped wait")) {
		t.Fatalf("expired payload still persisted: %s err=%v", data, err)
	}
	for range 2 {
		if result, err := sweepStaleNudgeMail(nudges, beads.MailStore{Store: store}, &state, capTime, time.Minute, 0, 0); err != nil || result.NudgeClosed != 0 {
			t.Fatalf("stale sweep consumed pending repair: %+v err=%v", result, err)
		}
		if err := dispatchReadyWaitNudgesWithSnapshot(dir, nil, sessFront, nudges, capTime, nil); err != nil {
			t.Fatal(err)
		}
		state, err = nudgequeue.LoadState(dir)
		if err != nil || len(state.Pending) != 0 || len(state.InFlight) != 0 {
			t.Fatalf("dispatch count=%d (want 1) after persisted cap during failed repair: %+v err=%v", 1+len(state.Pending)+len(state.InFlight), state, err)
		}
	}
	updated, err := store.Get(wait.ID)
	if err != nil || updated.Metadata["state"] != waitStateFailed {
		t.Fatalf("tombstone did not finalize the wait: %+v err=%v", updated, err)
	}
	if strings.Count(warnings.String(), "retention cap") != 1 || mailCalls != 0 {
		t.Fatalf("dropped loss must log once without mailing: %s calls=%d", warnings.String(), mailCalls)
	}
	maintain := func(at time.Time) nudgeQueueState {
		t.Helper()
		if err := withNudgeQueueState(dir, func(state *nudgeQueueState) error {
			return pruneDeadQueuedNudges(state, nudgeFrontDoor(nudges), at, noMaintenanceDeadline())
		}); err != nil {
			t.Fatal(err)
		}
		state, err := nudgequeue.LoadState(dir)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	// Metadata can succeed while Close still fails. Neither failure advances
	// the receipt clock, even after another retention window has elapsed.
	state = maintain(capTime.Add(nudgequeue.DeadLetterRetention))
	if len(state.Tombstones) != 1 || !state.Tombstones[0].RepairPending {
		t.Fatalf("unfinished repair expired: %+v", state)
	}
	store.failClose = true
	state = maintain(capTime.Add(2 * nudgequeue.DeadLetterRetention))
	if len(state.Tombstones) != 1 || !state.Tombstones[0].RepairPending {
		t.Fatalf("partial terminalization advanced receipt clock: %+v", state)
	}
	store.failID = ""
	recoveredAt := capTime.Add(3 * nudgequeue.DeadLetterRetention)
	state = maintain(recoveredAt)
	if len(state.Tombstones) != 1 || state.Tombstones[0].RepairPending || !state.Tombstones[0].RepairedAt.Equal(recoveredAt) {
		t.Fatalf("repair did not start receipt retention: %+v", state)
	}
	state = maintain(recoveredAt.Add(nudgequeue.DispositionRetention - time.Nanosecond))
	if len(state.Tombstones) != 1 {
		t.Fatalf("receipt expired before one hour from repair: %+v", state)
	}
	state = maintain(recoveredAt.Add(nudgequeue.DispositionRetention))
	if len(state.Tombstones) != 0 || nudgeQueueHasWork(&state) {
		t.Fatalf("repaired receipt retained after its hour: %+v", state)
	}
	if strings.Count(warnings.String(), "retention cap") != 1 || mailCalls != 0 {
		t.Fatalf("repair retried the loss alert: %s calls=%d", warnings.String(), mailCalls)
	}
}

func TestNudgeDeadLetterDispatchFinalizationClearsOnlyIdleHold(t *testing.T) {
	for _, tc := range []struct {
		name        string
		tombstone   bool
		anotherWait bool
	}{
		{name: "terminal-shadow"},
		{name: "tombstone", tombstone: true},
		{name: "another-wait", tombstone: true, anotherWait: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, now, store, wait := droppedWaitNudgeFixture(t)
			nudges := beads.NudgesStore{Store: store}
			sessFront := cliSessionFrontDoor(store, nil, dir)
			if err := store.SetMetadataBatch(wait.SessionID, map[string]string{
				"state": "awake", "wait_hold": "true", "sleep_intent": "wait-hold", "sleep_reason": "wait-hold",
			}); err != nil {
				t.Fatal(err)
			}
			ready, err := prepareWaitWakeStateForCity(dir, store, now)
			if err != nil || !ready[wait.SessionID] {
				t.Fatalf("wait should be ready before terminalization: %v err=%v", ready, err)
			}
			if tc.anotherWait {
				if _, err := store.Create(beads.Bead{
					Type: waitBeadType, Labels: []string{waitBeadLabel, "session:" + wait.SessionID},
					Metadata: map[string]string{"session_id": wait.SessionID, "kind": "deps", "state": waitStatePending},
				}); err != nil {
					t.Fatal(err)
				}
			}
			// The terminal result arrives after preparation's chance to clear
			// the hold. Dispatch must finish both halves of that transition.
			at := now.Add(2 * time.Hour)
			front := nudgeFrontDoor(nudges)
			if tc.tombstone {
				at, front = now.Add(nudgequeue.DeadLetterRetention), nil
			} else {
				store.failID = ""
			}
			if err := withNudgeQueueState(dir, func(state *nudgeQueueState) error {
				return pruneDeadQueuedNudges(state, front, at, noMaintenanceDeadline())
			}); err != nil {
				t.Fatal(err)
			}
			if err := dispatchReadyWaitNudgesWithSnapshot(dir, nil, sessFront, nudges, at, nil); err != nil {
				t.Fatal(err)
			}
			updatedWait, err := store.Get(wait.ID)
			if err != nil || updatedWait.Status != "closed" || updatedWait.Metadata["state"] != waitStateFailed {
				t.Fatalf("dispatch did not finalize the wait: %+v err=%v", updatedWait, err)
			}
			updatedSession, err := store.Get(wait.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			for key, held := range map[string]string{"wait_hold": "true", "sleep_intent": "wait-hold", "sleep_reason": "wait-hold"} {
				want := ""
				if tc.anotherWait {
					want = held
				}
				if got := updatedSession.Metadata[key]; got != want {
					t.Errorf("%s = %q, want %q after dispatch finalization", key, got, want)
				}
			}
		})
	}
}

func TestNudgeDeadLetterDispositionRetention(t *testing.T) {
	now := time.Now()
	state := nudgeQueueState{Dead: []queuedNudge{
		{ID: "unresolved", DeadAt: now.Add(-48 * time.Hour)},
		{ID: "replayed", ReplayID: "successor", DeadAt: now.Add(-48 * time.Hour)},
		{ID: "replay-boundary", ReplayID: "boundary-successor", ReplayedAt: now.Add(-time.Hour), DeadAt: now.Add(-48 * time.Hour)},
		{ID: "recent-replay", ReplayID: "recent-successor", ReplayedAt: now.Add(-time.Hour + time.Nanosecond), DeadAt: now.Add(-48 * time.Hour)},
		{ID: "recent-dismissal", DismissedAt: now.Add(-30 * time.Minute), DeadAt: now.Add(-48 * time.Hour)},
		{ID: "dismissed", DismissedAt: now.Add(-2 * time.Hour), DeadAt: now.Add(-48 * time.Hour)},
		{ID: "superseded", LastError: "superseded", DeadAt: now.Add(-2 * time.Hour)},
	}}
	if err := pruneDeadQueuedNudges(&state, nil, now, noMaintenanceDeadline()); err != nil {
		t.Fatal(err)
	}
	if len(state.Dead) != 3 || state.Dead[0].ID != "unresolved" || state.Dead[1].ID != "recent-replay" || state.Dead[2].ID != "recent-dismissal" {
		t.Fatalf("disposed items outlived retention or unresolved item vanished: %+v", state.Dead)
	}
	if err := pruneDeadQueuedNudges(&state, nil, now.Add(time.Hour), noMaintenanceDeadline()); err != nil {
		t.Fatal(err)
	}
	if len(state.Dead) != 1 || state.Dead[0].ID != "unresolved" {
		t.Fatalf("recent receipts were retained forever: %+v", state.Dead)
	}
}

type nudgeReplayBoundaryClock struct{ now time.Time }

func (c *nudgeReplayBoundaryClock) Now() time.Time {
	now := c.now
	c.now = c.now.Add(2 * time.Nanosecond)
	return now
}

func TestNudgeReplayAtCapUsesOneTransactionTime(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	dead := queuedNudge{ID: "n-boundary", Agent: "worker", Message: "last moment recovery", DeadAt: now.Add(-nudgequeue.DeadLetterRetention + time.Nanosecond)}
	state := nudgeQueueState{Dead: []queuedNudge{dead}}
	if err := nudgequeue.WithState(dir, func(s *nudgequeue.State) error { *s = state; return nil }); err != nil {
		t.Fatal(err)
	}
	replay, err := nudgequeue.PrepareReplay(&state, dead.ID, "n-replay", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	oldNotify, oldWriter := nudgeDeadLetterLossNotify, nudgeWarningWriter
	t.Cleanup(func() { nudgeDeadLetterLossNotify, nudgeWarningWriter = oldNotify, oldWriter })
	calls := 0
	nudgeDeadLetterLossNotify = func(string, queuedNudge, io.Writer) error { calls++; return nil }
	var warnings bytes.Buffer
	nudgeWarningWriter = &warnings
	if err := enqueueQueuedNudgeWithStoreAndClock(dir, beads.NudgesStore{Store: beads.NewMemStore()}, replay, &nudgeReplayBoundaryClock{now}); err != nil {
		t.Fatal(err)
	}
	state, err = nudgequeue.LoadState(dir)
	if err != nil || len(state.Pending) != 1 || len(state.Dead) != 1 || calls != 0 || warnings.Len() != 0 {
		t.Fatalf("successful replay reported lost: state=%+v err=%v calls=%d warnings=%s", state, err, calls, &warnings)
	}
}
