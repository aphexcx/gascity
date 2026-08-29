package tmux

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// errSessionNotFoundForTest stands in for tmux refusing every command.
var errSessionNotFoundForTest = errors.New("can't find session")

// collectReceipts installs a sink on tm and returns an accessor for what it saw.
func collectReceipts(tm *Tmux) func() []runtime.NudgeReceipt {
	var mu sync.Mutex
	var got []runtime.NudgeReceipt
	tm.cfg.NudgeReceiptSink = func(r runtime.NudgeReceipt) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, r)
	}
	return func() []runtime.NudgeReceipt {
		mu.Lock()
		defer mu.Unlock()
		out := make([]runtime.NudgeReceipt, len(got))
		copy(out, got)
		return out
	}
}

// TestNudgeSessionEmitsReceiptForTheWholePayload covers the gp-2rq receipt: a
// nudge that reaches the terminal must leave evidence of WHAT reached it, so a
// sender that must not double-deliver (the Slack adapter's same-ts twin dedup,
// gp-32q) has something better to gate on than "the call returned nil".
//
// The payload here is a multi-line reminder of the shape that was arriving
// truncated: under the retired 4096-byte fast path it would have been typed as
// raw keys.
func TestNudgeSessionEmitsReceiptForTheWholePayload(t *testing.T) {
	const message = "<system-reminder>\nNew message in shared conversation slack/C0ASAPRETDK:\n\n- Afik (human): ship it\n</system-reminder>"

	fe := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = fe
	tm.cfg.DebounceMs = 0
	receipts := collectReceipts(tm)

	if err := tm.NudgeSession("gt-receipt-session", message); err != nil {
		t.Fatalf("NudgeSession: %v", err)
	}

	got := receipts()
	if len(got) != 1 {
		t.Fatalf("emitted %d receipts, want exactly 1: %+v", len(got), got)
	}
	r := got[0]
	if r.Bytes != len(message) {
		t.Errorf("receipt Bytes = %d, want %d — a receipt that under-reports the payload cannot prove the whole message landed", r.Bytes, len(message))
	}
	if want := runtime.NudgePayloadDigest(message); r.Digest != want {
		t.Errorf("receipt Digest = %q, want %q — the digest is what lets a caller several layers up name the delivery it is waiting on", r.Digest, want)
	}
	if r.Framing != runtime.NudgeFramingBracketedPaste {
		t.Errorf("receipt Framing = %q, want %q", r.Framing, runtime.NudgeFramingBracketedPaste)
	}
	if r.ID == "" {
		t.Error("receipt has no ID; gp-32q logs the receipt id to correlate a delivery")
	}
	if r.At.IsZero() {
		t.Error("receipt has no timestamp")
	}
}

// TestNudgeReceiptIDsAreUniquePerDelivery: two deliveries of identical text
// share a digest (it is content-addressed on purpose) but must not share an ID,
// or a consumer cannot tell a redelivery from the original.
func TestNudgeReceiptIDsAreUniquePerDelivery(t *testing.T) {
	fe := &fakeExecutor{}
	tm := NewTmux()
	tm.exec = fe
	tm.cfg.DebounceMs = 0
	receipts := collectReceipts(tm)

	for i := 0; i < 2; i++ {
		if err := tm.NudgeSession("gt-receipt-dupe", "same text"); err != nil {
			t.Fatalf("NudgeSession #%d: %v", i, err)
		}
	}

	got := receipts()
	if len(got) != 2 {
		t.Fatalf("emitted %d receipts, want 2", len(got))
	}
	if got[0].ID == got[1].ID {
		t.Errorf("both deliveries share receipt ID %q; a redelivery would be indistinguishable from the original", got[0].ID)
	}
	if got[0].Digest != got[1].Digest {
		t.Errorf("identical payloads produced different digests (%q vs %q); the digest must be content-addressed", got[0].Digest, got[1].Digest)
	}
}

