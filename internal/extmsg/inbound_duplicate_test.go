package extmsg

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// An adapter redelivery of an already-transcribed inbound (same conversation
// + provider message id) must be marked Duplicate so the delivery layer can
// suppress the member notification fan-out. Without the mark, every unacked
// redelivery re-injected the same message as an extra turn into the bound
// session — 2-4 duplicate turns per message while the mayor session sat
// wedged behind an auth wall (hq-703om). The redelivery emits
// events.ExtMsgInboundDuplicate instead of a second events.ExtMsgInbound.
func TestHandleInboundNormalizedRedeliveryMarksDuplicate(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()
	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	var captured []capturedEvent
	deps := InboundDeps{
		Services: fabric,
		EmitEvent: func(eventType, subject string, payload events.Payload) {
			captured = append(captured, capturedEvent{Type: eventType, Subject: subject, Payload: payload})
		},
	}
	message := ExternalInboundMessage{
		Conversation:      ref,
		ProviderMessageID: "1785354286.000100",
		Actor:             ExternalActor{ID: "user-1", DisplayName: "User One"},
		Text:              "fix first",
		ReceivedAt:        testNow(),
	}

	first, err := HandleInboundNormalized(context.Background(), deps, message)
	if err != nil {
		t.Fatalf("HandleInboundNormalized(first): %v", err)
	}
	if first.Duplicate {
		t.Fatalf("first delivery Duplicate = true, want false")
	}
	if first.TranscriptEntry == nil {
		t.Fatalf("first delivery TranscriptEntry = nil, want entry")
	}
	if len(captured) != 1 || captured[0].Type != events.ExtMsgInbound {
		t.Fatalf("first delivery events = %#v, want one %s", captured, events.ExtMsgInbound)
	}

	captured = nil
	redelivery := message
	redelivery.ReceivedAt = testNow().Add(time.Minute)
	second, err := HandleInboundNormalized(context.Background(), deps, redelivery)
	if err != nil {
		t.Fatalf("HandleInboundNormalized(redelivery): %v", err)
	}
	if !second.Duplicate {
		t.Fatalf("redelivery Duplicate = false, want true")
	}
	if second.TranscriptEntry == nil || second.TranscriptEntry.ID != first.TranscriptEntry.ID {
		t.Fatalf("redelivery TranscriptEntry = %#v, want original entry %q", second.TranscriptEntry, first.TranscriptEntry.ID)
	}
	if len(captured) != 1 || captured[0].Type != events.ExtMsgInboundDuplicate {
		t.Fatalf("redelivery events = %#v, want one %s", captured, events.ExtMsgInboundDuplicate)
	}
	payload, ok := captured[0].Payload.(InboundDuplicateEventPayload)
	if !ok {
		t.Fatalf("payload = %#v, want InboundDuplicateEventPayload", captured[0].Payload)
	}
	if payload.ProviderMessageID != message.ProviderMessageID {
		t.Fatalf("payload.ProviderMessageID = %q, want %q", payload.ProviderMessageID, message.ProviderMessageID)
	}
	if payload.TargetSession != "sess-a" {
		t.Fatalf("payload.TargetSession = %q, want sess-a", payload.TargetSession)
	}

	// The transcript holds exactly one entry — the dedup is what makes
	// suppressing the redelivery notification safe.
	entries, err := fabric.Transcript.List(context.Background(), ListTranscriptInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("transcript entries = %d, want 1", len(entries))
	}
}

// A message without a provider message id has no redelivery identity, so it
// is never marked Duplicate — each accept is treated as a new message.
func TestHandleInboundNormalizedNoProviderMessageIDNeverDuplicate(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()
	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	deps := InboundDeps{Services: fabric}
	message := ExternalInboundMessage{
		Conversation: ref,
		Actor:        ExternalActor{ID: "user-1", DisplayName: "User One"},
		Text:         "no identity",
		ReceivedAt:   testNow(),
	}
	for i := 0; i < 2; i++ {
		result, err := HandleInboundNormalized(context.Background(), deps, message)
		if err != nil {
			t.Fatalf("HandleInboundNormalized(%d): %v", i, err)
		}
		if result.Duplicate {
			t.Fatalf("delivery %d Duplicate = true, want false (no provider message id)", i)
		}
	}
}
