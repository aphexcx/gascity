package extmsg

import (
	"testing"
	"time"
)

func newTestReceiptStore(now *time.Time) *InboundReceiptStore {
	s := NewInboundReceiptStore()
	s.now = func() time.Time { return *now }
	return s
}

func TestInboundReceiptStoreUnknownIDIsUnknown(t *testing.T) {
	now := time.Date(2026, 8, 28, 8, 24, 0, 0, time.UTC)
	s := newTestReceiptStore(&now)

	got := s.Lookup("c", "ir-1-999")
	if got.State != InboundReceiptUnknown {
		t.Fatalf("state = %q, want unknown", got.State)
	}
	if got.ReceiptID != "ir-1-999" {
		t.Fatalf("receipt_id = %q, want echoed back", got.ReceiptID)
	}
	if got.Delivery != nil {
		t.Fatalf("unknown receipt carried a delivery: %+v", got.Delivery)
	}
}

func TestInboundReceiptStoreBegunIsPendingUntilConcluded(t *testing.T) {
	now := time.Date(2026, 8, 28, 8, 24, 0, 0, time.UTC)
	s := newTestReceiptStore(&now)

	s.Begin("c", "ir-1-1")
	if got := s.Lookup("c", "ir-1-1"); got.State != InboundReceiptPending || got.Delivery != nil {
		t.Fatalf("after Begin: %+v, want pending with no delivery", got)
	}

	delivery := SummarizeInboundDelivery("ir-1-1", []InboundDeliveryMember{{
		SessionID: "s1", Status: InboundDeliveryDelivered, DeliveredBytes: 223, ExpectedBytes: 223,
	}})
	s.Conclude("c", "ir-1-1", delivery)
	got := s.Lookup("c", "ir-1-1")
	if got.State != InboundReceiptConcluded {
		t.Fatalf("after Conclude: state = %q, want concluded", got.State)
	}
	if got.Delivery == nil || got.Delivery.Status != InboundDeliveryDelivered || got.Delivery.DeliveredBytes != 223 {
		t.Fatalf("after Conclude: delivery = %+v, want the concluded delivery verbatim", got.Delivery)
	}
}

// A conclusion that arrives without a matching Begin (a caller that only
// records the receipt once it knows it answered pending) must still be
// queryable: the whole point of the store is that a late result is never
// dropped on the floor.
func TestInboundReceiptStoreConcludeWithoutBeginStillRecords(t *testing.T) {
	now := time.Date(2026, 8, 28, 8, 24, 0, 0, time.UTC)
	s := newTestReceiptStore(&now)

	s.Conclude("c", "ir-1-7", FailedInboundDelivery("ir-1-7", "boom"))
	got := s.Lookup("c", "ir-1-7")
	if got.State != InboundReceiptConcluded || got.Delivery == nil || got.Delivery.Status != InboundDeliveryFailed {
		t.Fatalf("got %+v, want concluded failed", got)
	}
}

func TestInboundReceiptStoreConcludedEntriesExpireAfterRetention(t *testing.T) {
	now := time.Date(2026, 8, 28, 8, 24, 0, 0, time.UTC)
	s := newTestReceiptStore(&now)

	s.Begin("c", "ir-1-1")
	s.Conclude("c", "ir-1-1", PendingInboundDelivery("ir-1-1"))
	now = now.Add(inboundReceiptRetention - time.Second)
	if got := s.Lookup("c", "ir-1-1"); got.State != InboundReceiptConcluded {
		t.Fatalf("just inside retention: state = %q, want concluded", got.State)
	}
	now = now.Add(2 * time.Second)
	if got := s.Lookup("c", "ir-1-1"); got.State != InboundReceiptUnknown {
		t.Fatalf("past retention: state = %q, want unknown", got.State)
	}
}

// A pending record is never expired: the fan-out goroutine behind it is
// still running, and a record that vanished under it would let a later
// poll read "unknown" — "nobody is still trying" — for a send that then
// lands (codex r1 P2 #1). The adapter's own deadline is what bounds a
// wedged fan-out, and it records that as "state unknown", not as a loss.
func TestInboundReceiptStorePendingEntriesNeverExpire(t *testing.T) {
	now := time.Date(2026, 8, 28, 8, 24, 0, 0, time.UTC)
	s := newTestReceiptStore(&now)

	s.Begin("c", "ir-1-1")
	now = now.Add(48 * time.Hour)
	if got := s.Lookup("c", "ir-1-1"); got.State != InboundReceiptPending {
		t.Fatalf("two days later: state = %q, want still pending — the fan-out may still be running", got.State)
	}
	// And it still concludes normally afterwards.
	s.Conclude("c", "ir-1-1", PendingInboundDelivery("ir-1-1"))
	if got := s.Lookup("c", "ir-1-1"); got.State != InboundReceiptConcluded {
		t.Fatalf("late conclusion after a long pending: state = %q, want concluded", got.State)
	}
}

