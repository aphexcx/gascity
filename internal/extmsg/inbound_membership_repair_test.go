package extmsg

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// These tests pin the hq-ar4 recurrence: a conversation whose binding is
// active but whose transcript membership record has been lost (closed by a
// cleanup race) must have the membership repaired by the next inbound.
// Delivery fans out over memberships, so without the repair every inbound is
// accepted, transcribed, and evented — then notified to nobody, forever.

func TestHandleInboundNormalizedHandoffDoesNotRestoreDisplacedMembership(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	locks := sharedBindingLockPool(store)
	realTranscript := newTranscriptService(store, locks)
	transcript := &handoffCoordinatingTranscript{
		TranscriptService: realTranscript,
		locked:            realTranscript,
	}
	bindings := newBindingService(store, nil, transcript, locks)
	ref := testConversationRef()

	if _, err := bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "rig-a/old",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(old): %v", err)
	}

	transcript.publicEnsureReached = make(chan struct{})
	transcript.releasePublicEnsure = make(chan struct{})
	transcript.lockedEnsureReached = make(chan struct{})
	transcript.releaseLockedEnsure = make(chan struct{})

	type inboundOutcome struct {
		result *InboundResult
		err    error
	}
	firstInbound := make(chan inboundOutcome, 1)
	go func() {
		result, err := HandleInboundNormalized(context.Background(), InboundDeps{Services: Services{
			Bindings:   bindings,
			Transcript: transcript,
		}}, ExternalInboundMessage{
			Conversation:      ref,
			ProviderMessageID: "message-1",
			Actor:             ExternalActor{ID: "user-1", DisplayName: "User One"},
			Text:              "first",
			ReceivedAt:        testNow(),
		})
		firstInbound <- inboundOutcome{result: result, err: err}
	}()

	type bindOutcome struct {
		binding SessionBindingRecord
		err     error
	}
	handoff := func() bindOutcome {
		binding, err := bindings.Bind(context.Background(), testControllerCaller(), BindInput{
			Conversation: ref,
			AgentName:    "rig-a/new",
			Replace:      true,
			Now:          testNow().Add(time.Minute),
		})
		return bindOutcome{binding: binding, err: err}
	}

	var handedOff bindOutcome
	select {
	case <-transcript.publicEnsureReached:
		// Before the fix, binding resolution released the conversation lock
		// before calling the public membership repair. Complete the handoff in
		// that gap, then let the stale repair continue.
		handedOff = handoff()
		close(transcript.releasePublicEnsure)
	case <-transcript.lockedEnsureReached:
		// The fixed path repairs while holding the conversation lock. Queue the
		// handoff behind it, then release the repair.
		handoffDone := make(chan bindOutcome, 1)
		go func() { handoffDone <- handoff() }()
		close(transcript.releaseLockedEnsure)
		handedOff = <-handoffDone
	}
	if handedOff.err != nil {
		t.Fatalf("Bind(handoff): %v", handedOff.err)
	}
	if handedOff.binding.AgentName != "rig-a/new" {
		t.Fatalf("handoff AgentName = %q, want rig-a/new", handedOff.binding.AgentName)
	}

	first := <-firstInbound
	if first.err != nil {
		t.Fatalf("HandleInboundNormalized(first): %v", first.err)
	}

	second, err := HandleInboundNormalized(context.Background(), InboundDeps{Services: Services{
		Bindings:   bindings,
		Transcript: transcript,
	}}, ExternalInboundMessage{
		Conversation:      ref,
		ProviderMessageID: "message-2",
		Actor:             ExternalActor{ID: "user-1", DisplayName: "User One"},
		Text:              "second",
		ReceivedAt:        testNow().Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("HandleInboundNormalized(second): %v", err)
	}
	if second.TargetAgentName != "rig-a/new" {
		t.Fatalf("second TargetAgentName = %q, want rig-a/new", second.TargetAgentName)
	}

	members, err := transcript.ListMemberships(context.Background(), testControllerCaller(), ref)
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if len(members) != 1 || members[0].SessionID != "rig-a/new" {
		t.Fatalf("memberships after handoff and next inbound = %#v, want only rig-a/new", members)
	}
}