// TestNudgeSessionEmitsNoReceiptWhenDeliveryFails: a receipt is the delivery
// claim, so it must not exist for a nudge that never reached the terminal.
func TestNudgeSessionEmitsNoReceiptWhenDeliveryFails(t *testing.T) {
	fe := &fakeExecutor{err: errSessionNotFoundForTest}
	tm := NewTmux()
	tm.exec = fe
	tm.cfg.DebounceMs = 0
	receipts := collectReceipts(tm)

	if err := tm.NudgeSession("gt-receipt-dead", "text"); err == nil {
		t.Fatal("NudgeSession() = nil, want an error for a failing tmux")
	}

	if got := receipts(); len(got) != 0 {
		t.Fatalf("emitted %d receipts for a failed delivery, want 0: %+v", len(got), got)
	}
}

// TestSendHiddenAttachedTextFramesPayloadAsOneBracketedPaste covers the second
// transport gp-2rq had to fix. The hidden-attach client writes bytes straight
// into an attached tmux client's stdin, bypassing paste-buffer, and used to
// write the payload unframed — so a multi-line reminder arrived as a run of
// Enter presses and the TUI submitted the first line alone.
//
// Measured through `script` + `tmux attach-session` against a raw-mode TUI with
// bracketed paste enabled, the markers are forwarded to the pane intact, so
// framing here is both necessary and sufficient.
func TestSendHiddenAttachedTextFramesPayloadAsOneBracketedPaste(t *testing.T) {
	const message = "line one\nline two\nline three"

	fe := &fakeExecutor{out: strconv.FormatInt(time.Now().Unix(), 10)}
	tm := NewTmux()
	tm.exec = fe
	tm.cfg.DebounceMs = 0

	const sess = "hidden-attach-framing"
	sink := &recordingWriteCloser{}
	tm.hiddenAttachMu.Lock()
	tm.hiddenAttachClients = map[string]*hiddenAttachClient{sess: {stdin: sink}}
	tm.hiddenAttachMu.Unlock()

	used, _, err := tm.sendHiddenAttachedText(sess, message)
	if err != nil {
		t.Fatalf("sendHiddenAttachedText: %v", err)
	}
	if !used {
		t.Fatal("hidden-attach branch did not run")
	}

	got := sink.written()
	// The payload must sit inside exactly one bracketed paste...
	want := bracketedPasteStart + message + bracketedPasteEnd
	if !strings.Contains(got, want) {
		t.Fatalf("hidden client received %q, want the payload framed as %q", got, want)
	}
	if n := strings.Count(got, bracketedPasteStart); n != 1 {
		t.Fatalf("payload was framed as %d pastes, want exactly 1: %q", n, got)
	}
	// ...and the submit Enter must be OUTSIDE it: inside, it is pasted text
	// rather than a keypress, and the message never submits.
	if !strings.HasSuffix(got, bracketedPasteEnd+"\r") {
		t.Fatalf("hidden client received %q, want the submit Enter after the closing paste marker", got)
	}
}

// TestSendHiddenAttachedTextEmitsReceipt: the hidden-attach transport is a
// delivery path like any other and must not be a receipt blind spot.
func TestSendHiddenAttachedTextEmitsReceipt(t *testing.T) {
	const message = "hidden payload"

	fe := &fakeExecutor{out: strconv.FormatInt(time.Now().Unix(), 10)}
	tm := NewTmux()
	tm.exec = fe
	tm.cfg.DebounceMs = 0
	receipts := collectReceipts(tm)

	const sess = "hidden-attach-receipt"
	tm.hiddenAttachMu.Lock()
	tm.hiddenAttachClients = map[string]*hiddenAttachClient{sess: {stdin: &recordingWriteCloser{}}}
	tm.hiddenAttachMu.Unlock()

	if _, _, err := tm.sendHiddenAttachedText(sess, message); err != nil {
		t.Fatalf("sendHiddenAttachedText: %v", err)
	}

	got := receipts()
	if len(got) != 1 {
		t.Fatalf("emitted %d receipts, want 1", len(got))
	}
	if got[0].Bytes != len(message) {
		t.Errorf("receipt Bytes = %d, want %d", got[0].Bytes, len(message))
	}
	if got[0].Framing != runtime.NudgeFramingBracketedPaste {
		t.Errorf("receipt Framing = %q, want %q", got[0].Framing, runtime.NudgeFramingBracketedPaste)
	}
	if got[0].Target != sess {
		t.Errorf("receipt Target = %q, want %q", got[0].Target, sess)
	}
}

