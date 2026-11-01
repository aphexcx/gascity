package nudgequeue

import (
	"fmt"
	"strings"
	"time"
)

// DeadLetterRetention bounds recovery and disposition receipts from the dead transition.
const DeadLetterRetention = 7 * 24 * time.Hour

// DispositionRetention keeps duplicate-action receipts for one hour after disposition.
const DispositionRetention = time.Hour

// ManualDropReason identifies an explicit operator disposition, including old queue entries.
const ManualDropReason = "dropped by operator (gc nudge drop)"

// UnresolvedDeadLetter reports whether an item in State.Dead needs operator action.
func (item Item) UnresolvedDeadLetter() bool {
	reason := strings.TrimSpace(item.LastError)
	return reason != "superseded" && reason != ManualDropReason && item.ReplayID == "" && item.DismissedAt.IsZero()
}

// DeadLetterPruneAt returns the recovery cap or earlier disposition-receipt expiry.
// Historical receipts without a disposition timestamp use their dead transition.
func (item Item) DeadLetterPruneAt() time.Time {
	deadline := item.DeadAt.Add(DeadLetterRetention)
	if item.UnresolvedDeadLetter() {
		return deadline
	}
	disposedAt := item.DismissedAt
	if disposedAt.IsZero() {
		disposedAt = item.ReplayedAt
	}
	if disposedAt.IsZero() {
		disposedAt = item.DeadAt
	}
	if receiptDeadline := disposedAt.Add(DispositionRetention); receiptDeadline.Before(deadline) {
		return receiptDeadline
	}
	return deadline
}

// PrepareReplay copies a dead item's input and destination into a fresh delivery.
// It does not mutate state, so callers can use it for a read-only preview.
func PrepareReplay(state *State, id, newID string, now time.Time, ttl time.Duration) (Item, error) {
	dead, err := replayableItem(state, id, now)
	if err != nil {
		return Item{}, err
	}
	if err := validateReplaySource(dead); err != nil {
		return Item{}, err
	}
	item := Item{
		ID: newID, OriginID: id, Agent: dead.Agent, SessionID: dead.SessionID,
		ContinuationEpoch: dead.ContinuationEpoch, Source: dead.Source,
		Message: dead.Message, CreatedAt: now.UTC(), DeliverAfter: now.UTC(),
		ExpiresAt: now.Add(ttl).UTC(),
	}
	if dead.Reference != nil {
		ref := *dead.Reference
		item.Reference = &ref
	}
	return item, nil
}

// RecordReplay records the original item's successor. Call it under the same
// WithState transaction that enqueues the new item to serialize competing replays.
func RecordReplay(state *State, id, newID string, now time.Time) error {
	dead, err := replayableItem(state, id, now)
	if err != nil {
		return err
	}
	if err := validateReplaySource(dead); err != nil {
		return err
	}
	dead.ReplayID = newID
	dead.ReplayedAt = now.UTC()
	return nil
}

// Source-owned reminders need their owner's recovery path: a fresh queue ID
// cannot retry a failed wait or make a resolved mail message unread again.
// Check at both preview and commit, while keeping dismissal independent.
func validateReplaySource(item *Item) error {
	if item.Source == "wait" {
		return fmt.Errorf("nudge %q is a wait reminder; use gc wait ready for its wait, or gc nudge replay --dismiss %q", item.ID, item.ID)
	}
	if item.Source == "mail" && item.Reference != nil && item.Reference.Kind == "mail" && item.Reference.ID != "" {
		return fmt.Errorf("nudge %q is a mail reminder; use gc mail read %q to inspect its message, or gc nudge replay --dismiss %q", item.ID, item.Reference.ID, item.ID)
	}
	return nil
}

// DismissDeadLetter records an explicit disposition without enqueuing the input.
func DismissDeadLetter(state *State, id string, now time.Time) error {
	dead, err := replayableItem(state, id, now)
	if err != nil {
		return err
	}
	dead.DismissedAt = now.UTC()
	return nil
}

func replayableItem(state *State, id string, now time.Time) (*Item, error) {
	for i := range state.Dead {
		item := &state.Dead[i]
		if item.ID != id {
			continue
		}
		if item.ReplayID != "" {
			return nil, fmt.Errorf("nudge %q already replayed as %q", id, item.ReplayID)
		}
		if strings.TrimSpace(item.LastError) == "superseded" {
			return nil, fmt.Errorf("nudge %q was superseded; the newer reminder carries the message", id)
		}
		if strings.TrimSpace(item.LastError) == ManualDropReason {
			return nil, fmt.Errorf("nudge %q was dropped by the operator", id)
		}
		if !item.DismissedAt.IsZero() {
			return nil, fmt.Errorf("nudge %q was dismissed at %s", id, item.DismissedAt.Format(time.RFC3339Nano))
		}
		if !item.DeadAt.IsZero() && !item.DeadAt.Add(DeadLetterRetention).After(now) {
			return nil, fmt.Errorf("nudge %q exceeded the seven-day dead-letter retention cap", id)
		}
		return item, nil
	}
	for _, items := range [][]Item{state.Pending, state.InFlight} {
		for _, item := range items {
			if item.ID == id {
				return nil, fmt.Errorf("nudge %q is not dead-lettered", id)
			}
		}
	}
	return nil, fmt.Errorf("dead-letter nudge %q not found", id)
}
