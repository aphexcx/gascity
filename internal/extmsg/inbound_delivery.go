package extmsg

import (
	"fmt"
	"os"
	"sync/atomic"
)

// InboundDeliveryStatus is the closed set of outcomes gc reports for an
// inbound message's fan-out to session terminals.
//
// The consumer this exists for is an adapter that must not deliver the same
// provider message twice (the Slack adapter's same-ts twin dedup, gp-32q): it
// commits its dedup claim only on an outcome that means "gc has this", and
// releases the claim otherwise so a retry path can take over.
type InboundDeliveryStatus string

const (
	// InboundDeliveryDelivered means every notified member took the COMPLETE
	// payload into its terminal. See [InboundDelivery] for the precise (and
	// deliberately narrow) meaning of "took".
	InboundDeliveryDelivered InboundDeliveryStatus = "delivered"
	// InboundDeliveryNoRoute means the fan-out had nobody to notify. This is a
	// terminal, non-retryable outcome: a redelivery of the same message would
	// resolve the same empty membership and also reach nobody, so a consumer
	// should commit rather than retry.
	InboundDeliveryNoRoute InboundDeliveryStatus = "no_route"
	// InboundDeliveryPartial means only FRAGMENTS of the message are live: at
	// least one member's paste was truncated, and no member holds a whole
	// copy (those combinations summarize as pending — see
	// [SummarizeInboundDelivery]). A redelivery duplicates at most a fragment
	// and is the only thing that repairs the truncation, so this is the one
	// something-landed state in which a consumer may re-post.
	InboundDeliveryPartial InboundDeliveryStatus = "partial"
	// InboundDeliveryFailed means no member took the payload, though at least
	// one was supposed to. Nothing was delivered, so a retry is clean.
	InboundDeliveryFailed InboundDeliveryStatus = "failed"
	// InboundDeliveryPending means the outcome is not concluded in a way a
	// redelivery could safely repair. It is NOT a failure claim and NOT a
	// success claim, and a consumer must HOLD on it — never re-post. It
	// covers three shapes that share that one property:
	//
	//   - the fan-out outran the response budget and the sends are still
	//     running (the whole-receipt PendingInboundDelivery);
	//   - a member's payload landed whole but the agent was not seen taking
	//     the turn (submit unconfirmed — the 2026-08-28 gp-2io incident,
	//     which classified as failed and licensed a 6x duplicate storm);
	//   - a mixed fan-out in which at least one member holds a whole live
	//     copy, so a fan-out-wide re-post would duplicate it.
	//
	// The duplicate window is real and unavoidable here: gc cannot cancel a
	// paste already handed to a terminal, so a pending send may land AFTER
	// the consumer has given up and retried. Documented rather than hidden,
	// because the alternative is guessing.
	InboundDeliveryPending InboundDeliveryStatus = "pending"
)

// InboundDeliveryMember is one session's delivery outcome inside the fan-out.
type InboundDeliveryMember struct {
	// SessionID is the resolved session the payload was addressed to.
	SessionID string `json:"session_id"`
	// Selector is the membership selector that resolved to SessionID, kept
	// because it is what an operator recognizes in a transcript membership
	// list when SessionID is an opaque id.
	Selector string                `json:"selector,omitempty"`
	Status   InboundDeliveryStatus `json:"status"`
	// DeliveredBytes/ExpectedBytes are for THIS member's reminder, which is
	// not the same string as any other member's — see [InboundDelivery].
	DeliveredBytes int `json:"delivered_bytes"`
	ExpectedBytes  int `json:"expected_bytes"`
	// Digest is the content address of this member's reminder
	// ([runtime.NudgePayloadDigest]). It appears verbatim in gc's own
	// "nudge-receipt ... digest=" log line, so an operator can tie a consumer's
	// log entry to the exact delivery inside gc during an incident. Nothing
	// gates on it.
	Digest string `json:"digest"`
	// Error is context for a non-delivered member, never a second copy of
	// Status. Empty on success.
	Error string `json:"error,omitempty"`
}

// InboundDelivery is gc's delivery receipt for one inbound message: evidence
// about what actually reached agent terminals, returned synchronously on the
// inbound response.
//
// WHY THIS EXISTS. Until 2026-08-28 the inbound 200 meant only "gc accepted
// the HTTP request" — the terminal fan-out ran in the background and its
// outcome was unobservable to the caller. That is how four founder messages
// could arrive at agents as truncated tails on 2026-08-27/28 while every
// adapter-side signal said success (pc_2e2378b9918e). A consumer gating a
// dedup claim on that 200 was gating on nothing.
//
// WHAT "delivered" MEANS — read this before trusting it. It means the runtime
// accepted the COMPLETE payload for that session. On the tmux runtime that is
// a strong claim: the payload goes in as ONE bracketed paste, so it cannot
// have been split into per-keystroke Enters, which is the truncation mechanism
// above.
//
// It does NOT mean the TUI rendered it, and it does NOT mean the agent read or
// acted on it. gc has no truthful signal for either, and a receipt that
// overclaims is worse for a consumer than one that underclaims.
//
// WHY THE BYTE COUNTS ARE SUMS. One inbound notifies every transcript member,
// and the reminder text is built per member: it can embed the recipient's
// handle and, for a message addressed elsewhere, a "do not reply"
// discriminator, so two members' reminders may differ in size (and often do
// not — identical reminders are equally normal). Either way there is no single
// expected size for the message, so the top-level counts are summed across
// members, which keeps the useful invariant intact:
// DeliveredBytes == ExpectedBytes exactly when everything gc built reached a
// terminal. Per-member truth is in Members.
//
// ABSENCE IS MEANINGFUL. A gc build that cannot vouch for delivery omits this
// object entirely rather than sending it with empty fields, so a consumer can
// distinguish "this gc predates receipts, fail open" from "this gc says it was
// not delivered, do not commit".
type InboundDelivery struct {
	// ReceiptID identifies this fan-out, so two deliveries of identical text
	// are distinguishable in logs. Non-empty whenever the object is present.
	ReceiptID string                `json:"receipt_id"`
	Status    InboundDeliveryStatus `json:"status"`
	// DeliveredBytes is summed over members; see the type doc. 0 for no_route.
	DeliveredBytes int                     `json:"delivered_bytes"`
	ExpectedBytes  int                     `json:"expected_bytes"`
	Members        []InboundDeliveryMember `json:"members,omitempty"`
}