// TestNudgeDeliveredReportsFalseWhenSessionIsGone is the guard against the
// worst failure this receipt work can have: vouching for a delivery that never
// happened.
//
// Provider.Nudge is best-effort by contract — a missing session or dead tmux
// server is reported as a successful no-op so routine wake-ups do not have to
// treat a gone session as an error. That makes "err == nil" useless as
// evidence, and anything built on it (worker.NudgeResult.Delivered, and above
// it the inbound delivery receipt the Slack adapter commits an irreversible
// dedup claim on) would confidently report a message as delivered to a session
// that does not exist. NudgeDelivered must separate the two.
func TestNudgeDeliveredReportsFalseWhenSessionIsGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"session not found", ErrSessionNotFound},
		{"no tmux server", ErrNoServer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakeExecutor{fn: func([]string) (string, error) { return "", tc.err }}
			p := NewProviderWithConfig(DefaultConfig())
			p.tm.exec = fe
			p.tm.cfg.NudgeIdleTimeout = 0 // no wait-idle probing in a unit test

			delivery, err := p.NudgeDelivered("gt-vanished", runtime.TextContent("ship it"))
			if err != nil {
				t.Fatalf("NudgeDelivered() error = %v, want nil (best-effort semantics preserved)", err)
			}
			if delivery.Delivered {
				t.Fatal("NudgeDelivered() vouched for a payload sent to a session that does not exist — " +
					"a consumer would commit its dedup claim and the message would be lost for good")
			}
			if delivery != (runtime.NudgeDelivery{}) {
				t.Fatalf("NudgeDelivered() = %+v, want the zero delivery: a vanished session has no bytes and no submit verdict", delivery)
			}

			// The lenient error contract the rest of the system depends on must
			// be unchanged: Nudge still reports this as a non-error.
			if err := p.Nudge("gt-vanished", runtime.TextContent("ship it")); err != nil {
				t.Fatalf("Nudge() error = %v, want nil — best-effort semantics must not regress", err)
			}
		})
	}
}

// TestNudgeDeliveredVouchesForARealDelivery is the other half: the honest
// negative above is only useful if the positive case still reports true.
func TestNudgeDeliveredVouchesForARealDelivery(t *testing.T) {
	fe := &fakeExecutor{}
	p := NewProviderWithConfig(DefaultConfig())
	p.tm.exec = fe
	p.tm.cfg.NudgeIdleTimeout = 0
	p.tm.cfg.DebounceMs = 0

	delivery, err := p.NudgeDelivered("gt-live", runtime.TextContent("ship it"))
	if err != nil {
		t.Fatalf("NudgeDelivered() = %v, want nil", err)
	}
	if !delivery.Delivered {
		t.Fatal("NudgeDelivered() did not vouch for a delivery that succeeded")
	}
	if delivery.Bytes != len("ship it") {
		t.Fatalf("NudgeDelivered() Bytes = %d, want %d (the paste's size, as on the receipt)", delivery.Bytes, len("ship it"))
	}
	// No GC_PROVIDER on this fake pane, so no busy probe ran: the submit is
	// unverified, the historical best-effort outcome — not unconfirmed, which
	// would tell a consumer to hold.
	if delivery.Submit != runtime.NudgeSubmitUnverified {
		t.Fatalf("NudgeDelivered() Submit = %q, want %q for a family without a busy probe", delivery.Submit, runtime.NudgeSubmitUnverified)
	}
}

