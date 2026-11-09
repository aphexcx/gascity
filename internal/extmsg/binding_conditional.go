package extmsg

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// unbindConditional checks identity and closes under the same lock as Bind.
func (s *bindingService) unbindConditional(ctx context.Context, caller Caller, input UnbindInput) ([]SessionBindingRecord, error) {
	byID := input.BindingID != ""
	if strings.TrimSpace(input.SessionID) != "" || strings.TrimSpace(input.AgentName) != "" ||
		(byID && (input.Conversation != nil || input.ExpectedAgentName != "" || input.ExpectedCallerID != "")) ||
		(!byID && (input.Conversation == nil || input.ExpectedAgentName == "" || input.ExpectedCallerID == "")) {
		return nil, fmt.Errorf("%w: use binding_id alone, or conversation with both expected_agent_name and expected_caller_id", ErrInvalidInput)
	}
	var ref ConversationRef
	if byID {
		item, err := s.store.Get(input.BindingID)
		if errors.Is(err, beads.ErrNotFound) {
			return nil, ErrBindingNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("get binding %s: %w", input.BindingID, err)
		}
		if !hasLabel(item, "gc:extmsg-binding") {
			return nil, ErrBindingNotFound
		}
		record, err := decodeBindingBead(item)
		if err != nil {
			return nil, err
		}
		ref = record.Conversation
	} else {
		var err error
		ref, err = validateConversationRef(*input.Conversation)
		if err != nil {
			return nil, err
		}
	}
	if err := authorizeMutation(caller, ref); err != nil {
		return nil, err
	}
	now := zeroNow(input.Now)
	var removed []SessionBindingRecord
	err := withBindingLock(s.locks, ref, func() error {
		if err := checkContext(ctx); err != nil {
			return err
		}
		history, err := s.listBindingsForConversation(ref)
		if err != nil {
			return err
		}
		// A failed precondition must not expire or otherwise mutate bindings.
		active, err := selectActiveBinding(ctx, history, now, func(SessionBindingRecord) error { return nil })
		if err != nil {
			return err
		}
		if byID {
			if active == nil || active.ID != input.BindingID {
				return ErrBindingNotFound
			}
		} else if active == nil || active.AgentName != input.ExpectedAgentName || active.Caller.ID != input.ExpectedCallerID {
			return &BindingPreconditionError{ActiveBinding: active}
		}
		if err := s.endActiveBindingLocked(ctx, caller, *active, now); err != nil {
			return err
		}
		active.Status = BindingEnded
		removed = []SessionBindingRecord{*active}
		if s.delivery != nil && active.SessionID != "" {
			return s.delivery.ClearForConversation(ctx, active.SessionID, ref)
		}
		return nil
	})
	return removed, err
}
