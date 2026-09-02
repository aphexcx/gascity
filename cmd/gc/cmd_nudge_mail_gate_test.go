package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// TestBlockedQueuedNudgeReason_MailReadState is the spec for the mail half of
// the delivery gate: a mail-sourced reminder delivers only while the recipient
// still has unread mail; a read inbox withdraws it; a lookup error holds it
// (returned to the caller, which releases the claim); no configured lookup
// delivers as before; and the gate never touches other sources.
func TestBlockedQueuedNudgeReason_MailReadState(t *testing.T) {
	lookupErr := errors.New("store unavailable")
	unread := func(v bool) func() (bool, error) { return func() (bool, error) { return v, nil } }
	cases := []struct {
		name       string
		gate       queuedNudgeDeliveryGate
		item       queuedNudge
		wantReason string
		wantBlock  bool
		wantErr    error
	}{
		{"mail-unread-delivers", queuedNudgeDeliveryGate{unreadMail: unread(true)}, queuedNudge{Source: "mail"}, "", false, nil},
		{"mail-read-withdraws", queuedNudgeDeliveryGate{unreadMail: unread(false)}, queuedNudge{Source: "mail"}, nudgeBlockReasonMailRead, true, nil},
		{"mail-lookup-error-holds", queuedNudgeDeliveryGate{unreadMail: func() (bool, error) { return false, lookupErr }}, queuedNudge{Source: "mail"}, "", false, lookupErr},
		{"mail-no-gate-delivers", queuedNudgeDeliveryGate{}, queuedNudge{Source: "mail"}, "", false, nil},
		{"session-source-ignores-read-inbox", queuedNudgeDeliveryGate{unreadMail: unread(false)}, queuedNudge{Source: "session"}, "", false, nil},
		{"queue-source-ignores-read-inbox", queuedNudgeDeliveryGate{unreadMail: unread(false)}, queuedNudge{Source: "queue"}, "", false, nil},
		{"wait-source-without-front-door-passes", queuedNudgeDeliveryGate{unreadMail: unread(false)}, queuedNudge{Source: "wait", Reference: &nudgeReference{Kind: "bead", ID: "x"}}, "", false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, block, err := blockedQueuedNudgeReason(tc.gate, tc.item)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if reason != tc.wantReason || block != tc.wantBlock {
				t.Fatalf("got (%q, %v), want (%q, %v)", reason, block, tc.wantReason, tc.wantBlock)
			}
		})
	}
}

func TestSplitQueuedNudgesForDelivery_WithdrawsReadMailReminderOnly(t *testing.T) {
	calls := 0
	gate := queuedNudgeDeliveryGate{
		sessFront:  sessionFrontDoor(beads.NewMemStore()),
		unreadMail: memoizeUnreadMailLookup(func() (bool, error) { calls++; return false, nil }),
	}
	deliverable, blocked, err := splitQueuedNudgesForDelivery(gate, []queuedNudge{
		{ID: "m1", Agent: "worker", Source: "mail", Message: "You have mail from human"},
		{ID: "m2", Agent: "worker", Source: "mail", Message: "You have mail from mayor"},
		{ID: "s1", Agent: "worker", Source: "session", Message: "check for assigned work"},
	})
	if err != nil {
		t.Fatalf("splitQueuedNudgesForDelivery: %v", err)
	}
	if len(deliverable) != 1 || deliverable[0].ID != "s1" {
		t.Fatalf("deliverable = %#v, want only s1", deliverable)
	}
	if got := blocked[nudgeBlockReasonMailRead]; len(got) != 2 {
		t.Fatalf("blocked = %#v, want m1 and m2 under %s", blocked, nudgeBlockReasonMailRead)
	}
	if calls != 1 {
		t.Fatalf("unread lookup ran %d times, want 1 (memoized per pass)", calls)
	}
}

func TestSplitQueuedNudgesForDelivery_MailLookupErrorReturnsError(t *testing.T) {
	lookupErr := errors.New("store unavailable")
	gate := queuedNudgeDeliveryGate{unreadMail: func() (bool, error) { return false, lookupErr }}
	_, _, err := splitQueuedNudgesForDelivery(gate, []queuedNudge{{ID: "m1", Source: "mail"}})
	if !errors.Is(err, lookupErr) {
		t.Fatalf("err = %v, want the lookup error so the caller releases the claim", err)
	}
}

// closeCountingStore wraps a MemStore so a test can observe the mail gate
// releasing the work-store handle it opened.
type closeCountingStore struct {
	beads.Store
	closes int
}