// claudeNeverBusyExecutor stands in for a claude-family pane that accepts the
// bracketed paste and every submit Enter but never shows a busy indicator
// within the confirm budget — the shape of the 2026-08-28 08:24Z incident, in
// which a founder message was pasted whole into the mayor's session and gc
// reported the delivery as failed with 0 bytes because the submit could not be
// confirmed.
//
// Only the GC_PROVIDER lookup is answered; every other tmux call succeeds with
// empty output, so capture-pane never contains a busy marker. (runCtx prefixes
// every invocation with -u and an optional -L <socket>, so the subcommand is
// not args[0].)
func claudeNeverBusyExecutor() *fakeExecutor {
	return &fakeExecutor{fn: func(args []string) (string, error) {
		if slices.Contains(args, "show-environment") && args[len(args)-1] == "GC_PROVIDER" {
			return "GC_PROVIDER=claude", nil
		}
		return "", nil
	}}
}

// TestNudgeDeliveredVouchesForAPasteWhoseSubmitWasNeverConfirmed is the
// regression test for gp-2io. The runtime's own receipt is the delivery claim:
// it is emitted only after the payload was framed and handed to the terminal,
// and it carries the byte count that was pasted. NudgeDelivered must not
// contradict that receipt. An unconfirmed submit means "the agent has not been
// seen taking the turn yet" — it is NOT "nothing was delivered", and reporting
// it as a failure is what drove a consumer's clean-retry path six times over
// for one message that had landed every time.
func TestNudgeDeliveredVouchesForAPasteWhoseSubmitWasNeverConfirmed(t *testing.T) {
	const message = "<system-reminder>\nNew message in shared conversation slack/C0BEZ3CQK5X:\n\n- Taylor (human): are we still on for 3?\n</system-reminder>"

	p := NewProviderWithConfig(DefaultConfig())
	p.tm.exec = claudeNeverBusyExecutor()
	p.tm.cfg.NudgeIdleTimeout = 0
	p.tm.cfg.DebounceMs = 0
	receipts := collectReceipts(p.tm)

	delivery, err := p.NudgeDelivered("gt-busy-mayor", runtime.TextContent(message))

	// The fixture must reproduce the incident shape before the assertion means
	// anything: a receipt exists (the paste landed), it carries the whole
	// payload, and the submit was not confirmed.
	got := receipts()
	if len(got) != 1 {
		t.Fatalf("fixture: emitted %d receipts, want exactly 1 (the paste must have landed): %+v", len(got), got)
	}
	if got[0].Bytes != len(message) {
		t.Fatalf("fixture: receipt Bytes = %d, want %d", got[0].Bytes, len(message))
	}
	if got[0].Submitted {
		t.Fatal("fixture: submit was confirmed, but this test is about the never-busy pane")
	}

	if err != nil {
		t.Fatalf("NudgeDelivered() error = %v — the runtime's own receipt (%s) says the payload landed; "+
			"an unconfirmed submit is 'not yet', not a delivery failure", err, got[0])
	}
	if !delivery.Delivered {
		t.Fatalf("NudgeDelivered() = %+v, want Delivered for a payload its own receipt vouches for", delivery)
	}
	if delivery.Bytes != len(message) {
		t.Fatalf("NudgeDelivered() Bytes = %d, want %d (what was actually pasted)", delivery.Bytes, len(message))
	}
	if delivery.Submit != runtime.NudgeSubmitUnconfirmed {
		t.Fatalf("NudgeDelivered() Submit = %q, want %q: the pane never went busy, so the submit is unconfirmed, not confirmed",
			delivery.Submit, runtime.NudgeSubmitUnconfirmed)
	}
}

