package api

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/runtime"
)

// notifyDeliveryFixture wires a conversation with two session members, which
// is the shape that makes the receipt non-trivial: the reminder text differs
// per member, so the byte accounting has to be per-member and summed.
func notifyDeliveryFixture(t *testing.T) (*fakeState, *Server, extmsg.ConversationRef) {
	t.Helper()
	fs := newSessionFakeState(t)
	srv := New(fs)

	services := extmsg.NewServices(fs.cityBeadStore)
	fs.extmsgSvc = &services

	ref := extmsg.ConversationRef{
		ScopeID:        "guild-1",
		Provider:       "slack",
		AccountID:      "acct-1",
		ConversationID: "C0ASAPRETDK",
		Kind:           extmsg.ConversationRoom,
	}
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	for _, title := range []string{"Mayor", "Worker"} {
		info := createTestSession(t, fs.cityBeadStore, fs.sp, title)
		if _, err := services.Transcript.EnsureMembership(context.Background(), extmsg.EnsureMembershipInput{
			Caller:         caller,
			Conversation:   ref,
			SessionID:      info.ID,
			BackfillPolicy: extmsg.MembershipBackfillSinceJoin,
			Owner:          extmsg.MembershipOwnerManual,
			Now:            time.Now().UTC(),
		}); err != nil {
			t.Fatalf("EnsureMembership(%s): %v", title, err)
		}
	}
	return fs, srv, ref
}

// TestExtmsgNotifyMembersReceiptMatchesWhatWasActuallySent is the core
// guarantee behind the inbound delivery receipt: the numbers gc reports
// describe the payload that really went to the terminal, not a hopeful
// summary. It checks the receipt against the nudges the runtime actually
// recorded, so a future change that reports success without sending (or sends
// something other than what it counted) fails here.
func TestExtmsgNotifyMembersReceiptMatchesWhatWasActuallySent(t *testing.T) {
	fs, srv, ref := notifyDeliveryFixture(t)

	outcomes, notifyErr := srv.extmsgNotifyMembers(context.Background(), extmsgNotifyBroadcast{
		Conversation: ref,
		ActorDisplay: "Afik",
		ActorKind:    "human",
		Text:         "ship it",
	})
	if notifyErr != nil {
		t.Fatalf("extmsgNotifyMembers: %v", notifyErr)
	}
	if len(outcomes) != 2 {
		t.Fatalf("got %d member outcomes, want 2: %+v", len(outcomes), outcomes)
	}

	// What the runtime was actually handed, indexed by the same content address
	// the receipt carries. Two members can legitimately receive byte-identical
	// reminders (they differ only when a handle or an addressed-to
	// discriminator applies), so this maps a digest to every payload sharing
	// it rather than assuming uniqueness.
	sentByDigest := map[string][]string{}
	sentCount := 0
	for _, call := range fs.sp.Calls {
		if call.Method == "Nudge" {
			digest := runtime.NudgePayloadDigest(call.Message)
			sentByDigest[digest] = append(sentByDigest[digest], call.Message)
			sentCount++
		}
	}
	if sentCount != 2 {
		t.Fatalf("runtime received %d nudges, want one per member (2)", sentCount)
	}

	for _, m := range outcomes {
		if m.Status != extmsg.InboundDeliveryDelivered {
			t.Fatalf("member %s status = %q, want delivered: %+v", m.SessionID, m.Status, m)
		}
		matches, ok := sentByDigest[m.Digest]
		if !ok {
			t.Fatalf("receipt digest %q for %s names no nudge the runtime actually received — "+
				"the receipt is describing a payload that was never sent", m.Digest, m.SessionID)
		}
		sent := matches[0]
		if m.ExpectedBytes != len(sent) {
			t.Fatalf("member %s expected_bytes = %d, want %d (len of the reminder gc built)", m.SessionID, m.ExpectedBytes, len(sent))
		}
		if m.DeliveredBytes != m.ExpectedBytes {
			t.Fatalf("member %s delivered %d of %d bytes on a successful send", m.SessionID, m.DeliveredBytes, m.ExpectedBytes)
		}
	}

	got := extmsg.SummarizeInboundDelivery("ir-test", outcomes)
	if got.Status != extmsg.InboundDeliveryDelivered {
		t.Fatalf("aggregate status = %q, want delivered", got.Status)
	}
	if got.DeliveredBytes != got.ExpectedBytes || got.ExpectedBytes == 0 {
		t.Fatalf("aggregate bytes = %d/%d, want equal and non-zero", got.DeliveredBytes, got.ExpectedBytes)
	}
}

// TestExtmsgNotifyMembersReceiptCannotClaimDeliveryWhenRuntimeFails is the
// property that makes the receipt worth gating on. A consumer commits an
// irreversible dedup claim on "delivered"; if a dead runtime could still
// produce that word, the claim would be committed for a message no agent ever
// saw — which is the 2026-08-27 failure with extra steps.
func TestExtmsgNotifyMembersReceiptCannotClaimDeliveryWhenRuntimeFails(t *testing.T) {
	fs, srv, ref := notifyDeliveryFixture(t)

	// Sessions exist and are members; the runtime dies before the fan-out.
	fs.sessionProvider = runtime.NewFailFake()

	outcomes, notifyErr := srv.extmsgNotifyMembers(context.Background(), extmsgNotifyBroadcast{
		Conversation: ref,
		ActorDisplay: "Afik",
		ActorKind:    "human",
		Text:         "ship it",
	})
	if notifyErr != nil {
		t.Fatalf("extmsgNotifyMembers: %v", notifyErr)
	}
	if len(outcomes) == 0 {
		t.Fatal("a dead runtime produced no member outcomes at all — an empty fan-out summarizes " +
			"as no_route, which would tell a consumer to COMMIT its dedup claim")
	}
	for _, m := range outcomes {
		if m.Status == extmsg.InboundDeliveryDelivered {
			t.Fatalf("member %s reported delivered against a failing runtime: %+v", m.SessionID, m)
		}
		if m.DeliveredBytes != 0 {
			t.Fatalf("member %s reported %d delivered bytes with nothing sent", m.SessionID, m.DeliveredBytes)
		}
		if m.Error == "" {
			t.Fatalf("member %s failed with no error context: %+v", m.SessionID, m)
		}
	}

	got := extmsg.SummarizeInboundDelivery("ir-test", outcomes)
	if got.Status != extmsg.InboundDeliveryFailed {
		t.Fatalf("aggregate status = %q, want failed", got.Status)
	}
	if got.DeliveredBytes >= got.ExpectedBytes {
		t.Fatalf("delivered_bytes %d >= expected_bytes %d on a total failure — this comparison is "+
			"exactly what the Slack adapter gates on", got.DeliveredBytes, got.ExpectedBytes)
	}
}

