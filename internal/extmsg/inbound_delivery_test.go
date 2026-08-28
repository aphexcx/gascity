package extmsg

import "testing"

// TestSummarizeInboundDeliveryStatusPrecedence pins the rule that makes the
// receipt safe to gate on: the aggregate is never more certain than its
// members. A consumer commits an irreversible dedup claim on "delivered", so
// every mixed or unconcluded combination must resolve to something else.
func TestSummarizeInboundDeliveryStatusPrecedence(t *testing.T) {
	member := func(status InboundDeliveryStatus, delivered, expected int) InboundDeliveryMember {
		return InboundDeliveryMember{SessionID: "s", Status: status, DeliveredBytes: delivered, ExpectedBytes: expected}
	}
	cases := []struct {
		name    string
		members []InboundDeliveryMember
		want    InboundDeliveryStatus
		why     string
	}{
		{
			name:    "no members is no_route not failed",
			members: nil,
			want:    InboundDeliveryNoRoute,
			why:     "nothing to retry toward; a consumer must commit, not redeliver forever",
		},
		{
			name:    "every member delivered",
			members: []InboundDeliveryMember{member(InboundDeliveryDelivered, 10, 10), member(InboundDeliveryDelivered, 20, 20)},
			want:    InboundDeliveryDelivered,
			why:     "unanimity is the only thing that earns the delivered claim",
		},
		{
			name:    "one delivered one failed",
			members: []InboundDeliveryMember{member(InboundDeliveryDelivered, 10, 10), member(InboundDeliveryFailed, 0, 20)},
			want:    InboundDeliveryPartial,
			why:     "the message is live somewhere, so a retry duplicates for that member",
		},
		{
			name:    "one delivered one pending",
			members: []InboundDeliveryMember{member(InboundDeliveryDelivered, 10, 10), member(InboundDeliveryPending, 0, 20)},
			want:    InboundDeliveryPartial,
			why:     "a pending sibling must not be rounded up into a delivered aggregate",
		},
		{
			name:    "all failed",
			members: []InboundDeliveryMember{member(InboundDeliveryFailed, 0, 10)},
			want:    InboundDeliveryFailed,
			why:     "nothing landed, so a retry is clean",
		},
		{
			name:    "all pending",
			members: []InboundDeliveryMember{member(InboundDeliveryPending, 0, 10)},
			want:    InboundDeliveryPending,
			why:     "unconcluded is not the same claim as failed",
		},
		{
			name:    "pending outranks failed when nothing delivered",
			members: []InboundDeliveryMember{member(InboundDeliveryFailed, 0, 10), member(InboundDeliveryPending, 0, 20)},
			want:    InboundDeliveryPending,
			why: "failed promises a retry is clean; the pending member may still land, which would " +
				"turn that retry into a duplicate. Unknown is the only supportable claim",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SummarizeInboundDelivery("ir-test-1", tc.members)
			if got.Status != tc.want {
				t.Fatalf("status = %q, want %q — %s", got.Status, tc.want, tc.why)
			}
			if got.ReceiptID != "ir-test-1" {
				t.Fatalf("receipt id = %q, want it carried through verbatim", got.ReceiptID)
			}
		})
	}
}

// TestSummarizeInboundDeliverySumsBytesAcrossMembers covers the invariant the
// consumer's delivered>=expected check rests on. Per-member reminders differ
// (each embeds its recipient's handle), so the top-level numbers are sums, and
// they may only be equal when nothing was dropped.
func TestSummarizeInboundDeliverySumsBytesAcrossMembers(t *testing.T) {
	got := SummarizeInboundDelivery("ir-test-2", []InboundDeliveryMember{
		{SessionID: "a", Status: InboundDeliveryDelivered, DeliveredBytes: 100, ExpectedBytes: 100},
		{SessionID: "b", Status: InboundDeliveryFailed, DeliveredBytes: 0, ExpectedBytes: 250},
	})
	if got.ExpectedBytes != 350 {
		t.Fatalf("expected_bytes = %d, want 350 (sum of both members' reminders)", got.ExpectedBytes)
	}
	if got.DeliveredBytes != 100 {
		t.Fatalf("delivered_bytes = %d, want 100 (only member a landed)", got.DeliveredBytes)
	}
	if got.DeliveredBytes >= got.ExpectedBytes {
		t.Fatal("delivered_bytes must stay below expected_bytes when a member got nothing — " +
			"this is the exact comparison the Slack adapter gates its dedup claim on")
	}
}

// TestPendingInboundDeliveryCarriesNoCounts guards a subtle overclaim: a
// pending receipt with zeroed byte counts satisfies delivered >= expected
// (0 >= 0) and would read to a consumer as a completed empty delivery.
func TestPendingInboundDeliveryCarriesNoCounts(t *testing.T) {
	got := PendingInboundDelivery("ir-test-3")
	if got.Status != InboundDeliveryPending {
		t.Fatalf("status = %q, want pending", got.Status)
	}
	if len(got.Members) != 0 {
		t.Fatalf("members = %d, want none: gc does not know them yet", len(got.Members))
	}
	if got.ReceiptID == "" {
		t.Fatal("a pending receipt still needs an id — it is the only handle for correlating the late-landing send")
	}
}

// TestNextInboundReceiptIDIsUnique keeps ids usable as delivery handles: two
// deliveries of identical text must be distinguishable in a log.
func TestNextInboundReceiptIDIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := NextInboundReceiptID()
		if id == "" {
			t.Fatal("empty receipt id")
		}
		if seen[id] {
			t.Fatalf("duplicate receipt id %q", id)
		}
		seen[id] = true
	}
}

// TestFailedInboundDeliveryIsNotMistakableForNoRoute covers the data-loss path:
// a membership lookup that FAILED has no members, exactly like a conversation
// that genuinely has none. But no_route tells a consumer to commit its dedup
// claim and stop retrying, so letting a transient store fault wear that verdict
// would silently discard the message.
func TestFailedInboundDeliveryIsNotMistakableForNoRoute(t *testing.T) {
	got := FailedInboundDelivery("ir-test-4", "list memberships: store unavailable")
	if got.Status != InboundDeliveryFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if got.Status == InboundDeliveryNoRoute {
		t.Fatal("a failed lookup must never summarize as no_route — that verdict tells the consumer to drop the message")
	}
	if len(got.Members) == 0 || got.Members[0].Error == "" {
		t.Fatalf("failed delivery must carry the reason for an operator: %+v", got)
	}
}