// TestNudgeKeepsUnconfirmedSubmitErrorForRetryingCallers pins the other half
// of the contract. Nudge and NudgeNow return no evidence, only an error, and
// the nudge queue relies on ErrNudgeSubmitUnconfirmed to leave an item unacked
// so it requeues (ga-bwm). Giving the vouching caller an honest answer must not
// take that signal away from the retrying ones.
func TestNudgeKeepsUnconfirmedSubmitErrorForRetryingCallers(t *testing.T) {
	p := NewProviderWithConfig(DefaultConfig())
	p.tm.exec = claudeNeverBusyExecutor()
	p.tm.cfg.NudgeIdleTimeout = 0
	p.tm.cfg.DebounceMs = 0

	for name, send := range map[string]func() error{
		"Nudge":    func() error { return p.Nudge("gt-busy-mayor", runtime.TextContent("hello")) },
		"NudgeNow": func() error { return p.NudgeNow("gt-busy-mayor", runtime.TextContent("hello")) },
	} {
		if err := send(); !errors.Is(err, ErrNudgeSubmitUnconfirmed) {
			t.Fatalf("%s() error = %v, want ErrNudgeSubmitUnconfirmed so a queued nudge stays unacked", name, err)
		}
	}
}

// claudeEnterRefusedExecutor stands in for a claude-family pane whose
// bracketed paste lands but where tmux refuses every submit Enter afterwards
// (a client that detached between the paste and the submit does this). The
// payload is in the pane; only the submit is unaccounted for.
func claudeEnterRefusedExecutor() *fakeExecutor {
	return &fakeExecutor{fn: func(args []string) (string, error) {
		if slices.Contains(args, "show-environment") && args[len(args)-1] == "GC_PROVIDER" {
			return "GC_PROVIDER=claude", nil
		}
		if slices.Contains(args, "send-keys") && args[len(args)-1] == "Enter" {
			return "", errors.New("no current client")
		}
		return "", nil
	}}
}

// TestNudgeDeliveredVouchesForAPasteWhoseSubmitEnterWasRefused closes the
// second gp-2io gap: not just an unobserved busy state, but a submit key that
// tmux refused outright AFTER the paste landed. The old shape returned zero
// evidence with an error, which classified as failed/0 bytes and licensed the
// consumer's clean re-post of a payload already sitting in the composer.
func TestNudgeDeliveredVouchesForAPasteWhoseSubmitEnterWasRefused(t *testing.T) {
	const message = "post-paste enter refusal"

	p := NewProviderWithConfig(DefaultConfig())
	p.tm.exec = claudeEnterRefusedExecutor()
	p.tm.cfg.NudgeIdleTimeout = 0
	p.tm.cfg.DebounceMs = 0
	receipts := collectReceipts(p.tm)

	delivery, err := p.NudgeDelivered("gt-enter-refused", runtime.TextContent(message))

	got := receipts()
	if len(got) != 1 || got[0].Bytes != len(message) {
		t.Fatalf("fixture: receipts = %+v, want exactly 1 carrying %d bytes (the paste must have landed)", got, len(message))
	}
	if err != nil {
		t.Fatalf("NudgeDelivered() error = %v — the paste landed (its receipt says so); a refused submit is evidence, not a delivery failure", err)
	}
	if !delivery.Delivered || delivery.Bytes != len(message) {
		t.Fatalf("NudgeDelivered() = %+v, want Delivered with Bytes=%d", delivery, len(message))
	}
	if delivery.Submit != runtime.NudgeSubmitUnconfirmed {
		t.Fatalf("NudgeDelivered() Submit = %q, want %q: the submit never reached tmux, so it is unconfirmed", delivery.Submit, runtime.NudgeSubmitUnconfirmed)
	}

	// The retrying callers keep their signal for the same shape.
	if err := p.tm.NudgeSession("gt-enter-refused", message); !errors.Is(err, ErrNudgeSubmitUnconfirmed) {
		t.Fatalf("NudgeSession() error = %v, want ErrNudgeSubmitUnconfirmed so a queued nudge stays unacked", err)
	}
}

