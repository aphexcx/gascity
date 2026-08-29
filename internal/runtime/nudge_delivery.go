package runtime

import (
	"errors"
	"fmt"
)

// NudgeSubmit is a vouching runtime's verdict on the submit keypress that
// follows a pasted nudge. It is a separate axis from delivery on purpose: the
// payload reaching the terminal and the agent taking the turn are two
// different facts, and the 2026-08-28 duplicate storm (gp-2io) came from
// collapsing them — a submit that could not be confirmed was reported as a
// delivery that never happened, and the consumer did what "never happened"
// licenses: it sent the message again.
type NudgeSubmit string

const (
	// NudgeSubmitConfirmed means the agent was observed taking the turn (its
	// busy indicator appeared after the submit). The strongest claim a runtime
	// makes.
	NudgeSubmitConfirmed NudgeSubmit = "confirmed"
	// NudgeSubmitUnconfirmed means the payload is in the pane — the receipt
	// vouches for that — but the runtime could not establish that the agent
	// accepted it: either a busy probe ran and never saw the agent go busy,
	// or the submit key sequence itself could not be delivered after the
	// paste. That is "not yet": the agent may be mid-turn with the paste
	// queued, or the draft may be sitting unsubmitted. A consumer must HOLD
	// on this, never redeliver; a redelivery duplicates the message in the
	// first case, and the runtime's own retry path (ErrNudgeSubmitUnconfirmed
	// from Nudge) already covers the second.
	NudgeSubmitUnconfirmed NudgeSubmit = "unconfirmed"
	// NudgeSubmitUnverified means no busy probe exists for this transport or
	// provider family, so the submit was sent best-effort and nothing was
	// checked. This is the ordinary outcome for every family without a
	// readable busy indicator and for the hidden-attach transport; it has
	// always counted as delivery and still does.
	NudgeSubmitUnverified NudgeSubmit = "unverified"
)

// ErrNudgeSubmitUnconfirmed is the retry signal for callers that have no
// evidence channel: the payload was handed to the session but the submit
// could not be confirmed, so the message may be sitting drafted-but-
// unsubmitted in the pane. A retry-capable caller (the nudge queue
// dispatcher, the idle-claim backstop) must treat it like an undelivered
// nudge — leave the item unacked so it requeues after the normal retry delay
// and spends one of its bounded attempts (ga-bwm proved that acking an
// unconfirmed submit is what lets a stalled nudge go undetected for many
// minutes).
//
// It is NOT a delivery-failure claim. Callers that carry evidence
// ([NudgeDelivery]) must never derive "nothing landed" from it; see
// [NudgeDelivery.UnconfirmedSubmitError] for the one place the translation
// is made.
var ErrNudgeSubmitUnconfirmed = errors.New("nudge: submit Enter delivered to tmux but not confirmed (busy state never observed)")

// NudgeDelivery is a vouching runtime's evidence about ONE nudge. It is the
// typed answer behind [NudgeVouchingProvider]: the fields are independent
// observations, not projections of one another, so a consumer classifying the
// outcome reads each on its own — through [NudgeDelivery.Landed] for the one
// question every consumer asks.
//
// The zero value means "nothing was delivered and the runtime has nothing to
// add", which is the honest answer for a session that no longer exists.
//
// An error never erases evidence. A runtime that learns the paste landed and
// then fails (the submit sequence, a client that closed mid-write) returns
// the non-zero delivery TOGETHER with the error, and every layer between the
// runtime and a consumer passes both through untouched. A consumer that
// wants a retry signal reads the error; a consumer that must not duplicate
// (the inbound delivery receipt) reads the evidence first, because an error
// that zeroed a landed payload is exactly what turns "hold" into a clean
// re-post of a message already in the terminal.
type NudgeDelivery struct {
	// Delivered reports that a paste was handed to the session's terminal.
	// It is the summary flag; Bytes says how much of the payload that paste
	// carried. On tmux a paste either lands whole or errors, so Delivered
	// with Bytes short of the payload is not a state that runtime produces —
	// but a consumer must still read Bytes rather than infer completeness
	// from this flag. Implementations must never set it unless a paste
	// actually reached the session.
	Delivered bool
	// Bytes is how much of the payload the runtime handed over — the number
	// its own receipt carries. It is evidence only when Submit is set: a
	// runtime that did not vouch (Submit == "") counted nothing, and a
	// consumer must not read its zero as "zero bytes landed". When Submit is
	// set, a positive Bytes is proof that at least that much is in the
	// terminal, whatever Delivered says.
	Bytes int
	// Submit is the runtime's verdict on the submit keypress. Empty when the
	// runtime made no claim at all (Delivered was inferred from a nil error
	// by a caller that only had [Provider.Nudge]); otherwise one of the
	// NudgeSubmit constants.
	Submit NudgeSubmit
}

// Landed is the ONE definition of "the runtime says something reached the
// terminal", shared by every consumer of this evidence — the inbound
// receipt's classifier, the retry-signal translation below, and the ack
// sites that decide whether a nudge is live-delivered. It is true when the
// runtime raised the Delivered flag, or when a vouching runtime (Submit set)
// counted a positive number of pasted bytes: the count is proof on its own,
// whatever the flag says. A runtime that did not vouch (Submit == "") has no
// count to read, so only its flag speaks.
//
// Two consumers reading "landed" from two different operands was the shape
// of the second gp-2io gate failure: the classifier trusted the byte count
// while the ack path trusted the flag, so evidence the classifier would hold
// on (a whole live copy) could be released and re-pasted by the queue.
func (d NudgeDelivery) Landed() bool {
	return d.Delivered || (d.Submit != "" && d.Bytes > 0)
}

// UnconfirmedSubmitError is the ONE translation from delivery evidence to
// the retry signal that callers without an evidence channel rely on: a
// non-nil [ErrNudgeSubmitUnconfirmed] when the payload landed ([Landed]) but
// the submit is unconfirmed, nil otherwise. tmux's NudgeSession/NudgeNow
// wrappers apply it on the way out, and every consumer that acks, commits,
// or reports success on a delivery applies it before doing so.
//
// It is never applied on the evidence path (the inbound delivery receipt):
// there the same outcome classifies as pending — landed, hold, do not
// redeliver — which is the whole point of carrying the evidence (gp-2io).
func (d NudgeDelivery) UnconfirmedSubmitError(target string) error {
	if d.Landed() && d.Submit == NudgeSubmitUnconfirmed {
		return fmt.Errorf("%w: session %q", ErrNudgeSubmitUnconfirmed, target)
	}
	return nil
}
