package extmsg

import (
	"context"
	"errors"
	"testing"

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
			input := UnbindInput{Conversation: &ref, ExpectedAgentName: "helper", ExpectedCallerID: caller.ID, Now: testNow()}
			if byID {
				input = UnbindInput{BindingID: first.ID, Now: testNow()}
			}
			removed, err := svc.Unbind(ctx, testControllerCaller(), input)
			if err != nil || len(removed) != 1 || removed[0].ID != first.ID || removed[0].Caller.ID != caller.ID {
				t.Fatalf("Unbind = %v, %v", removed, err)
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