// TestNudgeDeliveredReportsUnconfirmedWhenBestEffortSubmitNeverReachesTmux
// covers the same post-paste gap on the best-effort arm (a provider family
// with no busy probe): the paste lands, every submit attempt is refused, and
// the evidence must still say the payload is in the pane.
func TestNudgeDeliveredReportsUnconfirmedWhenBestEffortSubmitNeverReachesTmux(t *testing.T) {
	const message = "best-effort enter refusal"

	p := NewProviderWithConfig(DefaultConfig())
	p.tm.exec = &fakeExecutor{fn: func(args []string) (string, error) {
		if slices.Contains(args, "send-keys") && args[len(args)-1] == "Enter" {
			return "", errors.New("no current client")
		}
		return "", nil
	}}
	p.tm.cfg.NudgeIdleTimeout = 0
	p.tm.cfg.DebounceMs = 0
	receipts := collectReceipts(p.tm)

	delivery, err := p.NudgeDelivered("gt-best-effort-refused", runtime.TextContent(message))
	if err != nil {
		t.Fatalf("NudgeDelivered() error = %v, want evidence for a landed paste", err)
	}
	if got := receipts(); len(got) != 1 || got[0].Bytes != len(message) {
		t.Fatalf("fixture: receipts = %+v, want exactly 1 carrying %d bytes", got, len(message))
	}
	if !delivery.Delivered || delivery.Bytes != len(message) || delivery.Submit != runtime.NudgeSubmitUnconfirmed {
		t.Fatalf("NudgeDelivered() = %+v, want Delivered/Bytes=%d/unconfirmed", delivery, len(message))
	}
}

// enterRefusedWriteCloser accepts the first write (the framed payload) and
// refuses everything after it (the submit '\r').
type enterRefusedWriteCloser struct {
	writes int
}

func (w *enterRefusedWriteCloser) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, errors.New("hidden attach client stdin closed")
	}
	return len(p), nil
}

func (w *enterRefusedWriteCloser) Close() error { return nil }

// TestSendHiddenAttachedTextReportsUnconfirmedWhenEnterFails is the
// hidden-attach twin of the refused-Enter case: the framed payload was
// written into the client's stdin, so it is in the composer whatever happens
// to the trailing '\r'. Reporting an error here would let a consumer classify
// the member as failed and re-post a payload that is already drafted.
func TestSendHiddenAttachedTextReportsUnconfirmedWhenEnterFails(t *testing.T) {
	const message = "hidden payload, enter refused"

	fe := &fakeExecutor{out: strconv.FormatInt(time.Now().Unix(), 10)}
	tm := NewTmux()
	tm.exec = fe
	tm.cfg.DebounceMs = 0
	receipts := collectReceipts(tm)

	const sess = "hidden-attach-enter-refused"
	tm.hiddenAttachMu.Lock()
	tm.hiddenAttachClients = map[string]*hiddenAttachClient{sess: {stdin: &enterRefusedWriteCloser{}}}
	tm.hiddenAttachMu.Unlock()

	used, delivery, err := tm.sendHiddenAttachedText(sess, message)
	if !used {
		t.Fatal("sendHiddenAttachedText: used = false, want the hidden-attach transport taken")
	}
	if err != nil {
		t.Fatalf("sendHiddenAttachedText: err = %v — the payload write succeeded, so the failed Enter is evidence, not an error", err)
	}
	if !delivery.Delivered || delivery.Bytes != len(message) {
		t.Fatalf("delivery = %+v, want Delivered with Bytes=%d", delivery, len(message))
	}
	if delivery.Submit != runtime.NudgeSubmitUnconfirmed {
		t.Fatalf("delivery Submit = %q, want %q for an Enter that never reached the client", delivery.Submit, runtime.NudgeSubmitUnconfirmed)
	}
	if got := receipts(); len(got) != 1 || got[0].Bytes != len(message) {
		t.Fatalf("receipts = %+v, want exactly 1 carrying the whole payload", got)
	}
}

// shortWriteCloser accepts the first accept bytes of the first write and then
// fails, the shape of a hidden-attach client that closed under a long paste.
type shortWriteCloser struct {
	accept int
	writes int
}

func (w *shortWriteCloser) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, errors.New("hidden attach client stdin closed")
	}
	n := min(w.accept, len(p))
	return n, errors.New("write: broken pipe")
}

