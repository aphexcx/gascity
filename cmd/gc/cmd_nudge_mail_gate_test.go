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

func mailRef(id string) *nudgeReference {
	return &nudgeReference{Kind: nudgeReferenceKindMail, ID: id}
}

// TestBlockedQueuedNudgeReason_MailReadState is the spec for the mail half of
// the delivery gate. A mail reminder is gated only when it carries provenance
// (a `mail` reference to the message it announces) AND the delivering
// process has a mail-state lookup: unread delivers, read withdraws as
// mail-read, a vanished message withdraws as mail-gone, a lookup error holds
// (returned to the caller, which releases the claim). No provenance or no
// lookup delivers as before, and other sources are never touched.
func TestBlockedQueuedNudgeReason_MailReadState(t *testing.T) {
	lookupErr := errors.New("store unavailable")
	state := func(st mailReminderState) func(string) (mailReminderState, error) {
		return func(string) (mailReminderState, error) { return st, nil }
	}
	withProv := queuedNudge{Source: "mail", Reference: mailRef("gc-msg1")}
	cases := []struct {
		name       string
		gate       queuedNudgeDeliveryGate
		item       queuedNudge
		wantReason string
		wantBlock  bool
		wantErr    error
	}{
		{"unread-delivers", queuedNudgeDeliveryGate{mailState: state(mailReminderUnread)}, withProv, "", false, nil},
		{"read-withdraws", queuedNudgeDeliveryGate{mailState: state(mailReminderRead)}, withProv, nudgeBlockReasonMailRead, true, nil},
		{"gone-withdraws", queuedNudgeDeliveryGate{mailState: state(mailReminderGone)}, withProv, nudgeBlockReasonMailGone, true, nil},
		{"lookup-error-holds", queuedNudgeDeliveryGate{mailState: func(string) (mailReminderState, error) { return mailReminderUnread, lookupErr }}, withProv, "", false, lookupErr},
		{"no-provenance-delivers-even-if-read", queuedNudgeDeliveryGate{mailState: state(mailReminderRead)}, queuedNudge{Source: "mail"}, "", false, nil},
		{"foreign-reference-kind-delivers", queuedNudgeDeliveryGate{mailState: state(mailReminderRead)}, queuedNudge{Source: "mail", Reference: &nudgeReference{Kind: "bead", ID: "x"}}, "", false, nil},
		{"empty-reference-id-delivers", queuedNudgeDeliveryGate{mailState: state(mailReminderRead)}, queuedNudge{Source: "mail", Reference: mailRef(" ")}, "", false, nil},
		{"no-lookup-delivers", queuedNudgeDeliveryGate{}, withProv, "", false, nil},
		{"session-source-ignores-mail-state", queuedNudgeDeliveryGate{mailState: state(mailReminderRead)}, queuedNudge{Source: "session", Reference: mailRef("gc-msg1")}, "", false, nil},
		{"queue-source-ignores-mail-state", queuedNudgeDeliveryGate{mailState: state(mailReminderRead)}, queuedNudge{Source: "queue"}, "", false, nil},
		{"wait-source-without-front-door-passes", queuedNudgeDeliveryGate{mailState: state(mailReminderRead)}, queuedNudge{Source: "wait", Reference: &nudgeReference{Kind: "bead", ID: "x"}}, "", false, nil},
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

func TestSplitQueuedNudgesForDelivery_WithdrawsReadMailRemindersOnly(t *testing.T) {
	calls := map[string]int{}
	gate := queuedNudgeDeliveryGate{
		sessFront: sessionFrontDoor(beads.NewMemStore()),
		mailState: memoizeMailStateLookup(func(id string) (mailReminderState, error) {
			calls[id]++
			if id == "read-msg" {
				return mailReminderRead, nil
			}
			return mailReminderUnread, nil
		}),
	}
	deliverable, blocked, held, err := splitQueuedNudgesForDelivery(gate, []queuedNudge{
		{ID: "m1", Agent: "worker", Source: "mail", Message: "You have mail from human", Reference: mailRef("read-msg")},
		{ID: "m2", Agent: "worker", Source: "mail", Message: "You have mail from human", Reference: mailRef("read-msg")},
		{ID: "m3", Agent: "worker", Source: "mail", Message: "You have mail from mayor", Reference: mailRef("unread-msg")},
		{ID: "m4", Agent: "worker", Source: "mail", Message: "You have mail from human"}, // no provenance
		{ID: "s1", Agent: "worker", Source: "session", Message: "check for assigned work"},
	})
	if err != nil || len(held) != 0 {
		t.Fatalf("splitQueuedNudgesForDelivery: err=%v held=%#v", err, held)
	}
	var ids []string
	for _, item := range deliverable {
		ids = append(ids, item.ID)
	}
	if strings.Join(ids, ",") != "m3,m4,s1" {
		t.Fatalf("deliverable = %v, want m3,m4,s1", ids)
	}
	if got := blocked[nudgeBlockReasonMailRead]; len(got) != 2 {
		t.Fatalf("blocked = %#v, want m1 and m2 under %s", blocked, nudgeBlockReasonMailRead)
	}
	if calls["read-msg"] != 1 || calls["unread-msg"] != 1 {
		t.Fatalf("lookup calls = %v, want one per message (memoized per pass)", calls)
	}
}

// TestSplitQueuedNudgesForDelivery_MailLookupErrorHoldsOnlyThatItem pins the
// codex round-5 finding: a failed gate read holds the affected reminder and
// nothing else — unrelated session nudges and readable reminders in the same
// batch are still classified.
func TestSplitQueuedNudgesForDelivery_MailLookupErrorHoldsOnlyThatItem(t *testing.T) {
	lookupErr := errors.New("store unavailable")
	gate := queuedNudgeDeliveryGate{mailState: func(id string) (mailReminderState, error) {
		switch id {
		case "broken":
			return mailReminderUnread, lookupErr
		case "read":
			return mailReminderRead, nil
		default:
			return mailReminderUnread, nil
		}
	}}
	deliverable, blocked, held, err := splitQueuedNudgesForDelivery(gate, []queuedNudge{
		{ID: "m1", Source: "mail", Reference: mailRef("broken")},
		{ID: "m2", Source: "mail", Reference: mailRef("read")},
		{ID: "m3", Source: "mail", Reference: mailRef("unread")},
		{ID: "s1", Source: "session"},
		{ID: "m4", Source: "mail", Reference: mailRef("broken")},
	})
	if !errors.Is(err, lookupErr) {
		t.Fatalf("err = %v, want the lookup error for diagnostics", err)
	}
	ids := func(items []queuedNudge) string {
		var out []string
		for _, item := range items {
			out = append(out, item.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids(held); got != "m1,m4" {
		t.Fatalf("held = %s, want m1,m4", got)
	}
	if got := ids(deliverable); got != "m3,s1" {
		t.Fatalf("deliverable = %s, want m3,s1", got)
	}
	if got := ids(blocked[nudgeBlockReasonMailRead]); got != "m2" {
		t.Fatalf("blocked mail-read = %s, want m2", got)
	}
}

// closeCountingStore wraps a MemStore so a test can observe the mail gate
// releasing the work-store handle it opened.
type closeCountingStore struct {
	beads.Store
	closes int
}

// CloseStore satisfies the release seam closeBeadStoreHandle looks for.
//
//nolint:unparam // the seam's signature is fixed; this double never fails
func (c *closeCountingStore) CloseStore() error { c.closes++; return nil }

// TestMailStateLookupForNudgeTarget_ReadsTheMessageFromTheWorkStore pins the
// codex round-2/3 findings: the message is read through the messaging-class
// store derived from the city WORK store (never the nudges-class delivery
// store), and the handle the lookup opens is closed after each evaluation.
func TestMailStateLookupForNudgeTarget_ReadsTheMessageFromTheWorkStore(t *testing.T) {
	clearGCEnv(t)
	work := &closeCountingStore{Store: beads.NewMemStore()}
	nudgesStore := beads.NewMemStore() // relocated nudges class: holds no mail
	prev := openMailGateWorkStore
	openMailGateWorkStore = func(string) (beads.Store, error) { return work, nil }
	t.Cleanup(func() { openMailGateWorkStore = prev })

	mp := beadmail.New(work.Store)
	msg, err := mp.Send("human", "worker", "hello", "please look")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	target := nudgeTarget{cityPath: t.TempDir(), sessionID: "gc-s1", cfg: &config.City{}}
	lookup := mailStateLookupForNudgeTarget(target, nudgesStore)
	if lookup == nil {
		t.Fatal("expected a configured mail gate")
	}
	if st, err := lookup(msg.ID); err != nil || st != mailReminderUnread {
		t.Fatalf("fresh mail in the work store must read as unread: state=%v err=%v", st, err)
	}
	if work.closes != 1 {
		t.Fatalf("work store closes = %d after one lookup, want 1", work.closes)
	}
	if err := mp.MarkRead(msg.ID); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if st, err := lookup(msg.ID); err != nil || st != mailReminderRead {
		t.Fatalf("after MarkRead the message must read as read: state=%v err=%v", st, err)
	}
	if st, err := lookup("gc-does-not-exist"); err != nil || st != mailReminderGone {
		t.Fatalf("an unknown message must read as gone: state=%v err=%v", st, err)
	}
	if err := mp.Archive(msg.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if st, err := lookup(msg.ID); err != nil || st != mailReminderGone {
		t.Fatalf("an archived message must read as gone: state=%v err=%v", st, err)
	}
	if work.closes != 4 {
		t.Fatalf("work store closes = %d after four lookups, want 4", work.closes)
	}

	broken := errors.New("work store unavailable")
	openMailGateWorkStore = func(string) (beads.Store, error) { return nil, broken }
	if _, err := mailStateLookupForNudgeTarget(target, nudgesStore)(msg.ID); !errors.Is(err, broken) {
		t.Fatalf("a work-store open failure must surface as a lookup error (hold), got %v", err)
	}
}

func TestMailStateLookupForNudgeTarget_NotConfiguredCases(t *testing.T) {
	store := beads.NewMemStore()
	base := nudgeTarget{cityPath: t.TempDir(), sessionID: "gc-s1", cfg: &config.City{}}
	if got := mailStateLookupForNudgeTarget(base, nil); got != nil {
		t.Fatal("nil delivery store must yield no gate")
	}
	noCity := base
	noCity.cityPath = ""
	if got := mailStateLookupForNudgeTarget(noCity, store); got != nil {
		t.Fatal("empty city path must yield no gate")
	}
	for _, name := range []string{"fake", "fail", "exec:/bin/true"} {
		storeless := base
		storeless.cfg = &config.City{Mail: config.MailConfig{Provider: name}}
		if got := mailStateLookupForNudgeTarget(storeless, store); got != nil {
			t.Fatalf("storeless mail provider %q must yield no gate", name)
		}
	}
	if got := mailStateLookupForNudgeTarget(base, store); got == nil {
		t.Fatal("bead-backed provider with a store must yield a gate")
	}
}

// TestMailNudgeReference_StampsOnlyBeadBackedProducers pins the codex round-4
// finding: provenance is stamped only when the PRODUCING process's mail
// provider is bead-backed; a storeless producer (whose inbox no other process
// can read) or a missing message id yields no reference, so a later consumer
// on a different provider delivers the reminder unchanged.
func TestMailNudgeReference_StampsOnlyBeadBackedProducers(t *testing.T) {
	clearGCEnv(t)
	if ref := mailNudgeReference("gc-msg1"); ref == nil || ref.Kind != nudgeReferenceKindMail || ref.ID != "gc-msg1" {
		t.Fatalf("bead-backed producer should stamp the message reference, got %#v", ref)
	}
	if ref := mailNudgeReference(""); ref != nil {
		t.Fatalf("no message id must yield no reference, got %#v", ref)
	}
	for _, name := range []string{"fake", "fail", "exec:/bin/true"} {
		t.Setenv("GC_MAIL", name)
		if ref := mailNudgeReference("gc-msg1"); ref != nil {
			t.Fatalf("storeless producer %q must yield no reference, got %#v", name, ref)
		}
	}
}

// TestSendMailNotifyReferenceSupersedesSameMessageOnly: two notifies for the
// SAME message collapse to one pending reminder (queue supersession on the
// reference), while notifies for different messages stay independent — the
// #2968 contract (TestSendMailNotifyQueuesIndependentRemindersForEachMail)
// with provenance attached.
func TestSendMailNotifyReferenceSupersedesSameMessageOnly(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "mayor", Title: "Mayor", Command: "claude", WorkDir: dir, Provider: "claude", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	prevManaged := nudgeCityUsesManagedReconciler
	nudgeCityUsesManagedReconciler = func(string) bool { return false }
	t.Cleanup(func() { nudgeCityUsesManagedReconciler = prevManaged })
	target := nudgeTarget{
		cityPath:    dir,
		cfg:         &config.City{Agents: []config.Agent{{Name: "mayor", Provider: "claude"}}},
		sessionID:   info.ID,
		sessionName: info.SessionName,
		identity:    "mayor",
		agent:       config.Agent{Name: "mayor", Provider: "claude"},
	}
	for _, id := range []string{"gc-msg1", "gc-msg1", "gc-msg2"} {
		if err := sendMailNotifyWithWorker(target, store, fake, "human", id); err != nil {
			t.Fatalf("sendMailNotifyWithWorker(%s): %v", id, err)
		}
	}
	pending, inFlight, _, err := listQueuedNudgesForTarget(dir, target, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudgesForTarget: %v", err)
	}
	if len(pending)+len(inFlight) != 2 {
		t.Fatalf("pending+inFlight = %d, want 2 (msg1 superseded once, msg2 independent); pending=%#v inFlight=%#v", len(pending)+len(inFlight), pending, inFlight)
	}
	seen := map[string]bool{}
	for _, item := range append(pending, inFlight...) {
		if item.Reference == nil || item.Reference.Kind != nudgeReferenceKindMail {
			t.Fatalf("queued mail reminder must carry the message reference, got %#v", item)
		}
		seen[item.Reference.ID] = true
	}
	if !seen["gc-msg1"] || !seen["gc-msg2"] {
		t.Fatalf("expected reminders for msg1 and msg2, got %v", seen)
	}
}

// TestTryDeliverQueuedNudgesByPollerWithdrawsMailReminderOnceRead runs the
// poller consumer end to end over a real bead store: a mail reminder with
// provenance delivers while its message is unread and is withdrawn — no Nudge
// call, queue drained — once the recipient has read it.
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
		item := newQueuedNudgeWithOptions("worker", "You have mail from human", "mail", time.Now().Add(-time.Minute), queuedNudgeOptions{Reference: mailRef(msg.ID)})
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
// through the UserPromptSubmit drain hook, plus the cross-process case codex
// round 4 asked for: a reminder without provenance (storeless producer, or an
// older item) is delivered by a bead-backed consumer even when its inbox is
// otherwise fully read.
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
	enqueue := func(ref *nudgeReference) {
		t.Helper()
		item := newQueuedNudgeWithOptions("worker", "You have mail from human", "mail", time.Now().Add(-time.Minute), queuedNudgeOptions{SessionID: created.ID, Reference: ref})
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
	queueEmpty := func() bool {
		t.Helper()
		target, err := resolveNudgeTarget(created.ID)
		if err != nil {
			t.Fatalf("resolveNudgeTarget: %v", err)
		}
		pending, inFlight, _, err := listQueuedNudgesForTarget(cityDir, target, time.Now())
		if err != nil {
			t.Fatalf("listQueuedNudgesForTarget: %v", err)
		}
		return len(pending) == 0 && len(inFlight) == 0
	}

	enqueue(mailRef(msg.ID))
	if got := drain(); !strings.Contains(got, "You have mail from human") {
		t.Fatalf("unread mail: reminder should be injected, got %q", got)
	}

	if err := mp.MarkRead(msg.ID); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	enqueue(mailRef(msg.ID))
	if got := drain(); strings.Contains(got, "You have mail") {
		t.Fatalf("read mail: reminder must be withdrawn, got %q", got)
	}
	if !queueEmpty() {
		t.Fatal("withdrawn reminder must leave the queue")
	}

	// Cross-process provenance: no reference → delivered even though the
	// inbox is fully read.
	enqueue(nil)
	if got := drain(); !strings.Contains(got, "You have mail from human") {
		t.Fatalf("a reminder without provenance must be delivered unchanged, got %q", got)
	}
	if !queueEmpty() {
		t.Fatal("delivered reminder must leave the queue")
	}
}

// TestTryDeliverQueuedNudgesByPollerMailGateOutageDoesNotStarveOtherNudges
// pins the codex round-5 finding through the poller: when the messaging
// store cannot be opened, mail reminders with provenance are held (claims
// released, back to pending) while a session nudge in the same batch is still
// delivered.
func TestTryDeliverQueuedNudgesByPollerMailGateOutageDoesNotStarveOtherNudges(t *testing.T) {
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

	outage := errors.New("messaging store down")
	prev := openMailGateWorkStore
	openMailGateWorkStore = func(string) (beads.Store, error) { return nil, outage }
	t.Cleanup(func() { openMailGateWorkStore = prev })

	now := time.Now().Add(-time.Minute)
	if err := enqueueQueuedNudge(dir, newQueuedNudgeWithOptions("worker", "You have mail from human", "mail", now, queuedNudgeOptions{Reference: mailRef("gc-msg1")})); err != nil {
		t.Fatalf("enqueue mail: %v", err)
	}
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "check for assigned work", now)); err != nil {
		t.Fatalf("enqueue session: %v", err)
	}

	delivered, err := tryDeliverQueuedNudgesByPoller(target, store.Store, store.Store, fake, 3*time.Second, obs)
	if !delivered {
		t.Fatalf("the session nudge must still be delivered during a mail-gate outage (err=%v)", err)
	}
	if !errors.Is(err, outage) {
		t.Fatalf("the hold reason should ride on the returned error, got %v", err)
	}
	var texts []string
	for _, call := range fake.Calls {
		if call.Method == "Nudge" {
			texts = append(texts, call.Message)
		}
	}
	if len(texts) != 1 || !strings.Contains(texts[0], "check for assigned work") || strings.Contains(texts[0], "You have mail") {
		t.Fatalf("exactly the session nudge should have been delivered, got %q", texts)
	}
	pending, inFlight, _, err := listQueuedNudgesForTarget(dir, target, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudgesForTarget: %v", err)
	}
	if len(pending) != 1 || pending[0].Source != "mail" || len(inFlight) != 0 {
		t.Fatalf("the held mail reminder must be back in pending with its claim released: pending=%#v inFlight=%#v", pending, inFlight)
	}
}
