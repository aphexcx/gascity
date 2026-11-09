package extmsg

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// A read followed by a same-agent explicit bind must not let either guard
// remove the explicit binding using the default route's identity.
func TestBindingConditionalUnbindProtectsReplacement(t *testing.T) {
	for _, byID := range []bool{false, true} {
		t.Run(map[bool]string{false: "provenance", true: "id"}[byID], func(t *testing.T) {
			freezeTestClock(t)
			ctx := context.Background()
			svc := NewServices(beads.NewMemStore()).Bindings
			ref := testConversationRef()
			defaultCaller := Caller{Kind: CallerController, ID: "extmsg-default-route"}
			first, err := svc.Bind(ctx, defaultCaller, BindInput{Conversation: ref, AgentName: "helper", Now: testNow()})
			if err != nil {
				t.Fatal(err)
			}
			second, err := svc.Bind(ctx, testControllerCaller(), BindInput{Conversation: ref, AgentName: "helper", Now: testNow()})
			if err != nil {
				t.Fatal(err)
			}
			if second.ID == first.ID || second.Caller.ID != testControllerCaller().ID {
				t.Fatalf("ownership change retained stale identity: first=%+v second=%+v", first, second)
			}
			input := UnbindInput{Conversation: &ref, ExpectedAgentName: "helper", ExpectedCallerID: defaultCaller.ID, Now: testNow()}
			want := ErrBindingConflict
			if byID {
				input = UnbindInput{BindingID: first.ID, Now: testNow()}
				want = ErrBindingNotFound
			}
			removed, err := svc.Unbind(ctx, testControllerCaller(), input)
			if !errors.Is(err, want) || len(removed) != 0 {
				t.Fatalf("Unbind = %v, %v; want %v", removed, err, want)
			}
			if !byID {
				var conflict *BindingPreconditionError
				if !errors.As(err, &conflict) || conflict.ActiveBinding == nil || conflict.ActiveBinding.ID != second.ID {
					t.Fatalf("missing active binding: %v", err)
				}
			}
			active, err := svc.ResolveByConversation(ctx, ref)
			if err != nil || active == nil || active.ID != second.ID {
				t.Fatalf("replacement lost: %v %v", active, err)
			}
		})
	}
}

func TestBindingConditionalUnbindMatchesAndListsProvenance(t *testing.T) {
	for _, byID := range []bool{false, true} {
		t.Run(map[bool]string{false: "provenance", true: "id"}[byID], func(t *testing.T) {
			freezeTestClock(t)
			ctx := context.Background()
			svc := NewServices(beads.NewMemStore()).Bindings
			ref := testConversationRef()
			caller := Caller{Kind: CallerController, ID: "extmsg-default-route"}
			first, err := svc.Bind(ctx, caller, BindInput{Conversation: ref, AgentName: "helper", Now: testNow()})
			if err != nil {
				t.Fatal(err)
			}
			other := ref
			other.ConversationID += "-other"
			if _, err := svc.Bind(ctx, caller, BindInput{Conversation: other, SessionID: "session-1", Now: testNow()}); err != nil {
				t.Fatal(err)
			}
			for _, conversation := range []ConversationRef{ref, other} {
				listed, err := svc.ListByConversation(ctx, conversation)
				if err != nil || len(listed) != 1 {
					t.Fatalf("ListByConversation = %v, %v", listed, err)
				}
				if listed[0].Caller.ID != caller.ID || listed[0].Caller.Kind != caller.Kind || listed[0].CreatedAt.IsZero() {
					t.Fatalf("missing provenance: %+v", listed[0])
				}
			}
			removedAt := testNow().Add(time.Minute)
			input := UnbindInput{Conversation: &ref, ExpectedAgentName: "helper", ExpectedCallerID: caller.ID, Now: removedAt}
			if byID {
				input = UnbindInput{BindingID: first.ID, Now: removedAt}
			}
			removed, err := svc.Unbind(ctx, testControllerCaller(), input)
			if err != nil || len(removed) != 1 || removed[0].ID != first.ID || removed[0].Caller.ID != caller.ID {
				t.Fatalf("Unbind = %v, %v", removed, err)
			}
			if got := removed[0].Metadata["last_touched_at"]; got != formatTime(removedAt) {
				t.Fatalf("removed last_touched_at = %q, want %q", got, formatTime(removedAt))
			}
			listed, err := svc.ListByConversation(ctx, ref)
			if err != nil || len(listed) != 0 {
				t.Fatalf("ended binding listed: %v, %v", listed, err)
			}
			remaining, err := svc.ListBySession(ctx, "session-1")
			if err != nil || len(remaining) != 1 {
				t.Fatalf("unrelated binding lost: %v, %v", remaining, err)
			}
			if _, err := svc.Unbind(ctx, testControllerCaller(), input); err == nil {
				t.Fatal("stale conditional unbind succeeded")
			} else if !byID {
				var conflict *BindingPreconditionError
				if !errors.As(err, &conflict) || conflict.ActiveBinding != nil {
					t.Fatalf("empty conversation must return a precondition conflict without an active binding: %v", err)
				}
			}
		})
	}
}