// TestExtmsgNotifyMembersReportsUnresolvableMemberAsUndelivered covers the
// quieter half of the same risk. A transcript member whose session cannot be
// resolved was, from the sender's side, simply not reached — but it is easy to
// treat it as "not a target" and drop it, at which point a fan-out that
// reached nobody summarizes as fully delivered.
func TestExtmsgNotifyMembersReportsUnresolvableMemberAsUndelivered(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	services := extmsg.NewServices(fs.cityBeadStore)
	fs.extmsgSvc = &services

	ref := extmsg.ConversationRef{
		ScopeID:        "guild-1",
		Provider:       "slack",
		AccountID:      "acct-1",
		ConversationID: "C0GHOST",
		Kind:           extmsg.ConversationRoom,
	}
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	if _, err := services.Transcript.EnsureMembership(context.Background(), extmsg.EnsureMembershipInput{
		Caller:         caller,
		Conversation:   ref,
		SessionID:      "gt-does-not-exist",
		BackfillPolicy: extmsg.MembershipBackfillSinceJoin,
		Owner:          extmsg.MembershipOwnerManual,
		Now:            time.Now().UTC(),
	}); err != nil {
		t.Fatalf("EnsureMembership: %v", err)
	}

	outcomes, notifyErr := srv.extmsgNotifyMembers(context.Background(), extmsgNotifyBroadcast{
		Conversation: ref,
		ActorDisplay: "Afik",
		ActorKind:    "human",
		Text:         "anyone home?",
	})
	if notifyErr != nil {
		t.Fatalf("extmsgNotifyMembers: %v", notifyErr)
	}
	if len(outcomes) != 1 {
		t.Fatalf("got %d outcomes, want 1 for the unresolvable member: %+v", len(outcomes), outcomes)
	}
	if outcomes[0].Status == extmsg.InboundDeliveryDelivered {
		t.Fatalf("unresolvable member reported delivered: %+v", outcomes[0])
	}
	if extmsg.SummarizeInboundDelivery("ir-test", outcomes).Status != extmsg.InboundDeliveryFailed {
		t.Fatal("a fan-out that reached nobody must not summarize as delivered or no_route")
	}
}

// vouchingFakeProvider is a runtime that implements
// [runtime.NudgeVouchingProvider] and reports a nudge as NOT delivered while
// returning no error — the shape a real runtime takes when the session has
// vanished underneath it (best-effort wake semantics turn that into a
// successful no-op).
type vouchingFakeProvider struct {
	*runtime.Fake
	delivered bool
}

func (p *vouchingFakeProvider) NudgeDelivered(name string, content []runtime.ContentBlock) (bool, error) {
	if err := p.Fake.Nudge(name, content); err != nil {
		return false, err
	}
	return p.delivered, nil
}

// TestExtmsgNotifyMembersReceiptHonorsRuntimeThatDeclinesToVouch is the
// end-to-end guard for the failure mode that is easiest to ship by accident:
// the runtime returns no error, so every layer above reports success, and the
// receipt confidently vouches for a message that reached no terminal.
//
// A consumer commits an irreversible dedup claim on "delivered", so a receipt
// that trusts a nil error would make the message unrecoverable — the exact
// class of loss this receipt was built to prevent.
func TestExtmsgNotifyMembersReceiptHonorsRuntimeThatDeclinesToVouch(t *testing.T) {
	fs, srv, ref := notifyDeliveryFixture(t)
	fs.sessionProvider = &vouchingFakeProvider{Fake: fs.sp, delivered: false}

	outcomes, notifyErr := srv.extmsgNotifyMembers(context.Background(), extmsgNotifyBroadcast{
		Conversation: ref,
		ActorDisplay: "Afik",
		ActorKind:    "human",
		Text:         "ship it",
	})
	if notifyErr != nil {
		t.Fatalf("extmsgNotifyMembers: %v", notifyErr)
	}
	if len(outcomes) == 0 {
		t.Fatal("no member outcomes; an empty fan-out summarizes as no_route and would tell the consumer to COMMIT")
	}
	for _, m := range outcomes {
		if m.Status == extmsg.InboundDeliveryDelivered {
			t.Fatalf("member %s reported delivered although the runtime declined to vouch: %+v", m.SessionID, m)
		}
		if m.DeliveredBytes != 0 {
			t.Fatalf("member %s reported %d delivered bytes for an unvouched send", m.SessionID, m.DeliveredBytes)
		}
	}
	if got := extmsg.SummarizeInboundDelivery("ir-test", outcomes); got.Status != extmsg.InboundDeliveryFailed {
		t.Fatalf("aggregate status = %q, want failed", got.Status)
	}
}
