package main

import (
	"testing"
	"time"
)

// Recurring foreign work must not flood diagnostics; changed refusals and
// different stores still need their own first report.
func TestClaimBackstopRefusalLogCadence(t *testing.T) {
	var refusals claimRefusalLog
	now := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, store, id, reason string
		after                   time.Duration
		want                    bool
	}{
		{"first", "city:jadegate", "work", "citadel", 0, true},
		{"same-tick-alias", "city", "work", "citadel", 0, false},
		{"later-tick", "city", "work", "citadel", 30 * time.Second, false},
		{"just-before-hour", "city", "work", "citadel", time.Hour - time.Nanosecond, false},
		{"hour", "city", "work", "citadel", time.Hour, true},
		{"reason-change", "city", "work", "boomtown", time.Hour + time.Second, true},
		{"repeat-new-reason", "city", "work", "boomtown", time.Hour + time.Minute, false},
		{"different-store", "rig:other", "work", "boomtown", time.Hour + time.Minute, true},
		{"different-bead", "city", "another", "boomtown", time.Hour + time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := refusals.shouldLog(now.Add(tc.after), storeScopedBeadKey{StoreRef: tc.store, ID: tc.id}, tc.reason); got != tc.want {
				t.Errorf("shouldLog=%v, want %v", got, tc.want)
			}
		})
	}
	// A later observation expires old history rather than retaining every bead
	// the runtime has ever encountered.
	refusals.shouldLog(now.Add(3*time.Hour), storeScopedBeadKey{StoreRef: "city", ID: "new"}, "citadel")
	if got := len(refusals.entries); got != 1 {
		t.Errorf("retained refusal entries=%d, want only the new bead", got)
	}
	var anotherCity claimRefusalLog
	if !anotherCity.shouldLog(now, storeScopedBeadKey{StoreRef: "city", ID: "work"}, "citadel") {
		t.Error("one runtime suppressed another runtime's first report")
	}
}