func (c *closeCountingStore) CloseStore() error { c.closes++; return nil }

// TestMailUnreadLookupForNudgeTarget_ReadsMailFromTheWorkStore pins the codex
// round-2/3 findings: both the mailbox resolution and the unread read derive
// from the city WORK store (never the nudges-class delivery store), and the
// handle the lookup opens is closed after each evaluation. With the stores
// split, a session and its mail that live only in the work store must still
// resolve and count as unread.
func TestMailUnreadLookupForNudgeTarget_ReadsMailFromTheWorkStore(t *testing.T) {
	clearGCEnv(t)
	work := &closeCountingStore{Store: beads.NewMemStore()}
	nudgesStore := beads.NewMemStore() // relocated nudges class: holds neither sessions nor mail
	prev := openMailGateWorkStore
	openMailGateWorkStore = func(string) (beads.Store, error) { return work, nil }
	t.Cleanup(func() { openMailGateWorkStore = prev })

	sess, err := work.Create(beads.Bead{
		Title: "Session: worker", Type: session.BeadType, Status: "open",
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"session_name": "worker-session", "agent_name": "worker", "state": string(session.StateActive)},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	mp := beadmail.New(work.Store)
	msg, err := mp.Send("human", sess.ID, "hello", "please look")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	target := nudgeTarget{cityPath: t.TempDir(), sessionID: sess.ID, cfg: &config.City{}}
	lookup := mailUnreadLookupForNudgeTarget(target, nudgesStore)
	if lookup == nil {
		t.Fatal("expected a configured mail gate")
	}
	unread, err := lookup()
	if err != nil || !unread {
		t.Fatalf("mail in the work store must count as unread: unread=%v err=%v", unread, err)
	}
	if work.closes != 1 {
		t.Fatalf("work store closes = %d after one lookup, want 1", work.closes)
	}
	if err := mp.MarkRead(msg.ID); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if unread, err := lookup(); err != nil || unread {
		t.Fatalf("after MarkRead the inbox must read as empty: unread=%v err=%v", unread, err)
	}
	if work.closes != 2 {
		t.Fatalf("work store closes = %d after two lookups, want 2", work.closes)
	}

	broken := errors.New("work store unavailable")
	openMailGateWorkStore = func(string) (beads.Store, error) { return nil, broken }
	if _, err := mailUnreadLookupForNudgeTarget(target, nudgesStore)(); !errors.Is(err, broken) {
		t.Fatalf("a work-store open failure must surface as a lookup error (hold), got %v", err)
	}
}

func TestMailUnreadLookupForNudgeTarget_NotConfiguredCases(t *testing.T) {
	store := beads.NewMemStore()
	base := nudgeTarget{cityPath: t.TempDir(), sessionID: "gc-s1", cfg: &config.City{}}
	if got := mailUnreadLookupForNudgeTarget(base, nil); got != nil {
		t.Fatal("nil delivery store must yield no gate")
	}
	noCity := base
	noCity.cityPath = ""
	if got := mailUnreadLookupForNudgeTarget(noCity, store); got != nil {
		t.Fatal("empty city path must yield no gate")
	}
	noIdentity := base
	noIdentity.sessionID = ""
	if got := mailUnreadLookupForNudgeTarget(noIdentity, store); got != nil {
		t.Fatal("a target with no identity must yield no gate")
	}
	for _, name := range []string{"fake", "fail", "exec:/bin/true"} {
		storeless := base
		storeless.cfg = &config.City{Mail: config.MailConfig{Provider: name}}
		if got := mailUnreadLookupForNudgeTarget(storeless, store); got != nil {
			t.Fatalf("storeless mail provider %q must yield no gate", name)
		}
	}
	if got := mailUnreadLookupForNudgeTarget(base, store); got == nil {
		t.Fatal("bead-backed provider with a store and identity must yield a gate")
	}
}

// TestTryDeliverQueuedNudgesByPollerWithdrawsMailReminderOnceRead runs the
// poller consumer end to end over a real bead store: a mail reminder delivers
// while the mail is unread and is withdrawn — no Nudge call, queue drained —
// once the recipient has read it.
func TestTryDeliverQueuedNudgesByPollerWithdrawsMailReminderOnceRead(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "codex", WorkDir: dir, Provider: "codex", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	idleSince := time.Now().Add(-10 * time.Second)
	fake.Activity = map[string]time.Time{info.SessionName: idleSince}
	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "worker"},
		sessionID:   info.ID,
		resolved:    &config.ResolvedProvider{Name: "codex"},
		sessionName: info.SessionName,
	}
	obs := worker.LiveObservation{Running: true, LastActivity: &idleSince}
	nudgeCalls := func() int {
		n := 0
		for _, call := range fake.Calls {
			if call.Method == "Nudge" {
				n++
			}
		}
		return n
	}

	mp := beadmail.New(store.Store)
	msg, err := mp.Send("human", info.ID, "hello", "please look")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	enqueue := func() {
		t.Helper()
		item := newQueuedNudgeWithOptions("worker", "You have mail from human", "mail", time.Now().Add(-time.Minute), queuedNudgeOptions{})
		if err := enqueueQueuedNudge(dir, item); err != nil {
			t.Fatalf("enqueueQueuedNudge: %v", err)
		}
	}

	enqueue()
	delivered, err := tryDeliverQueuedNudgesByPoller(target, store.Store, store.Store, fake, 3*time.Second, obs)
	if err != nil {
		t.Fatalf("tryDeliverQueuedNudgesByPoller (unread): %v", err)
	}
	if !delivered || nudgeCalls() != 1 {
		t.Fatalf("unread mail: delivered=%v nudges=%d, want delivered with one Nudge call", delivered, nudgeCalls())
	}

	if err := mp.MarkRead(msg.ID); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	enqueue()
	delivered, err = tryDeliverQueuedNudgesByPoller(target, store.Store, store.Store, fake, 3*time.Second, obs)
	if err != nil {
		t.Fatalf("tryDeliverQueuedNudgesByPoller (read): %v", err)
	}
	if delivered || nudgeCalls() != 1 {
		t.Fatalf("read mail: delivered=%v nudges=%d, want withdrawn with no new Nudge call", delivered, nudgeCalls())
	}
	pending, inFlight, _, err := listQueuedNudgesForTarget(dir, target, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudgesForTarget: %v", err)
	}
	if len(pending) != 0 || len(inFlight) != 0 {
		t.Fatalf("withdrawn reminder must leave the queue: pending=%#v inFlight=%#v", pending, inFlight)
	}
}