func (w *shortWriteCloser) Close() error { return nil }

// TestSendHiddenAttachedTextReportsAFragmentThatLandedBeforeTheWriteFailed
// covers the hidden-attach write failing partway: the bytes the client
// accepted before the error are in front of the pane, so the evidence must
// say so (a short, unconfirmed paste) alongside the error — never a zero
// delivery, which would classify as "nothing landed" and re-post the whole
// message on top of the fragment.
func TestSendHiddenAttachedTextReportsAFragmentThatLandedBeforeTheWriteFailed(t *testing.T) {
	const message = "a payload long enough to be cut in half by a closing client"
	const landed = 17

	fe := &fakeExecutor{out: strconv.FormatInt(time.Now().Unix(), 10)}
	tm := NewTmux()
	tm.exec = fe
	tm.cfg.DebounceMs = 0
	receipts := collectReceipts(tm)

	const sess = "hidden-attach-short-write"
	tm.hiddenAttachMu.Lock()
	tm.hiddenAttachClients = map[string]*hiddenAttachClient{sess: {stdin: &shortWriteCloser{accept: len(bracketedPasteStart) + landed}}}
	tm.hiddenAttachMu.Unlock()

	used, delivery, err := tm.sendHiddenAttachedText(sess, message)
	if !used {
		t.Fatal("sendHiddenAttachedText: used = false, want the hidden-attach transport taken")
	}
	if err == nil {
		t.Fatal("sendHiddenAttachedText: err = nil, want the write error passed through for retrying callers")
	}
	if !delivery.Landed() || delivery.Bytes != landed || delivery.Submit != runtime.NudgeSubmitUnconfirmed {
		t.Fatalf("delivery = %+v, want a landed fragment of %d bytes, submit unconfirmed", delivery, landed)
	}
	if got := receipts(); len(got) != 1 || got[0].Bytes != landed {
		t.Fatalf("receipts = %+v, want exactly 1 carrying the %d-byte fragment", got, landed)
	}

	// A write that fails before any of the payload got past the start marker
	// left nothing in the pane: zero evidence, the error alone.
	tm.hiddenAttachMu.Lock()
	tm.hiddenAttachClients = map[string]*hiddenAttachClient{sess: {stdin: &shortWriteCloser{accept: len(bracketedPasteStart)}}}
	tm.hiddenAttachMu.Unlock()
	_, delivery, err = tm.sendHiddenAttachedText(sess, message)
	if err == nil || delivery.Landed() {
		t.Fatalf("marker-only write: delivery = %+v err = %v, want zero evidence with the error", delivery, err)
	}
	if got := receipts(); len(got) != 1 {
		t.Fatalf("receipts = %+v, want no receipt for a paste of which nothing landed", got)
	}
}

// TestNudgeDeliveredKeepsHiddenAttachFragmentAlongsideError pins the
// provider boundary above sendHiddenAttachedText: the fragment evidence must
// reach NudgeDelivered's caller together with the error, and Nudge/NudgeNow
// must still surface the error for retrying callers.
func TestNudgeDeliveredKeepsHiddenAttachFragmentAlongsideError(t *testing.T) {
	const message = "a payload long enough to be cut in half by a closing client"
	const landed = 11

	p := NewProviderWithConfig(DefaultConfig())
	p.tm.exec = &fakeExecutor{out: strconv.FormatInt(time.Now().Unix(), 10)}
	p.tm.cfg.DebounceMs = 0
	const sess = "hidden-attach-fragment-boundary"
	arm := func() {
		p.tm.hiddenAttachMu.Lock()
		p.tm.hiddenAttachClients = map[string]*hiddenAttachClient{sess: {stdin: &shortWriteCloser{accept: len(bracketedPasteStart) + landed}}}
		p.tm.hiddenAttachMu.Unlock()
	}

	arm()
	delivery, err := p.NudgeDelivered(sess, runtime.TextContent(message))
	if err == nil {
		t.Fatal("NudgeDelivered() error = nil, want the write error passed through")
	}
	if !delivery.Landed() || delivery.Bytes != landed || delivery.Submit != runtime.NudgeSubmitUnconfirmed {
		t.Fatalf("NudgeDelivered() = %+v, want the landed %d-byte fragment, unconfirmed, alongside the error", delivery, landed)
	}

	arm()
	if err := p.NudgeNow(sess, runtime.TextContent(message)); err == nil {
		t.Fatal("NudgeNow() error = nil, want the write error so a queued nudge stays unacked")
	}
}

