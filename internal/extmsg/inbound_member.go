package extmsg

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

// InboundMemberEvidence is everything the send path learned about ONE
// member's nudge, gathered in one place so that exactly one function —
// [ClassifyInboundMember] — turns it into a receipt row.
type InboundMemberEvidence struct {
	// NudgeDelivery is the runtime's evidence, passed through unchanged from
	// the send path — including when the send path also returned an error
	// (see [runtime.NudgeDelivery]: an error never erases evidence). Submit
	// == "" means the runtime did not vouch: Delivered is then the
	// historical "no error" reading and Bytes is not a count. Its Landed
	// method is the one definition of "something reached the terminal".
	runtime.NudgeDelivery
	// ExpectedBytes is the size of the reminder gc built for this member.
	ExpectedBytes int
	// Err is the send path's own failure, if any. It is read AFTER the
	// evidence: a runtime that errored after the paste landed still put the
	// payload in the terminal, and "failed" would license a redelivery on
	// top of it.
	Err error
	// Undelivered is the runtime's reason for a live delivery that did not
	// happen without an error (a downgrade to the queue), when it gave one.
	Undelivered string
}

// InboundMemberOutcome is the verdict on one member: the status and delivered
// byte count that go on the receipt row, and the reason a non-delivered row
// carries (empty on delivered).
type InboundMemberOutcome struct {
	Status         InboundDeliveryStatus
	DeliveredBytes int
	Reason         string
}