// TestCmdNudgeDrainInjectWithdrawsMailReminderOnceRead is the same contract
// through the UserPromptSubmit drain hook.
func TestCmdNudgeDrainInjectWithdrawsMailReminderOnceRead(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_INJECT_CLOCK", "")

	cityDir := t.TempDir()
	writeNamedSessionCityTOML(t, cityDir)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_ALIAS", "worker")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
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
	mp := beadmail.New(store)
	msg, err := mp.Send("human", created.ID, "hello", "please look")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	enqueue := func() {
		t.Helper()
		item := newQueuedNudgeWithOptions("worker", "You have mail from human", "mail", time.Now().Add(-time.Minute), queuedNudgeOptions{SessionID: created.ID})
		if err := enqueueQueuedNudgeWithStore(cityDir, beads.NudgesStore{Store: store}, item); err != nil {
			t.Fatalf("enqueueQueuedNudgeWithStore: %v", err)
		}
	}
	drain := func() string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := cmdNudgeDrainWithFormat([]string{created.ID}, true, "codex", &stdout, &stderr); code != 0 {
			t.Fatalf("cmdNudgeDrainWithFormat = %d, want 0; stderr=%s", code, stderr.String())
		}
		if stdout.Len() == 0 {
			return ""
		}
		var doc map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
			t.Fatalf("decode: %v; raw=%s", err, stdout.String())
		}
		hook, _ := doc["hookSpecificOutput"].(map[string]any)
		ctx, _ := hook["additionalContext"].(string)
		return ctx
	}

	enqueue()
	if got := drain(); !strings.Contains(got, "You have mail from human") {
		t.Fatalf("unread mail: reminder should be injected, got %q", got)
	}

	if err := mp.MarkRead(msg.ID); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	enqueue()
	if got := drain(); strings.Contains(got, "You have mail") {
		t.Fatalf("read mail: reminder must be withdrawn, got %q", got)
	}
	target, err := resolveNudgeTarget(created.ID)
	if err != nil {
		t.Fatalf("resolveNudgeTarget: %v", err)
	}
	pending, inFlight, _, err := listQueuedNudgesForTarget(cityDir, target, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudgesForTarget: %v", err)
	}
	if len(pending) != 0 || len(inFlight) != 0 {
		t.Fatalf("withdrawn reminder must leave the queue: pending=%#v inFlight=%#v", pending, inFlight)
	}
}