type handoffCoordinatingTranscript struct {
	TranscriptService
	locked bindingMembershipEnsurer

	publicEnsureReached chan struct{}
	releasePublicEnsure chan struct{}
	lockedEnsureReached chan struct{}
	releaseLockedEnsure chan struct{}
}

func (s *handoffCoordinatingTranscript) EnsureMembership(ctx context.Context, input EnsureMembershipInput) (ConversationMembershipRecord, error) {
	if s.publicEnsureReached != nil {
		close(s.publicEnsureReached)
		<-s.releasePublicEnsure
		s.publicEnsureReached = nil
	}
	return s.TranscriptService.EnsureMembership(ctx, input)
}

func (s *handoffCoordinatingTranscript) ensureMembershipLocked(input EnsureMembershipInput) (ConversationMembershipRecord, error) {
	if s.lockedEnsureReached != nil {
		close(s.lockedEnsureReached)
		<-s.releaseLockedEnsure
		s.lockedEnsureReached = nil
	}
	return s.locked.ensureMembershipLocked(input)
}

func (s *handoffCoordinatingTranscript) ensureMembershipLockedWriter(w membershipWriter, input EnsureMembershipInput) (ConversationMembershipRecord, error) {
	return s.locked.ensureMembershipLockedWriter(w, input)
}

func (s *handoffCoordinatingTranscript) removeMembershipLocked(input RemoveMembershipInput) error {
	return s.locked.removeMembershipLocked(input)
}

func TestHandleInboundNormalizedSkipsRepairWithoutBindingLockContract(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "rig-a/helper",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(agent): %v", err)
	}
	if err := fabric.Transcript.RemoveMembership(context.Background(), RemoveMembershipInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		SessionID:    "rig-a/helper",
		Owner:        MembershipOwnerBinding,
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("RemoveMembership: %v", err)
	}

	transcript := &ensureRecordingTranscript{TranscriptService: fabric.Transcript}
	result, err := HandleInboundNormalized(context.Background(), InboundDeps{Services: Services{
		Bindings:   bindingServiceWrapper{BindingService: fabric.Bindings},
		Transcript: transcript,
	}}, ExternalInboundMessage{
		Conversation:      ref,
		ProviderMessageID: "message-1",
		Actor:             ExternalActor{ID: "user-1", DisplayName: "User One"},
		Text:              "hello",
		ReceivedAt:        testNow(),
	})
	if err != nil {
		t.Fatalf("HandleInboundNormalized: %v", err)
	}
	if result.TargetAgentName != "rig-a/helper" {
		t.Fatalf("TargetAgentName = %q, want rig-a/helper", result.TargetAgentName)
	}
	if transcript.ensureCalls != 0 {
		t.Fatalf("EnsureMembership calls = %d, want 0 without a binding lock contract", transcript.ensureCalls)
	}
	members, err := transcript.ListMemberships(context.Background(), testControllerCaller(), ref)
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("memberships = %#v, want no unlocked repair", members)
	}
}

type bindingServiceWrapper struct {
	BindingService
}

type ensureRecordingTranscript struct {
	TranscriptService
	ensureCalls int
}

func (s *ensureRecordingTranscript) EnsureMembership(ctx context.Context, input EnsureMembershipInput) (ConversationMembershipRecord, error) {
	s.ensureCalls++
	return s.TranscriptService.EnsureMembership(ctx, input)
}