// ClassifyInboundMember is the ONLY place a member's send result becomes a
// receipt status. Every consumer of the receipt acts on the word this returns,
// and the consumer this exists for (the Slack adapter's same-ts twin dedup,
// gp-32q) treats the words as promises:
//
//	delivered  the complete payload is in the terminal — a twin may skip.
//	pending    gc handed something to the terminal but cannot conclude — HOLD,
//	           never redeliver; a redelivery here is a duplicate.
//	partial    part of the payload is in the terminal — a redelivery repairs
//	           it at the cost of a duplicated fragment.
//	failed     nothing reached the terminal — a redelivery is clean.
//
// The one question asked first is [runtime.NudgeDelivery.Landed]: did the
// runtime say anything reached the terminal? When it did not, the row is
// failed and the error (or the downgrade reason) is why. When it did, "failed"
// is unreachable — every remaining row is delivered, pending, or partial —
// and the send path's error, if any, only turns a would-be delivered row into
// a hold. The table, in evaluation order (the table test is the spec):
//
//	not landed, send path errored                      → failed, 0 bytes, the error
//	not landed, no error                               → failed, 0 bytes, "not delivered live[: reason]"
//	landed, runtime did not vouch (Submit == "")       → delivered, expected bytes (pre-receipt reading);
//	                                                     pending if the send path also errored
//	landed, vouched, 0 < bytes < expected              → partial, its bytes (truncated; whatever Delivered says)
//	landed, vouched, bytes > expected                  → pending, its bytes (contradiction; hold)
//	landed, vouched, no bytes (Delivered flag only)    → pending, 0 bytes (contradiction; hold)
//	landed, vouched, whole, send path errored          → pending, its bytes (landed, then failed; hold)
//	landed, vouched, whole, submit unconfirmed         → pending, its bytes ← the 2026-08-28 incident
//	landed, vouched, whole, unknown submit state       → pending, its bytes (landed; hold rather than guess)
//	landed, vouched, whole, confirmed or unverified    → delivered, its bytes
//
// A vouching runtime's byte count is read before its Delivered flag and
// before its submit verdict: positive bytes are proof that at least that much
// is in the terminal, and a truncated paste is truncated whether or not the
// agent took it.
//
// "Unconfirmed" is the row this function was written for (gp-2io). The tmux
// runtime pastes the payload, sends Enter, and then watches for the agent's
// busy indicator; when the indicator never appears — or the Enter itself
// could not be delivered after the paste — the paste is in the pane but the
// agent has not been seen taking the turn. Until 2026-08-28 that came back
// as an error and was classified as failed with 0 bytes, and the adapter did
// what failed licenses: it re-posted, six times, a message the mayor had
// received whole every time. The payload is in the terminal and the outcome
// is not concluded — that is pending by definition.
func ClassifyInboundMember(e InboundMemberEvidence) InboundMemberOutcome {
	if !e.Landed() {
		if e.Err != nil {
			return InboundMemberOutcome{Status: InboundDeliveryFailed, Reason: e.Err.Error()}
		}
		reason := "not delivered live"
		if e.Undelivered != "" {
			reason += ": " + e.Undelivered
		}
		return InboundMemberOutcome{Status: InboundDeliveryFailed, Reason: reason}
	}

	// Landed: from here on "failed" is unreachable. The send path's error, if
	// any, is context on the row and turns a would-be delivered row into a
	// hold; it never becomes a promise that a redelivery is clean.
	withErr := func(reason string) string {
		if e.Err == nil {
			return reason
		}
		if reason == "" {
			return "send path errored after the paste landed: " + e.Err.Error()
		}
		return reason + "; send path errored: " + e.Err.Error()
	}
	hold := func(bytes int, reason string) InboundMemberOutcome {
		return InboundMemberOutcome{Status: InboundDeliveryPending, DeliveredBytes: bytes, Reason: withErr(reason)}
	}

	if e.Submit == "" {
		// The runtime did not vouch. Delivered is the pre-receipt "no error"
		// reading and the runtime counted nothing, so the reminder's own
		// length is the only size on offer. This is the trust boundary every
		// caller of Provider.Nudge has always relied on; it is not new here.
		if e.Err != nil {
			return hold(e.ExpectedBytes, "")
		}
		return InboundMemberOutcome{Status: InboundDeliveryDelivered, DeliveredBytes: e.ExpectedBytes}
	}

	// Vouched: the byte count is evidence and is read first.
	switch {
	case e.Bytes > 0 && e.Bytes < e.ExpectedBytes:
		return InboundMemberOutcome{
			Status:         InboundDeliveryPartial,
			DeliveredBytes: e.Bytes,
			Reason:         withErr(fmt.Sprintf("short paste: %d of %d bytes reached the terminal", e.Bytes, e.ExpectedBytes)),
		}
	case e.Bytes > e.ExpectedBytes:
		// More than gc built cannot have reached the terminal. The runtime
		// still says something landed, so a redelivery would duplicate: hold.
		return hold(e.Bytes, fmt.Sprintf("runtime counted %d bytes for a %d-byte payload", e.Bytes, e.ExpectedBytes))
	case e.Bytes <= 0:
		// Landed on the flag alone: a contradiction the runtime should never
		// produce (tmux errors on a short write and refuses an empty paste).
		// It still said the payload landed, so a redelivery would duplicate:
		// hold.
		return hold(0, "runtime vouched for the delivery but counted no bytes")
	}

	// The whole payload is in the terminal.
	if e.Err != nil {
		// Landed, then the send path failed (a submit that could not be
		// delivered, a client that closed). The payload is in the composer;
		// the outcome is not concluded.
		return hold(e.Bytes, "pasted whole")
	}
	switch e.Submit {
	case runtime.NudgeSubmitConfirmed, runtime.NudgeSubmitUnverified:
		return InboundMemberOutcome{Status: InboundDeliveryDelivered, DeliveredBytes: e.Bytes}
	case runtime.NudgeSubmitUnconfirmed:
		return hold(e.Bytes, "pasted whole; submit not confirmed (busy state never observed) — the agent has not been seen taking the turn; hold, do not redeliver")
	default:
		// A submit state this gc does not know. The payload landed, so a
		// redelivery would duplicate; hold rather than guess.
		return hold(e.Bytes, fmt.Sprintf("runtime reported an unknown submit state %q", e.Submit))
	}
}
