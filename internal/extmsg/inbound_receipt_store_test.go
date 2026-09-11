package extmsg

import (
	"strconv"
	"sync"
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

// The message index behind BeginFor/LatestFor: the evidence a redelivery of
// an inbound message is judged against (see the api package's
// extmsgClaimInboundFanout). It follows the LATEST fan-out for the message,
// is scoped to the city, and never outlives the record it points at.
func TestInboundReceiptStoreLatestForFollowsMessageIndex(t *testing.T) {
	now := time.Date(2026, 9, 11, 8, 24, 0, 0, time.UTC)
	s := newTestReceiptStore(&now)
	ref := ConversationRef{ScopeID: "g", Provider: "slack", AccountID: "a", ConversationID: "C1", Kind: ConversationRoom}
	key := InboundMessageKey(ref, "1785354286.000100")
	if key == "" {
		t.Fatal("message with a provider id produced no key")
	}
	if InboundMessageKey(ref, "  ") != "" {
		t.Fatal("message without a provider id produced a key — it must keep at-least-once delivery")
	}
	// Parity with the transcript dedup: a ref that sameConversationRef calls
	// the same conversation — every field perturbed the way
	// normalizeConversationRef undoes (padding on all six, case on provider
	// and kind) — must produce the same key, and so must a padded id.
	shouty := ConversationRef{
		ScopeID:              " g ",
		Provider:             " Slack ",
		AccountID:            "\ta\t",
		ConversationID:       " C1 ",
		ParentConversationID: "  ",
		Kind:                 " ROOM ",
	}
	if !sameConversationRef(ref, shouty) {
		t.Fatal("test premise: the transcript dedup does not normalize the perturbed ref back to ref")
	}
	if InboundMessageKey(shouty, " 1785354286.000100 ") != key {
		t.Fatal("key is not normalized the way the transcript dedup normalizes the conversation")
	}

	if got := s.LatestFor("c", key); got.State != InboundReceiptUnknown {
		t.Fatalf("before any fan-out: %+v, want unknown", got)
	}
	s.BeginFor("c", "ir-1-1", key)
	if got := s.LatestFor("c", key); got.State != InboundReceiptPending || got.ReceiptID != "ir-1-1" {
		t.Fatalf("after BeginFor: %+v, want pending on ir-1-1", got)
	}
	if got := s.LatestFor("other", key); got.State != InboundReceiptUnknown {
		t.Fatalf("another city's lookup: %+v, want unknown", got)
	}
	s.Conclude("c", "ir-1-1", FailedInboundDelivery("ir-1-1", "runtime dead"))
	if got := s.LatestFor("c", key); got.State != InboundReceiptConcluded || got.Delivery == nil || got.Delivery.Status != InboundDeliveryFailed {
		t.Fatalf("after a failed conclusion: %+v, want concluded failed", got)
	}

	// A later fan-out for the same message supersedes the first.
	s.BeginFor("c", "ir-1-2", key)
	if got := s.LatestFor("c", key); got.State != InboundReceiptPending || got.ReceiptID != "ir-1-2" {
		t.Fatalf("after a second BeginFor: %+v, want pending on ir-1-2", got)
	}

	// Retention drops the superseded record without disturbing the index,
	// which now belongs to ir-1-2.
	now = now.Add(inboundReceiptRetention + time.Second)
	s.Conclude("c", "ir-1-2", SummarizeInboundDelivery("ir-1-2", []InboundDeliveryMember{{
		SessionID: "s1", Status: InboundDeliveryDelivered, DeliveredBytes: 5, ExpectedBytes: 5,
	}}))
	if got := s.Lookup("c", "ir-1-1"); got.State != InboundReceiptUnknown {
		t.Fatalf("superseded record survived retention: %+v", got)
	}
	if got := s.LatestFor("c", key); got.ReceiptID != "ir-1-2" || got.Delivery == nil || got.Delivery.Status != InboundDeliveryDelivered {
		t.Fatalf("after the superseded record aged out: %+v, want ir-1-2 delivered", got)
	}

	// Once the latest record ages out too, the message is unknown again and
	// the index entry is gone with it.
	now = now.Add(inboundReceiptRetention + time.Second)
	if got := s.LatestFor("c", key); got.State != InboundReceiptUnknown {
		t.Fatalf("after the latest record aged out: %+v, want unknown", got)
	}
	s.mu.Lock()
	_, leaked := s.byMessage[messageIndexKey("c", key)]
	s.mu.Unlock()
	if leaked {
		t.Fatal("message index outlived the record it pointed at")
	}
}

// The message key carries the WHOLE normalized conversation identity the
// transcript dedup compares (sameConversationRef), not just the provider /
// account / conversation id triple: two conversations that differ only in
// scope, parent conversation or kind are different conversations to the
// transcript, and their receipts must not stand in for each other (codex
// r3 MAJOR 2) — a redelivery judged on another conversation's delivery
// would be suppressed with nobody in its own conversation holding the
// message, and one judged on another's failure would be duplicated.
func TestInboundMessageKeyCoversWholeConversationIdentity(t *testing.T) {
	now := time.Date(2026, 9, 11, 8, 24, 0, 0, time.UTC)
	s := newTestReceiptStore(&now)
	const id = "1785354286.000100"
	base := ConversationRef{ScopeID: "g", Provider: "slack", AccountID: "a", ConversationID: "C1", Kind: ConversationRoom}
	baseKey := InboundMessageKey(base, id)

	for name, mutate := range map[string]func(*ConversationRef){
		"scope":               func(r *ConversationRef) { r.ScopeID = "g2" },
		"provider":            func(r *ConversationRef) { r.Provider = "discord" },
		"account":             func(r *ConversationRef) { r.AccountID = "a2" },
		"conversation":        func(r *ConversationRef) { r.ConversationID = "C2" },
		"parent conversation": func(r *ConversationRef) { r.ParentConversationID = "C0" },
		"kind":                func(r *ConversationRef) { r.Kind = ConversationThread },
	} {
		other := base
		mutate(&other)
		if InboundMessageKey(other, id) == baseKey {
			t.Fatalf("a conversation differing only in %s produced the same message key", name)
		}
		if !sameConversationRef(base, base) || sameConversationRef(base, other) {
			t.Fatalf("test premise: the transcript dedup does not distinguish %s", name)
		}
	}
	// Normalization matches the transcript's for the added fields too.
	shouty := base
	shouty.ScopeID, shouty.Kind, shouty.ParentConversationID = " g ", " ROOM ", "  "
	if InboundMessageKey(shouty, id) != baseKey {
		t.Fatal("key is not normalized the way the transcript dedup normalizes scope, kind and parent")
	}

	// The collision itself: the same provider message id delivered to a room
	// and to a thread hanging off it (or the same id under another kind)
	// keeps separate receipts.
	thread := base
	thread.ParentConversationID, thread.Kind = "C1", ConversationThread
	threadKey := InboundMessageKey(thread, id)
	s.BeginFor("c", "ir-1-room", baseKey)
	s.Conclude("c", "ir-1-room", SummarizeInboundDelivery("ir-1-room", []InboundDeliveryMember{{
		SessionID: "s1", Status: InboundDeliveryDelivered, DeliveredBytes: 5, ExpectedBytes: 5,
	}}))
	if got := s.LatestFor("c", threadKey); got.State != InboundReceiptUnknown {
		t.Fatalf("the room's delivered receipt answered for the thread: %+v", got)
	}
	s.BeginFor("c", "ir-1-thread", threadKey)
	s.Conclude("c", "ir-1-thread", FailedInboundDelivery("ir-1-thread", "runtime dead"))
	if got := s.LatestFor("c", baseKey); got.ReceiptID != "ir-1-room" || got.Delivery == nil || got.Delivery.Status != InboundDeliveryDelivered {
		t.Fatalf("the thread's failed receipt overwrote the room's delivery evidence: %+v", got)
	}
	if got := s.LatestFor("c", threadKey); got.ReceiptID != "ir-1-thread" || got.Delivery == nil || got.Delivery.Status != InboundDeliveryFailed {
		t.Fatalf("thread evidence = %+v, want its own failed receipt", got)
	}
	kindOnly := base
	kindOnly.Kind = ConversationDM
	if _, claimed := s.ClaimFor("c", "ir-1-dm", InboundMessageKey(kindOnly, id)); !claimed {
		t.Fatal("a claim for the same id under another kind was refused on the room's delivery")
	}
}

// ClaimFor is the atomic claim-or-lookup behind the api package's
// extmsgClaimInboundFanout: the lookup of a message's latest receipt and the
// registration of a new fan-out for it happen under one lock, so of any
// number of requests carrying the same message, exactly one claims while a
// whole copy is live or landed, and the rest read the claim they lost to
// (codex r3 MAJOR 1).
func TestInboundReceiptStoreClaimForIsAtomicPerMessage(t *testing.T) {
	now := time.Date(2026, 9, 11, 8, 24, 0, 0, time.UTC)
	s := newTestReceiptStore(&now)
	ref := ConversationRef{ScopeID: "g", Provider: "slack", AccountID: "a", ConversationID: "C1", Kind: ConversationRoom}
	key := InboundMessageKey(ref, "1785354286.000100")
	whole := func(id string) InboundDelivery {
		return SummarizeInboundDelivery(id, []InboundDeliveryMember{{SessionID: "s1", Status: InboundDeliveryDelivered, DeliveredBytes: 5, ExpectedBytes: 5}})
	}

	// No evidence: claimed, and registered as the message's pending fan-out
	// in the same step.
	prior, claimed := s.ClaimFor("c", "ir-1-1", key)
	if !claimed || prior.State != InboundReceiptUnknown {
		t.Fatalf("first claim: claimed=%v prior=%+v, want claimed on unknown", claimed, prior)
	}
	if got := s.LatestFor("c", key); got.State != InboundReceiptPending || got.ReceiptID != "ir-1-1" {
		t.Fatalf("after the first claim: %+v, want pending on ir-1-1", got)
	}
	// While it runs, a second claim is refused, reads the running claim, and
	// records nothing for the refused id.
	prior, claimed = s.ClaimFor("c", "ir-1-2", key)
	if claimed || prior.State != InboundReceiptPending || prior.ReceiptID != "ir-1-1" {
		t.Fatalf("claim during a running fan-out: claimed=%v prior=%+v, want refused with pending ir-1-1", claimed, prior)
	}
	if got := s.Lookup("c", "ir-1-2"); got.State != InboundReceiptUnknown {
		t.Fatalf("refused claim left a record: %+v", got)
	}

	// The evidence that licenses a retry lets the next claim through, which
	// then supersedes it as the message's latest fan-out.
	for i, prior := range []InboundDelivery{
		FailedInboundDelivery("", "runtime dead"),
		SummarizeInboundDelivery("", []InboundDeliveryMember{{SessionID: "s1", Status: InboundDeliveryPartial, DeliveredBytes: 2, ExpectedBytes: 5}}),
		SummarizeInboundDelivery("", nil),
	} {
		latest := s.LatestFor("c", key).ReceiptID
		prior.ReceiptID = latest
		s.Conclude("c", latest, prior)
		next := "ir-2-" + string(rune('a'+i))
		got, claimed := s.ClaimFor("c", next, key)
		if !claimed || got.State != InboundReceiptConcluded || got.Delivery == nil || got.Delivery.Status != prior.Status {
			t.Fatalf("claim after %s: claimed=%v prior=%+v, want claimed on that evidence", prior.Status, claimed, got)
		}
		if l := s.LatestFor("c", key); l.State != InboundReceiptPending || l.ReceiptID != next {
			t.Fatalf("after claiming on %s: latest = %+v, want pending on %s", prior.Status, l, next)
		}
	}

	// A whole copy live (concluded pending) or landed (delivered) refuses
	// every further claim.
	latest := s.LatestFor("c", key).ReceiptID
	s.Conclude("c", latest, SummarizeInboundDelivery(latest, []InboundDeliveryMember{{SessionID: "s1", Status: InboundDeliveryPending, DeliveredBytes: 5, ExpectedBytes: 5}}))
	if prior, claimed := s.ClaimFor("c", "ir-3-1", key); claimed || prior.ReceiptID != latest || prior.Delivery == nil || prior.Delivery.Status != InboundDeliveryPending {
		t.Fatalf("claim on a concluded-pending delivery: claimed=%v prior=%+v, want refused with that delivery", claimed, prior)
	}
	s.BeginFor("c", "ir-3-2", key)
	s.Conclude("c", "ir-3-2", whole("ir-3-2"))
	for _, id := range []string{"ir-3-3", "ir-3-4"} {
		if prior, claimed := s.ClaimFor("c", id, key); claimed || prior.ReceiptID != "ir-3-2" || prior.Delivery == nil || prior.Delivery.Status != InboundDeliveryDelivered {
			t.Fatalf("claim on a delivered message (%s): claimed=%v prior=%+v, want refused with the delivered receipt", id, claimed, prior)
		}
		if got := s.Lookup("c", id); got.State != InboundReceiptUnknown {
			t.Fatalf("refused claim %s left a record: %+v", id, got)
		}
	}

	// Scope and degenerate keys: another city has no evidence for the same
	// key; a message without a provider id is always claimed and never
	// indexed, so it keeps at-least-once delivery.
	if _, claimed := s.ClaimFor("other", "ir-4-1", key); !claimed {
		t.Fatal("another city's claim was refused on this city's evidence")
	}
	for _, id := range []string{"ir-5-1", "ir-5-2"} {
		if prior, claimed := s.ClaimFor("c", id, ""); !claimed || prior.State != InboundReceiptUnknown {
			t.Fatalf("claim without a message key (%s): claimed=%v prior=%+v, want claimed on unknown", id, claimed, prior)
		}
		if got := s.Lookup("c", id); got.State != InboundReceiptPending {
			t.Fatalf("keyless claim %s not recorded: %+v", id, got)
		}
	}

	// The property itself, under contention: many claims for one message
	// with no evidence, exactly one wins, every loser reads the winner.
	fresh := InboundMessageKey(ref, "1785354286.000200")
	var wg sync.WaitGroup
	winners := make(chan string, 64)
	losersSaw := make(chan string, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			prior, claimed := s.ClaimFor("c", id, fresh)
			if claimed {
				winners <- id
			} else {
				losersSaw <- prior.ReceiptID
			}
		}("ir-6-" + strconv.Itoa(i))
	}
	wg.Wait()
	close(winners)
	close(losersSaw)
	var won []string
	for id := range winners {
		won = append(won, id)
	}
	if len(won) != 1 {
		t.Fatalf("%d of 64 concurrent claims for one message succeeded, want exactly 1: %v", len(won), won)
	}
	n := 0
	for saw := range losersSaw {
		n++
		if saw != won[0] {
			t.Fatalf("a refused claim read receipt %q, want the winner %q", saw, won[0])
		}
	}
	if n != 63 {
		t.Fatalf("%d refused claims, want 63", n)
	}
}
