package main

import "time"

// claimRefusalLog bounds repeated owner-fence diagnostics across reconcile
// ticks. Its owner is one city's runtime or demand builder, never a global.
// Callers retain per-snapshot dedupe and supply the tick's clock.
type claimRefusalLog struct {
	entries map[storeScopedBeadKey]claimRefusalEntry
	sweptAt time.Time
}

type claimRefusalEntry struct {
	reason   string
	loggedAt time.Time
}

const claimRefusalLogInterval = time.Hour

func (l *claimRefusalLog) shouldLog(now time.Time, key storeScopedBeadKey, reason string) bool {
	// One-shot callers have no history to retain across ticks.
	if l == nil {
		return true
	}
	if l.entries == nil {
		l.entries = make(map[storeScopedBeadKey]claimRefusalEntry)
	}
	if l.sweptAt.IsZero() || now.Sub(l.sweptAt) >= claimRefusalLogInterval {
		for k, entry := range l.entries {
			if now.Sub(entry.loggedAt) >= claimRefusalLogInterval {
				delete(l.entries, k)
			}
		}
		l.sweptAt = now
	}
	key.StoreRef = normalizeIdleClaimStoreRef(key.StoreRef)
	if previous, ok := l.entries[key]; ok && previous.reason == reason && now.Sub(previous.loggedAt) < claimRefusalLogInterval {
		return false
	}
	l.entries[key] = claimRefusalEntry{reason: reason, loggedAt: now}
	return true
}
