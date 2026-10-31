package nudgequeue

import (
	"fmt"
	"time"
)

// PrepareReplay copies a dead item's input and destination into a fresh delivery.
// It does not mutate state, so callers can use it for a read-only preview.
func PrepareReplay(state *State, id, newID string, now time.Time, ttl time.Duration) (Item, error) {
	dead, err := replayableItem(state, id)
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
func RecordReplay(state *State, id, newID string) error {
	dead, err := replayableItem(state, id)
	if err != nil {
		return err
	}
	dead.ReplayID = newID
	return nil
}

func replayableItem(state *State, id string) (*Item, error) {
	for i := range state.Dead {
		item := &state.Dead[i]
		if item.ID != id {
			continue
		}
		if item.ReplayID != "" {
			return nil, fmt.Errorf("nudge %q already replayed as %q", id, item.ReplayID)
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