// A store full of PENDING records grows past capacity rather than evicting
// one: every pending record is backed by a live fan-out goroutine, so the
// count is already bounded by those, and evicting would turn a running
// send into "unknown" (codex r1 P2 #1).
func TestInboundReceiptStoreAllPendingGrowsRatherThanEvicts(t *testing.T) {
	now := time.Date(2026, 8, 28, 8, 24, 0, 0, time.UTC)
	s := newTestReceiptStore(&now)
	s.capacity = 2

	for _, id := range []string{"p-0", "p-1", "p-2", "p-3"} {
		s.Begin("c", id)
		now = now.Add(time.Second)
	}
	for _, id := range []string{"p-0", "p-1", "p-2", "p-3"} {
		if got := s.Lookup("c", id); got.State != InboundReceiptPending {
			t.Fatalf("%s: state = %q, want pending — a pending record was evicted to make room", id, got.State)
		}
	}
}

// The store owns its copies: a caller mutating the delivery it passed to
// Conclude, or the one it got back from Lookup, must not change what the
// next Lookup reports (codex r1 P2 #2).
func TestInboundReceiptStoreCopiesMembersInAndOut(t *testing.T) {
	now := time.Date(2026, 8, 28, 8, 24, 0, 0, time.UTC)
	s := newTestReceiptStore(&now)

	members := []InboundDeliveryMember{{SessionID: "s1", Status: InboundDeliveryDelivered, DeliveredBytes: 5, ExpectedBytes: 5}}
	s.Conclude("c", "ir-1-1", SummarizeInboundDelivery("ir-1-1", members))
	members[0].Status = InboundDeliveryFailed
	got := s.Lookup("c", "ir-1-1")
	if got.Delivery.Members[0].Status != InboundDeliveryDelivered {
		t.Fatalf("caller's later mutation reached the stored delivery: %+v", got.Delivery.Members[0])
	}
	got.Delivery.Members[0].Status = InboundDeliveryFailed
	if again := s.Lookup("c", "ir-1-1"); again.Delivery.Members[0].Status != InboundDeliveryDelivered {
		t.Fatalf("mutating a Lookup result changed the store: %+v", again.Delivery.Members[0])
	}
}

// Capacity bounds the CONCLUDED population only (codex r2 P2 #1): the
// oldest concluded records are shed when a conclusion pushes the count
// past it, and pending records are neither counted nor touched.
func TestInboundReceiptStoreCapBoundsConcludedOnly(t *testing.T) {
	now := time.Date(2026, 8, 28, 8, 24, 0, 0, time.UTC)
	s := newTestReceiptStore(&now)
	s.capacity = 2

	// Many pending records must not make a lone conclusion evictable.
	for _, id := range []string{"p-0", "p-1", "p-2"} {
		s.Begin("c", id)
		now = now.Add(time.Second)
	}
	s.Begin("c", "done-0")
	s.Conclude("c", "done-0", PendingInboundDelivery("done-0"))
	now = now.Add(time.Second)
	s.Begin("c", "p-3")
	if got := s.Lookup("c", "done-0"); got.State != InboundReceiptConcluded {
		t.Fatalf("a Begin evicted the only concluded record with pending ones around: %+v", got)
	}

	// Conclusions past capacity shed the oldest CONCLUDED record — even
	// when it is a pending record concluding, which Begin never counted.
	s.Conclude("c", "p-0", PendingInboundDelivery("p-0"))
	now = now.Add(time.Second)
	s.Conclude("c", "p-1", PendingInboundDelivery("p-1"))
	if got := s.Lookup("c", "done-0"); got.State != InboundReceiptUnknown {
		t.Fatalf("oldest concluded record survived a third conclusion under capacity 2: %+v", got)
	}
	for _, id := range []string{"p-0", "p-1"} {
		if got := s.Lookup("c", id); got.State != InboundReceiptConcluded {
			t.Fatalf("%s: state = %q, want concluded (within capacity)", id, got.State)
		}
	}
	for _, id := range []string{"p-2", "p-3"} {
		if got := s.Lookup("c", id); got.State != InboundReceiptPending {
			t.Fatalf("%s: state = %q, want pending — capacity must never touch a pending record", id, got.State)
		}
	}
}

// A receipt is answered only through the city it was issued for: ids are
// enumerable and a concluded delivery names session ids and error text,
// which one city must not read off another (codex r4 P2).
func TestInboundReceiptStoreLookupIsCityScoped(t *testing.T) {
	now := time.Date(2026, 8, 28, 8, 24, 0, 0, time.UTC)
	s := newTestReceiptStore(&now)

	s.Begin("citadel", "ir-1-1")
	if got := s.Lookup("other", "ir-1-1"); got.State != InboundReceiptUnknown {
		t.Fatalf("another city's lookup: state = %q, want unknown", got.State)
	}
	if got := s.Lookup("citadel", "ir-1-1"); got.State != InboundReceiptPending {
		t.Fatalf("own city's lookup: state = %q, want pending", got.State)
	}
	s.Conclude("citadel", "ir-1-1", FailedInboundDelivery("ir-1-1", "nudge lock timeout"))
	if got := s.Lookup("other", "ir-1-1"); got.State != InboundReceiptUnknown || got.Delivery != nil {
		t.Fatalf("another city's lookup after conclusion: %+v, want unknown with no delivery", got)
	}
	if got := s.Lookup("citadel", "ir-1-1"); got.State != InboundReceiptConcluded {
		t.Fatalf("own city's lookup after conclusion: state = %q, want concluded", got.State)
	}
}