// SummarizeInboundDelivery folds per-member outcomes into the aggregate
// status. Split out from the fan-out so the precedence rules below are
// testable without a terminal.
//
// Precedence is chosen so the aggregate NEVER reads as more certain than the
// members justify: "delivered" requires unanimity, and any unconcluded member
// downgrades the whole result. A consumer acting on the aggregate alone is
// therefore always acting on a floor, not a guess.
func SummarizeInboundDelivery(receiptID string, members []InboundDeliveryMember) InboundDelivery {
	out := InboundDelivery{ReceiptID: receiptID, Members: members}
	if len(members) == 0 {
		// Nobody to notify. Distinct from "failed": there is nothing a retry
		// could reach, so the consumer should commit, not redeliver.
		out.Status = InboundDeliveryNoRoute
		return out
	}
	delivered, partial, pending := 0, 0, 0
	for _, m := range members {
		out.DeliveredBytes += m.DeliveredBytes
		out.ExpectedBytes += m.ExpectedBytes
		switch m.Status {
		case InboundDeliveryDelivered:
			delivered++
		case InboundDeliveryPartial:
			partial++
		case InboundDeliveryPending:
			pending++
		}
	}
	switch {
	case delivered == len(members):
		out.Status = InboundDeliveryDelivered
	case delivered > 0 || pending > 0:
		// A WHOLE copy of the payload is live in a delivered member's
		// terminal, or landed/unconcluded in a pending one. The only repair a
		// consumer has is a fan-out-wide redelivery, and that would duplicate
		// the whole copy — the worse failure (a truncated message looks wrong
		// when caught; a duplicated one reads as the sender saying it twice).
		// So any such mix must HOLD: partial and failed both license the
		// re-post and are unreachable from here, no matter what the other
		// members report.
		out.Status = InboundDeliveryPending
	case partial > 0:
		// Only truncated fragments and failures — no member holds a whole
		// copy. A redelivery duplicates at most a fragment and is the only
		// thing that repairs the truncation, so this is the one mixed state
		// that may invite it. A lone partial member lands here too; it must
		// never fall through to failed, which promises a clean retry.
		out.Status = InboundDeliveryPartial
	default:
		// Every member concluded and nothing landed anywhere.
		out.Status = InboundDeliveryFailed
	}
	return out
}

// String renders the delivery as one greppable log field set, mirroring the
// runtime's own "nudge-receipt" line so both ends of a delivery can be found
// with one search.
func (d InboundDelivery) String() string {
	return fmt.Sprintf("inbound-delivery receipt=%s status=%s delivered_bytes=%d expected_bytes=%d members=%d",
		d.ReceiptID, d.Status, d.DeliveredBytes, d.ExpectedBytes, len(d.Members))
}

var inboundReceiptSeq uint64

// NextInboundReceiptID mints an id for one inbound fan-out. The pid keeps ids
// distinct across the several gc processes that can serve one city, so an id
// in a consumer's log names exactly one delivery.
func NextInboundReceiptID() string {
	seq := atomic.AddUint64(&inboundReceiptSeq, 1)
	return fmt.Sprintf("ir-%d-%d", os.Getpid(), seq)
}

// PendingInboundDelivery is the receipt for a fan-out that had not concluded
// when the response had to be written.
//
// It deliberately carries no members and no byte counts: at this point gc does
// not know them, and emitting zeros would let a consumer read "0 of 0 bytes
// delivered" as a completed empty delivery. Status is the whole message.
func PendingInboundDelivery(receiptID string) InboundDelivery {
	return InboundDelivery{ReceiptID: receiptID, Status: InboundDeliveryPending}
}

// FailedInboundDelivery is the receipt for a fan-out that could not be carried
// out at all — the membership lookup failed, or the services behind it were
// unavailable.
//
// This exists so such a failure cannot borrow the shape of a successful
// zero-member fan-out. Both have no members, but no_route means "there was
// nobody to tell, and a retry would find nobody either", which invites a
// consumer to commit and drop the message. A lookup that failed says nothing
// about who the recipients were, so it must read as a failure a retry can fix.
func FailedInboundDelivery(receiptID, reason string) InboundDelivery {
	return InboundDelivery{
		ReceiptID: receiptID,
		Status:    InboundDeliveryFailed,
		Members: []InboundDeliveryMember{{
			Status: InboundDeliveryFailed,
			Error:  reason,
		}},
	}
}
