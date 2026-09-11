package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func (p *vouchingFakeProvider) NudgeDelivered(name string, content []runtime.ContentBlock) (runtime.NudgeDelivery, error) {
	if err := p.Nudge(name, content); err != nil {
		return runtime.NudgeDelivery{}, err
	}
	if !p.delivered {
		return runtime.NudgeDelivery{}, nil
	}
	return runtime.NudgeDelivery{
		Delivered: true,
		Bytes:     len(runtime.FlattenText(content)),
		Submit:    runtime.NudgeSubmitConfirmed,
	}, nil
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

// unconfirmedSubmitFakeProvider is a runtime that reproduces the 2026-08-28
// 08:24Z incident at the vouching boundary: the payload was pasted whole into
// a session that never showed a busy indicator, so the submit could not be
// confirmed. It records the nudge like the Fake so the test can see what was
// handed to the runtime.
type unconfirmedSubmitFakeProvider struct {
	*runtime.Fake
}

func (p *unconfirmedSubmitFakeProvider) NudgeDelivered(name string, content []runtime.ContentBlock) (runtime.NudgeDelivery, error) {
	if err := p.Nudge(name, content); err != nil {
		return runtime.NudgeDelivery{}, err
	}
	return runtime.NudgeDelivery{
		Delivered: true,
		Bytes:     len(runtime.FlattenText(content)),
		Submit:    runtime.NudgeSubmitUnconfirmed,
	}, nil
}

// TestExtmsgNotifyMembersReportsUnconfirmedSubmitAsPendingWithFullBytes is
// the gp-2io regression test. On 2026-08-28 08:24Z one founder message reached
// the mayor six times in ~90s: gc reported the delivery as failed with
// delivered_bytes=0 because the submit Enter could not be confirmed, and the
// Slack adapter did exactly what "failed" licenses — a clean re-post. The
// payload had landed every time.
//
// A paste that reached the terminal but whose submit was not observed is
// "not yet", which is what pending means, and the byte count must say what
// was actually pasted. "failed" is a promise that a retry is clean; here it
// is not.
func TestExtmsgNotifyMembersReportsUnconfirmedSubmitAsPendingWithFullBytes(t *testing.T) {
	fs, srv, ref := notifyDeliveryFixture(t)
	fs.sessionProvider = &unconfirmedSubmitFakeProvider{Fake: fs.sp}

	outcomes, notifyErr := srv.extmsgNotifyMembers(context.Background(), extmsgNotifyBroadcast{
		Conversation:      ref,
		ActorDisplay:      "Taylor",
		ActorKind:         "human",
		Text:              "are we still on for 3?",
		ProviderMessageID: "1787905440.672959",
	})
	if notifyErr != nil {
		t.Fatalf("extmsgNotifyMembers: %v", notifyErr)
	}
	if len(outcomes) != 2 {
		t.Fatalf("got %d member outcomes, want 2: %+v", len(outcomes), outcomes)
	}
	for _, m := range outcomes {
		if m.Status != extmsg.InboundDeliveryPending {
			t.Fatalf("member %s status = %q, want pending: an unconfirmed submit on a pasted payload is "+
				"'not yet', and 'failed' would license the consumer's clean re-post (the 6x duplicate storm): %+v",
				m.SessionID, m.Status, m)
		}
		if m.ExpectedBytes == 0 {
			t.Fatalf("member %s expected_bytes = 0: %+v", m.SessionID, m)
		}
		if m.DeliveredBytes != m.ExpectedBytes {
			t.Fatalf("member %s delivered_bytes = %d/%d: the whole payload was pasted, so the count must say so",
				m.SessionID, m.DeliveredBytes, m.ExpectedBytes)
		}
		if m.Error == "" {
			t.Fatalf("member %s pending with no reason: %+v", m.SessionID, m)
		}
	}

	got := extmsg.SummarizeInboundDelivery("ir-test", outcomes)
	if got.Status != extmsg.InboundDeliveryPending {
		t.Fatalf("aggregate status = %q, want pending", got.Status)
	}
	if got.DeliveredBytes != got.ExpectedBytes || got.ExpectedBytes == 0 {
		t.Fatalf("aggregate bytes = %d/%d, want equal and non-zero", got.DeliveredBytes, got.ExpectedBytes)
	}
}

// TestHandleExtMsgInboundRedeliveryRetriesAfterFailedNotify pins the retry
// contract across an adapter redelivery. The transcript row is written BEFORE
// the member fan-out, so when the fan-out fails the row already exists and
// the next delivery of the same provider message id is a transcript
// duplicate. gc answered "failed" — the status that tells the adapter a
// retry is clean — so the adapter redelivers. Suppressing the fan-out on the
// transcript dedup alone (#15 as merged) answered that retry with no_route,
// which is terminal: the adapter stopped, and no member ever held the
// message. The redelivery after the runtime recovers must notify and report
// delivered; only a redelivery after THAT is the hq-703om duplicate turn and
// gets suppressed.
func TestHandleExtMsgInboundRedeliveryRetriesAfterFailedNotify(t *testing.T) {
	fs, srv, services, ref := newExtMsgAgentBindingFixture(t)
	source := createTestSession(t, fs.cityBeadStore, fs.sp, "Mayor")
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	if _, err := services.Bindings.Bind(context.Background(), caller, extmsg.BindInput{
		Conversation: ref,
		SessionID:    source.ID,
		Now:          time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	body := map[string]any{
		"message": map[string]any{
			"provider_message_id": "1785354286.000100",
			"conversation":        conversationBody(ref),
			"actor":               map[string]any{"id": "user-1", "display_name": "Afik", "is_bot": false},
			"text":                "ship it",
			"received_at":         time.Now().UTC().Format(time.RFC3339),
		},
	}
	post := func(step string) extmsg.InboundResult {
		t.Helper()
		rec := postExtMsg(t, fs, srv, "/extmsg/inbound", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200; body: %s", step, rec.Code, rec.Body.String())
		}
		var result extmsg.InboundResult
		if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
			t.Fatalf("%s: decode: %v", step, err)
		}
		if result.Delivery == nil {
			t.Fatalf("%s: response carries no delivery receipt", step)
		}
		return result
	}
	nudges := func() int {
		n := 0
		for _, call := range fs.sp.Calls {
			if call.Method == "Nudge" {
				n++
			}
		}
		return n
	}

	// 1. First delivery: the runtime is dead, so the fan-out fails AFTER the
	// transcript row is written. gc tells the adapter a retry is clean.
	fs.sessionProvider = runtime.NewFailFake()
	first := post("first delivery")
	if first.Duplicate {
		t.Fatal("first delivery marked Duplicate")
	}
	if first.Delivery.Status != extmsg.InboundDeliveryFailed {
		t.Fatalf("first delivery status = %q, want failed against a dead runtime: %+v", first.Delivery.Status, first.Delivery)
	}
	if n := nudges(); n != 0 {
		t.Fatalf("live runtime received %d nudges during the failed delivery", n)
	}

	// 2. The runtime recovers and the adapter redelivers the same message id.
	// It is a transcript duplicate, but no member ever received it: the
	// fan-out must run again and land.
	fs.sessionProvider = nil
	second := post("redelivery after failure")
	if !second.Duplicate {
		t.Fatal("redelivery not marked Duplicate — the transcript dedup did not fire, so this test is not exercising the suppression path")
	}
	if second.Delivery.Status != extmsg.InboundDeliveryDelivered {
		t.Fatalf("redelivery after a failed fan-out: status = %q, want delivered — the retry gc invited was suppressed: %+v",
			second.Delivery.Status, second.Delivery)
	}
	if second.Delivery.DeliveredBytes == 0 || second.Delivery.DeliveredBytes != second.Delivery.ExpectedBytes {
		t.Fatalf("redelivery bytes = %d/%d, want a whole non-empty copy", second.Delivery.DeliveredBytes, second.Delivery.ExpectedBytes)
	}
	if n := nudges(); n != 1 {
		t.Fatalf("runtime received %d nudges after the redelivery, want exactly 1", n)
	}

	// 3. A further redelivery of a message the member now holds is the
	// hq-703om case: suppressed, answered no_route (commit the claim), and
	// no extra turn reaches the runtime.
	third := post("redelivery after delivery")
	if !third.Duplicate {
		t.Fatal("second redelivery not marked Duplicate")
	}
	if third.Delivery.Status != extmsg.InboundDeliveryNoRoute {
		t.Fatalf("redelivery of a delivered message: status = %q, want no_route (suppressed): %+v", third.Delivery.Status, third.Delivery)
	}
	if n := nudges(); n != 1 {
		t.Fatalf("redelivery of a delivered message re-nudged the runtime (%d nudges, want 1)", n)
	}
	if st := srv.inboundReceiptStore().Lookup(fs.CityName(), third.Delivery.ReceiptID); st.State != extmsg.InboundReceiptConcluded || st.Delivery == nil || st.Delivery.Status != extmsg.InboundDeliveryNoRoute {
		t.Fatalf("suppression receipt in store = %+v, want the same no_route conclusion the response carried", st)
	}
}

// TestExtmsgClaimInboundFanoutFollowsFirstDeliveryEvidence is the decision
// table behind the test above: whether the fan-out is skipped depends only on
// what the message's latest fan-out receipt says, never on the transcript
// row. Held arms answer with a receipt the adapter can act on; claimed arms
// hand the handler a receipt id the store already holds as the message's
// pending fan-out — so the claim itself is what the NEXT request of the same
// message is judged against.
func TestExtmsgClaimInboundFanoutFollowsFirstDeliveryEvidence(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	store := extmsg.NewInboundReceiptStore()
	srv.inboundReceipts = store
	city := fs.CityName()
	ref := extmsg.ConversationRef{ScopeID: "guild-1", Provider: "slack", AccountID: "acct-1", ConversationID: "C1", Kind: extmsg.ConversationRoom}
	msg := extmsg.ExternalInboundMessage{Conversation: ref, ProviderMessageID: "1785354286.000100", Text: "ship it"}
	key := extmsg.InboundMessageKey(ref, msg.ProviderMessageID)
	dup := &extmsg.InboundResult{Duplicate: true}
	member := func(st extmsg.InboundDeliveryStatus, delivered int) []extmsg.InboundDeliveryMember {
		return []extmsg.InboundDeliveryMember{{SessionID: "s1", Status: st, DeliveredBytes: delivered, ExpectedBytes: 10}}
	}

	// No evidence in this process: claimed, and the claim is registered as
	// the message's pending fan-out in the same step.
	first, _, held := srv.extmsgClaimInboundFanout(&extmsg.InboundResult{}, msg)
	if held || first == "" {
		t.Fatalf("a first delivery with no evidence: held=%v receipt=%q, want claimed with a receipt id", held, first)
	}
	if got := store.LatestFor(city, key); got.State != extmsg.InboundReceiptPending || got.ReceiptID != first {
		t.Fatalf("after the claim the store reports %+v, want pending on %s — the claim must register the fan-out atomically", got, first)
	}
	// A duplicate arriving while that claim runs holds on it, whatever the
	// transcript said about either request.
	for _, result := range []*extmsg.InboundResult{dup, {}, nil} {
		_, got, held := srv.extmsgClaimInboundFanout(result, msg)
		if !held || got.Status != extmsg.InboundDeliveryPending || got.ReceiptID != first {
			t.Fatalf("second request (result=%+v) while the first claim runs: held=%v receipt=%+v, want held pending on %s", result, held, got, first)
		}
	}
	store.Conclude(city, first, extmsg.FailedInboundDelivery(first, "runtime dead"))

	// A message without a provider id is never held: at-least-once.
	anon := msg
	anon.ProviderMessageID = ""
	for i := 0; i < 2; i++ {
		if id, _, held := srv.extmsgClaimInboundFanout(dup, anon); held || id == "" {
			t.Fatalf("message without a provider id (attempt %d): held=%v receipt=%q, want claimed", i, held, id)
		}
	}

	for _, tc := range []struct {
		name  string
		prior func(id string) extmsg.InboundDelivery
		held  bool
		want  extmsg.InboundDeliveryStatus
	}{
		{"failed", func(id string) extmsg.InboundDelivery { return extmsg.FailedInboundDelivery(id, "runtime dead") }, false, ""},
		{"partial", func(id string) extmsg.InboundDelivery {
			return extmsg.SummarizeInboundDelivery(id, member(extmsg.InboundDeliveryPartial, 4))
		}, false, ""},
		{"no_route", func(id string) extmsg.InboundDelivery { return extmsg.SummarizeInboundDelivery(id, nil) }, false, ""},
		{"concluded pending", func(id string) extmsg.InboundDelivery {
			return extmsg.SummarizeInboundDelivery(id, member(extmsg.InboundDeliveryPending, 10))
		}, true, extmsg.InboundDeliveryPending},
		{"delivered", func(id string) extmsg.InboundDelivery {
			return extmsg.SummarizeInboundDelivery(id, member(extmsg.InboundDeliveryDelivered, 10))
		}, true, extmsg.InboundDeliveryNoRoute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := extmsg.NextInboundReceiptID()
			store.BeginFor(city, id, key)
			store.Conclude(city, id, tc.prior(id))
			claimed, got, held := srv.extmsgClaimInboundFanout(dup, msg)
			if held != tc.held {
				t.Fatalf("held = %v, want %v (got %+v)", held, tc.held, got)
			}
			if !held {
				if claimed == "" {
					t.Fatal("not held but no receipt id to run the fan-out under")
				}
				if latest := store.LatestFor(city, key); latest.State != extmsg.InboundReceiptPending || latest.ReceiptID != claimed {
					t.Fatalf("after a claim on %s evidence the store reports %+v, want pending on %s", tc.name, latest, claimed)
				}
				return
			}
			if claimed != "" {
				t.Fatalf("held, but a receipt id %q was handed out as if the fan-out should run", claimed)
			}
			if got.Status != tc.want {
				t.Fatalf("held receipt status = %q, want %q: %+v", got.Status, tc.want, got)
			}
			if tc.want == extmsg.InboundDeliveryPending && got.ReceiptID != id {
				t.Fatalf("held on receipt %q, want the ORIGINAL %q so the adapter polls the fan-out carrying the message", got.ReceiptID, id)
			}
			if tc.want == extmsg.InboundDeliveryNoRoute {
				if st := store.Lookup(city, got.ReceiptID); st.State != extmsg.InboundReceiptConcluded || st.Delivery == nil || st.Delivery.Status != extmsg.InboundDeliveryNoRoute {
					t.Fatalf("suppression receipt %s in store = %+v, want the no_route conclusion the response carried", got.ReceiptID, st)
				}
				if latest := store.LatestFor(city, key); latest.ReceiptID != id {
					t.Fatalf("suppression receipt displaced the delivered fan-out as the message's evidence: %+v", latest)
				}
			}
		})
	}

	// Still running: held on the original receipt, so the adapter's poll
	// lands on the fan-out that is actually carrying the message.
	id := extmsg.NextInboundReceiptID()
	store.BeginFor(city, id, key)
	_, got, held := srv.extmsgClaimInboundFanout(dup, msg)
	if !held || got.Status != extmsg.InboundDeliveryPending || got.ReceiptID != id {
		t.Fatalf("redelivery while the first fan-out runs: held=%v receipt=%+v, want held pending on %s", held, got, id)
	}
}

// gatedTranscript freezes the FIRST inbound Append for providerMessageID
// after its row is written and before it returns to the handler — the exact
// point between "transcript has the message" and "the fan-out is claimed"
// that a concurrent request of the same message can slip through. Every
// other call passes straight to the wrapped service.
type gatedTranscript struct {
	extmsg.TranscriptService
	providerMessageID string
	mu                sync.Mutex
	frozen            bool
	entered           chan struct{} // closed once the gated Append has written its row
	release           chan struct{} // closed by the test to let it return
}

func newGatedTranscript(inner extmsg.TranscriptService, providerMessageID string) *gatedTranscript {
	return &gatedTranscript{
		TranscriptService: inner,
		providerMessageID: providerMessageID,
		entered:           make(chan struct{}),
		release:           make(chan struct{}),
	}
}

func (g *gatedTranscript) Append(ctx context.Context, input extmsg.AppendTranscriptInput) (extmsg.ConversationTranscriptRecord, bool, error) {
	rec, created, err := g.TranscriptService.Append(ctx, input)
	if input.ProviderMessageID != g.providerMessageID {
		return rec, created, err
	}
	g.mu.Lock()
	first := !g.frozen
	g.frozen = true
	g.mu.Unlock()
	if first {
		close(g.entered)
		<-g.release
	}
	return rec, created, err
}

// gatedNudgeProvider is a runtime whose Nudge parks until released, so a
// fan-out can be held mid-flight while another request of the same message
// is judged against it. It records the nudge like the Fake once released.
type gatedNudgeProvider struct {
	*runtime.Fake
	entered chan struct{} // receives once per Nudge that arrived
	release chan struct{}
}

func (p *gatedNudgeProvider) Nudge(name string, content []runtime.ContentBlock) error {
	p.entered <- struct{}{}
	<-p.release
	return p.Fake.Nudge(name, content)
}

// TestHandleExtMsgInboundConcurrentDeliveriesFanOutOnce is the codex r3
// MAJOR 1 regression: receipt lookup and registration used to be separate
// store calls, so a second request of the same message landing between the
// first's transcript append and its BeginFor read "no evidence" and fanned
// out too — and the first, not being a transcript duplicate, fanned out
// regardless. The claim is now one atomic store operation for EVERY request
// carrying a provider message id, first deliveries included, so exactly one
// fan-out runs and the other request is answered from its evidence.
//
// The first request is frozen deterministically in that window (its row is
// written, its claim is not yet made) while the duplicate runs to completion;
// then the frozen request is released and must find the duplicate's claim.
func TestHandleExtMsgInboundConcurrentDeliveriesFanOutOnce(t *testing.T) {
	const providerMessageID = "1785354286.000100"

	type fixture struct {
		fs       *fakeState
		srv      *Server
		gate     *gatedTranscript
		raw      string
		frozen   chan *httptest.ResponseRecorder
		decode   func(step string, rec *httptest.ResponseRecorder) extmsg.InboundResult
		nudges   func() int
		serveDup func() *httptest.ResponseRecorder
	}
	setup := func(t *testing.T) *fixture {
		t.Helper()
		fs, srv, services, ref := newExtMsgAgentBindingFixture(t)
		source := createTestSession(t, fs.cityBeadStore, fs.sp, "Mayor")
		caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
		if _, err := services.Bindings.Bind(context.Background(), caller, extmsg.BindInput{
			Conversation: ref,
			SessionID:    source.ID,
			Now:          time.Now().UTC(),
		}); err != nil {
			t.Fatalf("Bind: %v", err)
		}
		gate := newGatedTranscript(services.Transcript, providerMessageID)
		services.Transcript = gate

		raw, err := json.Marshal(map[string]any{
			"message": map[string]any{
				"provider_message_id": providerMessageID,
				"conversation":        conversationBody(ref),
				"actor":               map[string]any{"id": "user-1", "display_name": "Afik", "is_bot": false},
				"text":                "ship it",
				"received_at":         time.Now().UTC().Format(time.RFC3339),
			},
		})
		if err != nil {
			t.Fatalf("Marshal(body): %v", err)
		}
		f := &fixture{fs: fs, srv: srv, gate: gate, raw: string(raw), frozen: make(chan *httptest.ResponseRecorder, 1)}
		serve := func() *httptest.ResponseRecorder {
			req := newPostRequest(cityURL(fs, "/extmsg/inbound"), strings.NewReader(f.raw))
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			return rec
		}
		f.serveDup = serve
		f.decode = func(step string, rec *httptest.ResponseRecorder) extmsg.InboundResult {
			t.Helper()
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200; body: %s", step, rec.Code, rec.Body.String())
			}
			var result extmsg.InboundResult
			if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
				t.Fatalf("%s: decode: %v", step, err)
			}
			if result.Delivery == nil {
				t.Fatalf("%s: response carries no delivery receipt", step)
			}
			return result
		}
		f.nudges = func() int {
			n := 0
			for _, call := range fs.sp.Calls {
				if call.Method == "Nudge" {
					n++
				}
			}
			return n
		}

		// The first delivery: its transcript row is written, then it is
		// frozen before it can claim the fan-out.
		go func() { f.frozen <- serve() }()
		select {
		case <-gate.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("first delivery never reached the transcript append")
		}
		return f
	}
	awaitFrozen := func(t *testing.T, f *fixture) extmsg.InboundResult {
		t.Helper()
		select {
		case rec := <-f.frozen:
			return f.decode("frozen first delivery", rec)
		case <-time.After(10 * time.Second):
			t.Fatal("frozen first delivery never returned after release — it fanned out and is waiting on the runtime")
		}
		return extmsg.InboundResult{}
	}

	t.Run("duplicate delivered before the first request claimed", func(t *testing.T) {
		f := setup(t)

		// The duplicate runs to completion in the window: it is a transcript
		// duplicate with no receipt evidence, so it claims and delivers.
		dup := f.decode("duplicate in the window", f.serveDup())
		if !dup.Duplicate {
			t.Fatal("second request not marked Duplicate — the first's row was not visible to it, so this is not the race")
		}
		if dup.Delivery.Status != extmsg.InboundDeliveryDelivered || dup.Delivery.DeliveredBytes == 0 {
			t.Fatalf("duplicate in the window: %+v, want delivered — it holds the only claim", dup.Delivery)
		}

		// The first request wakes up holding a fresh transcript row and NO
		// claim: the message is already delivered, so it must not fan out.
		close(f.gate.release)
		first := awaitFrozen(t, f)
		if first.Duplicate {
			t.Fatal("first delivery marked Duplicate — the gate did not freeze the request that wrote the row")
		}
		if first.Delivery.Status != extmsg.InboundDeliveryNoRoute {
			t.Fatalf("first delivery after the duplicate delivered: status = %q, want no_route (suppressed on the duplicate's receipt): %+v",
				first.Delivery.Status, first.Delivery)
		}
		if n := f.nudges(); n != 1 {
			t.Fatalf("runtime received %d nudges for one message posted twice, want exactly 1", n)
		}
		if st := f.srv.inboundReceiptStore().Lookup(f.fs.CityName(), first.Delivery.ReceiptID); st.State != extmsg.InboundReceiptConcluded || st.Delivery == nil || st.Delivery.Status != extmsg.InboundDeliveryNoRoute {
			t.Fatalf("suppression receipt in store = %+v, want the no_route conclusion the response carried", st)
		}
	})

	t.Run("duplicate still fanning out when the first request claims", func(t *testing.T) {
		f := setup(t)
		gatedRuntime := &gatedNudgeProvider{Fake: f.fs.sp, entered: make(chan struct{}, 2), release: make(chan struct{})}
		f.fs.sessionProvider = gatedRuntime

		// The duplicate claims and its fan-out parks inside the runtime.
		dupDone := make(chan *httptest.ResponseRecorder, 1)
		go func() { dupDone <- f.serveDup() }()
		select {
		case <-gatedRuntime.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("duplicate's fan-out never reached the runtime")
		}

		// The first request wakes up while that fan-out is running: it must
		// hold on the duplicate's receipt, not start a second fan-out.
		close(f.gate.release)
		first := awaitFrozen(t, f)
		if first.Delivery.Status != extmsg.InboundDeliveryPending {
			t.Fatalf("first delivery while the duplicate's fan-out runs: status = %q, want pending (held): %+v", first.Delivery.Status, first.Delivery)
		}
		select {
		case <-gatedRuntime.entered:
			t.Fatal("a second nudge reached the runtime: both requests fanned out")
		default:
		}

		close(gatedRuntime.release)
		var dup extmsg.InboundResult
		select {
		case rec := <-dupDone:
			dup = f.decode("duplicate", rec)
		case <-time.After(10 * time.Second):
			t.Fatal("duplicate never returned after the runtime was released")
		}
		if !dup.Duplicate || dup.Delivery.Status != extmsg.InboundDeliveryDelivered {
			t.Fatalf("duplicate: %+v (Duplicate=%v), want delivered by the only fan-out", dup.Delivery, dup.Duplicate)
		}
		if first.Delivery.ReceiptID != dup.Delivery.ReceiptID {
			t.Fatalf("first delivery held on receipt %q, want the duplicate's %q so its poll lands on the fan-out carrying the message",
				first.Delivery.ReceiptID, dup.Delivery.ReceiptID)
		}
		if n := f.nudges(); n != 1 {
			t.Fatalf("runtime received %d nudges for one message posted twice, want exactly 1", n)
		}
	})
}
