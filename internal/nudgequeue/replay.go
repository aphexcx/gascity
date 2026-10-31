package nudgequeue

import (
	"fmt"
	"strings"
	"time"
)

// DeadLetterRetention bounds recovery and disposition receipts from the dead transition.
const DeadLetterRetention = 7 * 24 * time.Hour

// UnresolvedDeadLetter reports whether an item in State.Dead needs operator action.
func (item Item) UnresolvedDeadLetter() bool {
	return strings.TrimSpace(item.LastError) != "superseded" && item.ReplayID == "" && item.DismissedAt.IsZero()
}

// PrepareReplay copies a dead item's input and destination into a fresh delivery.
// It does not mutate state, so callers can use it for a read-only preview.
func PrepareReplay(state *State, id, newID string, now time.Time, ttl time.Duration) (Item, error) {
	dead, err := replayableItem(state, id, now)
	if err != nil {
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
	dead.ReplayID = newID
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