func TestHandleInboundNormalizedRepairsMissingAgentBindingMembership(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "rig-a/helper",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(agent): %v", err)
	}

	// Simulate the production state: the binding-owned membership vanishes
	// while the binding stays active.
	if err := fabric.Transcript.RemoveMembership(context.Background(), RemoveMembershipInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		SessionID:    "rig-a/helper",
		Owner:        MembershipOwnerBinding,
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("RemoveMembership: %v", err)
	}
	members, err := fabric.Transcript.ListMemberships(context.Background(), testControllerCaller(), ref)
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("memberships after removal = %#v, want none", members)
	}

	deps := InboundDeps{Services: fabric}
	result, err := HandleInboundNormalized(context.Background(), deps, ExternalInboundMessage{
		Conversation: ref,
		Actor:        ExternalActor{ID: "user-1", DisplayName: "User One"},
		Text:         "hello",
		ReceivedAt:   testNow(),
	})
	if err != nil {
		t.Fatalf("HandleInboundNormalized: %v", err)
	}
	if result.TargetAgentName != "rig-a/helper" {
		t.Fatalf("TargetAgentName = %q, want rig-a/helper", result.TargetAgentName)
	}

	members, err = fabric.Transcript.ListMemberships(context.Background(), testControllerCaller(), ref)
	if err != nil {
		t.Fatalf("ListMemberships after inbound: %v", err)
	}
	if len(members) != 1 || members[0].SessionID != "rig-a/helper" {
		t.Fatalf("memberships after inbound = %#v, want one keyed rig-a/helper (membership repaired)", members)
	}
}

func TestHandleInboundNormalizedRepairsMissingSessionBindingMembership(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(session): %v", err)
	}
	if err := fabric.Transcript.RemoveMembership(context.Background(), RemoveMembershipInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		SessionID:    "sess-a",
		Owner:        MembershipOwnerBinding,
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("RemoveMembership: %v", err)
	}

	deps := InboundDeps{Services: fabric}
	result, err := HandleInboundNormalized(context.Background(), deps, ExternalInboundMessage{
		Conversation: ref,
		Actor:        ExternalActor{ID: "user-1", DisplayName: "User One"},
		Text:         "hello",
		ReceivedAt:   testNow(),
	})
	if err != nil {
		t.Fatalf("HandleInboundNormalized: %v", err)
	}
	if result.TargetSessionID != "sess-a" {
		t.Fatalf("TargetSessionID = %q, want sess-a", result.TargetSessionID)
	}

	members, err := fabric.Transcript.ListMemberships(context.Background(), testControllerCaller(), ref)
	if err != nil {
		t.Fatalf("ListMemberships after inbound: %v", err)
	}
	if len(members) != 1 || members[0].SessionID != "sess-a" {
		t.Fatalf("memberships after inbound = %#v, want one keyed sess-a (membership repaired)", members)
	}
}

// An intact membership must pass through the repair as a no-op: same single
// membership record, no owner or policy churn.
func TestHandleInboundNormalizedLeavesIntactMembershipAlone(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "rig-a/helper",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(agent): %v", err)
	}
	before, err := fabric.Transcript.ListMemberships(context.Background(), testControllerCaller(), ref)
	if err != nil || len(before) != 1 {
		t.Fatalf("ListMemberships before = %#v (%v), want one", before, err)
	}

	deps := InboundDeps{Services: fabric}
	if _, err := HandleInboundNormalized(context.Background(), deps, ExternalInboundMessage{
		Conversation: ref,
		Actor:        ExternalActor{ID: "user-1", DisplayName: "User One"},
		Text:         "hello",
		ReceivedAt:   testNow(),
	}); err != nil {
		t.Fatalf("HandleInboundNormalized: %v", err)
	}

	after, err := fabric.Transcript.ListMemberships(context.Background(), testControllerCaller(), ref)
	if err != nil {
		t.Fatalf("ListMemberships after: %v", err)
	}
	if len(after) != 1 || after[0].ID != before[0].ID {
		t.Fatalf("memberships after inbound = %#v, want the original record %s untouched", after, before[0].ID)
	}
}