func TestBindingOwnershipRotationPreservesTranscript(t *testing.T) {
	for _, targetKind := range []string{"agent", "session"} {
		for _, acknowledged := range []bool{false, true} {
			t.Run(targetKind+"/"+map[bool]string{false: "unread", true: "partly-read"}[acknowledged], func(t *testing.T) {
				freezeTestClock(t)
				ctx := context.Background()
				fabric := NewServices(&atomicTxStore{MemStore: beads.NewMemStore()})
				ref := testConversationRef()
				appendEntry := func(id string) {
					t.Helper()
					if _, _, err := fabric.Transcript.Append(ctx, AppendTranscriptInput{
						Caller: testAdapterCaller(), Conversation: ref,
						Kind: TranscriptMessageInbound, Provenance: TranscriptProvenanceLive,
						ProviderMessageID: id, Text: id, CreatedAt: testNow(),
					}); err != nil {
						t.Fatal(err)
					}
				}
				appendEntry("before-join")
				input := BindInput{Conversation: ref, Now: testNow()}
				if targetKind == "agent" {
					input.AgentName = "helper"
				} else {
					input.SessionID = "session-1"
				}
				first, err := fabric.Bindings.Bind(ctx, Caller{Kind: CallerController, ID: "extmsg-default-route"}, input)
				if err != nil {
					t.Fatal(err)
				}
				appendEntry("after-join-1")
				appendEntry("after-join-2")
				if acknowledged {
					if err := fabric.Transcript.Ack(ctx, AckMembershipInput{
						Caller: testControllerCaller(), Conversation: ref,
						SessionID: bindingMembershipKey(first), Sequence: 2,
					}); err != nil {
						t.Fatal(err)
					}
				}
				before, err := fabric.Transcript.ListMemberships(ctx, testControllerCaller(), ref)
				if err != nil || len(before) != 1 || before[0].JoinedSequence != 1 {
					t.Fatalf("membership before rotation = %+v, %v", before, err)
				}
				input.Now = testNow().Add(time.Minute)
				second, err := fabric.Bindings.Bind(ctx, testControllerCaller(), input)
				if err != nil {
					t.Fatal(err)
				}
				if second.ID == first.ID || second.Caller != testControllerCaller() || second.BindingGeneration != first.BindingGeneration+1 || second.CreatedAt.IsZero() {
					t.Fatalf("binding identity did not rotate: first=%+v second=%+v", first, second)
				}
				after, err := fabric.Transcript.ListMemberships(ctx, testControllerCaller(), ref)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Errorf("membership changed: before=%+v after=%+v err=%v", before, after, err)
				}
				backfill, err := fabric.Transcript.ListBackfill(ctx, ListBackfillInput{
					Caller: testControllerCaller(), Conversation: ref, SessionID: bindingMembershipKey(second),
				})
				if err != nil {
					t.Fatal(err)
				}
				var sequences []int64
				for _, entry := range backfill {
					sequences = append(sequences, entry.Sequence)
				}
				want := []int64{2, 3}
				if acknowledged {
					want = []int64{3}
				}
				if !reflect.DeepEqual(sequences, want) {
					t.Fatalf("backfill sequences = %v, want %v", sequences, want)
				}
			})
		}
	}
}