// blockingWriteCloser accepts the first accept bytes of a write and then
// blocks until Close is called, the shape of a hidden-attach client whose
// reader has stopped draining the pipe.
type blockingWriteCloser struct {
	accept  int
	started chan struct{} // closed when the first write is in flight
	closed  chan struct{}
	start   sync.Once
	once    sync.Once
}

func newBlockingWriteCloser(accept int) *blockingWriteCloser {
	return &blockingWriteCloser{accept: accept, started: make(chan struct{}), closed: make(chan struct{})}
}

func (w *blockingWriteCloser) Write(p []byte) (int, error) {
	n := min(w.accept, len(p))
	w.start.Do(func() { close(w.started) })
	<-w.closed
	return n, errors.New("write: file already closed")
}

func (w *blockingWriteCloser) Close() error {
	w.once.Do(func() { close(w.closed) })
	return nil
}

// TestCloseHiddenAttachClientReturnsWhileAWriteIsBlocked pins the
// shutdown side of the fragment evidence: closing the client must not wait
// on the write lock, because the close is the only thing that unblocks a
// write stuck on a reader cancel() did not reach. The interrupted write then
// reports what it got through, and that becomes evidence.
func TestCloseHiddenAttachClientReturnsWhileAWriteIsBlocked(t *testing.T) {
	const message = "a payload that blocks the client halfway through"
	const landed = 9

	fe := &fakeExecutor{out: strconv.FormatInt(time.Now().Unix(), 10)}
	tm := NewTmux()
	tm.exec = fe
	tm.cfg.DebounceMs = 0
	receipts := collectReceipts(tm)

	const sess = "hidden-attach-blocked-write"
	stdin := newBlockingWriteCloser(len(bracketedPasteStart) + landed)
	tm.hiddenAttachMu.Lock()
	tm.hiddenAttachClients = map[string]*hiddenAttachClient{sess: {stdin: stdin, cancel: func() {}}}
	tm.hiddenAttachMu.Unlock()

	type outcome struct {
		delivery runtime.NudgeDelivery
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		_, delivery, err := tm.sendHiddenAttachedText(sess, message)
		done <- outcome{delivery, err}
	}()
	// Only close once the paste is actually in flight; closing before the
	// send looked the client up would test nothing but the map.
	select {
	case <-stdin.started:
	case <-time.After(3 * time.Second):
		t.Fatal("the paste never reached the client's stdin")
	}

	closed := make(chan struct{})
	go func() {
		tm.CloseHiddenAttachClient(sess)
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("CloseHiddenAttachClient did not return while a write was blocked: it must never wait on the write lock")
	}

	var got outcome
	select {
	case got = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the blocked write did not return after the close")
	}
	if got.err == nil {
		t.Fatal("sendHiddenAttachedText: err = nil, want the interrupted write's error")
	}
	if !got.delivery.Landed() || got.delivery.Bytes != landed || got.delivery.Submit != runtime.NudgeSubmitUnconfirmed {
		t.Fatalf("delivery = %+v, want the %d-byte fragment the client accepted before the close, unconfirmed", got.delivery, landed)
	}
	if r := receipts(); len(r) != 1 || r[0].Bytes != landed {
		t.Fatalf("receipts = %+v, want exactly 1 carrying the fragment", r)
	}
}

var _ runtime.ImmediateNudgeVouchingProvider = (*Provider)(nil)