func TestBindingIfUnboundReturnsActiveBindingUntouched(t *testing.T) {
	freezeTestClock(t)
	ctx := context.Background()
	svc := NewServices(beads.NewMemStore()).Bindings
	ref := testConversationRef()
	first, err := svc.Bind(ctx, testControllerCaller(), BindInput{
		Conversation: ref, AgentName: "helper", IfUnbound: true, Now: testNow(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"helper", "other"} {
		got, err := svc.Bind(ctx, testControllerCaller(), BindInput{
			Conversation: ref, AgentName: agent, IfUnbound: true, Replace: true,
			Metadata: map[string]string{"should-not-change": "value"}, Now: testNow().Add(time.Minute),
		})
		if err != nil || !reflect.DeepEqual(got, first) {
			t.Fatalf("IfUnbound(%q) = %+v, %v; want %+v", agent, got, err, first)
		}
	}
}

func TestBindingOwnershipRotationFailurePreservesMembership(t *testing.T) {
	for _, atomic := range []bool{true, false} {
		t.Run(map[bool]string{true: "atomic", false: "non-atomic"}[atomic], func(t *testing.T) {
			freezeTestClock(t)
			ctx := context.Background()
			var store beads.Store = &bdLikeStore{MemStore: beads.NewMemStore()}
			if atomic {
				store = &atomicTxStore{MemStore: beads.NewMemStore()}
			}
			fabric := NewServices(store)
			ref := testConversationRef()
			input := BindInput{Conversation: ref, SessionID: "session-1", Now: testNow()}
			first, err := fabric.Bindings.Bind(ctx, Caller{Kind: CallerController, ID: "extmsg-default-route"}, input)
			if err != nil {
				t.Fatal(err)
			}
			before := membershipRecordBySession(t, fabric.Transcript, ref, "session-1")
			failure := errors.New("membership ensure failed")
			transcript := &flakyTranscriptService{failEnsureCount: 1, err: failure}
			// A same-target rotation must not clear the session's delivery context.
			delivery := &failingDeliveryContextService{err: errors.New("unexpected delivery clear")}
			svc := newBindingService(store, delivery, transcript, newBindingLockPool())
			if _, err := svc.Bind(ctx, testControllerCaller(), input); !errors.Is(err, failure) {
				t.Fatalf("rotation error = %v, want %v", err, failure)
			}
			if transcript.removeCalls != 0 {
				t.Fatalf("rotation removed membership %d times", transcript.removeCalls)
			}
			after := membershipRecordBySession(t, fabric.Transcript, ref, "session-1")
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("membership changed after failed rotation: before=%+v after=%+v", before, after)
			}
			active, err := fabric.Bindings.ResolveByConversation(ctx, ref)
			if err != nil || active == nil {
				t.Fatalf("failed rotation did not retain exactly one active binding: %+v, %v", active, err)
			}
			if atomic && active.ID != first.ID {
				t.Fatalf("atomic rotation failed to roll back: active=%s want=%s", active.ID, first.ID)
			}
			if _, err := fabric.Bindings.Bind(ctx, testControllerCaller(), input); err != nil {
				t.Fatalf("retry rotation: %v", err)
			}
		})
	}
}

func TestBindingConditionalUnbindRejectsAmbiguousSelectors(t *testing.T) {
	ref := testConversationRef()
	for _, input := range []UnbindInput{
		{Conversation: &ref, ExpectedAgentName: " ", ExpectedCallerID: " "},
		{BindingID: " "},
		{ExpectedAgentName: "helper"},
		{Conversation: &ref, ExpectedCallerID: "caller"},
		{ExpectedAgentName: "helper", ExpectedCallerID: "caller"},
		{BindingID: "id", AgentName: "helper"},
		{BindingID: "id", Conversation: &ref},
		{BindingID: "id", ExpectedAgentName: "helper", ExpectedCallerID: "caller"},
		{Conversation: &ref, ExpectedAgentName: "helper", ExpectedCallerID: "caller", SessionID: "session"},
	} {
		_, err := NewServices(beads.NewMemStore()).Bindings.Unbind(context.Background(), testControllerCaller(), input)
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("input %+v: error = %v", input, err)
		}
	}
}
