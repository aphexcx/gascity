package extmsg

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestClassifyInboundMemberTable is the spec for the one function that turns
// a member's send result into a receipt status. One row per outcome the send
// path can produce, with the verdict the consumer is entitled to act on.
//
// The row that matters most is "landed whole, submit unconfirmed": on
// 2026-08-28 that outcome was reported as failed with 0 bytes, which licensed
// the Slack adapter's clean re-post, six times, for one founder message that
// the mayor had received whole every time (gp-2io).
func TestClassifyInboundMemberTable(t *testing.T) {
	const expected = 1215 // the incident payload's size
	sendErr := errors.New("sending message to session: nudge lock timeout for session \"gc__mayor\"")
	evidence := func(delivered bool, bytes int, submit runtime.NudgeSubmit) runtime.NudgeDelivery {
		return runtime.NudgeDelivery{Delivered: delivered, Bytes: bytes, Submit: submit}
	}

	for _, tc := range []struct {
		name     string
		evidence InboundMemberEvidence
		status   InboundDeliveryStatus
		bytes    int
		reason   string // substring the reason must contain; "" means the reason must be empty
	}{
		// ---- nothing landed: failed, and the error or downgrade reason is why
		{
			name:     "send path errored before anything landed: a redelivery is clean",
			evidence: InboundMemberEvidence{ExpectedBytes: expected, Err: sendErr},
			status:   InboundDeliveryFailed,
			bytes:    0,
			reason:   sendErr.Error(),
		},
		{
			name:     "runtime says nothing landed (session gone)",
			evidence: InboundMemberEvidence{ExpectedBytes: expected},
			status:   InboundDeliveryFailed,
			bytes:    0,
			reason:   "not delivered live",
		},
		{
			name:     "runtime says nothing landed and names the downgrade",
			evidence: InboundMemberEvidence{ExpectedBytes: expected, Undelivered: "no_idle_boundary"},
			status:   InboundDeliveryFailed,
			bytes:    0,
			reason:   "not delivered live: no_idle_boundary",
		},
		{
			name:     "vouched, nothing delivered, no bytes: nothing landed, a redelivery is clean",
			evidence: InboundMemberEvidence{ExpectedBytes: expected, NudgeDelivery: evidence(false, 0, runtime.NudgeSubmitUnconfirmed), Undelivered: "session_not_live"},
			status:   InboundDeliveryFailed,
			bytes:    0,
			reason:   "not delivered live: session_not_live",
		},
		{
			name:     "vouched, nothing delivered, no bytes, send path errored: the error is the reason",
			evidence: InboundMemberEvidence{ExpectedBytes: expected, NudgeDelivery: evidence(false, 0, runtime.NudgeSubmitUnverified), Err: sendErr},
			status:   InboundDeliveryFailed,
			bytes:    0,
			reason:   sendErr.Error(),
		},
		// ---- the runtime did not vouch: the pre-receipt reading
		{
			name:     "runtime did not vouch: the pre-receipt no-error reading, sized by what gc built",
			evidence: InboundMemberEvidence{ExpectedBytes: expected, NudgeDelivery: evidence(true, 0, "")},
			status:   InboundDeliveryDelivered,
			bytes:    expected,
			reason:   "",
		},
		{
			name:     "runtime did not vouch but flagged delivered AND the send path errored: contradiction, hold",
			evidence: InboundMemberEvidence{ExpectedBytes: expected, NudgeDelivery: evidence(true, 0, ""), Err: sendErr},
			status:   InboundDeliveryPending,
			bytes:    expected,
			reason:   sendErr.Error(),
		},
		// ---- vouched: the byte count is read first
		{
			name:     "landed short, submit confirmed: truncated, only a redelivery repairs it",
			evidence: InboundMemberEvidence{ExpectedBytes: expected, NudgeDelivery: evidence(true, 41, runtime.NudgeSubmitConfirmed)},
			status:   InboundDeliveryPartial,
			bytes:    41,
			reason:   "short paste: 41 of 1215 bytes",
		},
		{
			name:     "landed short, submit unconfirmed: truncation outranks the hold",
			evidence: InboundMemberEvidence{ExpectedBytes: expected, NudgeDelivery: evidence(true, 41, runtime.NudgeSubmitUnconfirmed)},
			status:   InboundDeliveryPartial,
			bytes:    41,
			reason:   "short paste",
		},
		{
			name:     "vouched short paste with the complete-payload flag down: the count is the evidence, partial",
			evidence: InboundMemberEvidence{ExpectedBytes: expected, NudgeDelivery: evidence(false, 41, runtime.NudgeSubmitUnverified)},
			status:   InboundDeliveryPartial,
			bytes:    41,
			reason:   "short paste: 41 of 1215 bytes",
		},
		{
			name:     "vouched short paste AND the send path errored (hidden-attach client closed mid-write): partial, error kept as context",
			evidence: InboundMemberEvidence{ExpectedBytes: expected, NudgeDelivery: evidence(true, 41, runtime.NudgeSubmitUnconfirmed), Err: sendErr},
			status:   InboundDeliveryPartial,
			bytes:    41,
			reason:   "short paste: 41 of 1215 bytes reached the terminal; send path errored: " + sendErr.Error(),
		},
		{
			name:     "runtime counted more than gc built: a contradiction, but something landed — hold",
			evidence: InboundMemberEvidence{ExpectedBytes: expected, NudgeDelivery: evidence(true, expected+2, runtime.NudgeSubmitConfirmed)},
			status:   InboundDeliveryPending,
			bytes:    expected + 2,
			reason:   "counted 1217 bytes for a 1215-byte payload",
		},
		{
			name:     "vouched with no byte count: a contradiction, but the runtime said it landed — hold",
			evidence: InboundMemberEvidence{ExpectedBytes: expected, NudgeDelivery: evidence(true, 0, runtime.NudgeSubmitConfirmed)},
			status:   InboundDeliveryPending,
			bytes:    0,
			reason:   "counted no bytes",
		},
		// ---- vouched, whole payload in the terminal
		{
			name:     "landed whole, agent seen taking the turn",
			evidence: InboundMemberEvidence{ExpectedBytes: expected, NudgeDelivery: evidence(true, expected, runtime.NudgeSubmitConfirmed)},
			status:   InboundDeliveryDelivered,
			bytes:    expected,
			reason:   "",
		},
		{
			name:     "landed whole on a family with no busy probe: best-effort has always counted as delivery",
			evidence: InboundMemberEvidence{ExpectedBytes: expected, NudgeDelivery: evidence(true, expected, runtime.NudgeSubmitUnverified)},
			status:   InboundDeliveryDelivered,
			bytes:    expected,
			reason:   "",
		},
		{
			name:     "vouched whole paste with the complete-payload flag down: the count is the evidence, delivered",
			evidence: InboundMemberEvidence{ExpectedBytes: expected, NudgeDelivery: evidence(false, expected, runtime.NudgeSubmitConfirmed)},
			status:   InboundDeliveryDelivered,
			bytes:    expected,
			reason:   "",
		},
		{
			name:     "landed whole, submit unconfirmed (busy state never observed): the 2026-08-28 incident — hold with the full count",
			evidence: InboundMemberEvidence{ExpectedBytes: expected, NudgeDelivery: evidence(true, expected, runtime.NudgeSubmitUnconfirmed)},
			status:   InboundDeliveryPending,
			bytes:    expected,
			reason:   "submit not confirmed",
		},
		{
			name:     "landed whole with the flag down, submit unconfirmed: the same incident read from the count alone — hold",
			evidence: InboundMemberEvidence{ExpectedBytes: expected, NudgeDelivery: evidence(false, expected, runtime.NudgeSubmitUnconfirmed)},
			status:   InboundDeliveryPending,
			bytes:    expected,
			reason:   "submit not confirmed",
		},
		{
			name:     "every hostile operand at once — flag down, whole count, submit unconfirmed, send path errored: hold with the full count",
			evidence: InboundMemberEvidence{ExpectedBytes: expected, NudgeDelivery: evidence(false, expected, runtime.NudgeSubmitUnconfirmed), Err: sendErr},
			status:   InboundDeliveryPending,
			bytes:    expected,
			reason:   "pasted whole; send path errored: " + sendErr.Error(),
		},
		{
			name:     "landed whole, submit confirmed, but the send path errored afterwards: landed-then-failed is a hold, never a clean retry",
			evidence: InboundMemberEvidence{ExpectedBytes: expected, NudgeDelivery: evidence(true, expected, runtime.NudgeSubmitConfirmed), Err: sendErr},
			status:   InboundDeliveryPending,
			bytes:    expected,
			reason:   "pasted whole; send path errored: " + sendErr.Error(),
		},
		{
			name:     "unknown submit state from a newer runtime: landed, so hold rather than guess",
			evidence: InboundMemberEvidence{ExpectedBytes: expected, NudgeDelivery: evidence(true, expected, runtime.NudgeSubmit("queued"))},
			status:   InboundDeliveryPending,
			bytes:    expected,
			reason:   `unknown submit state "queued"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyInboundMember(tc.evidence)
			if got.Status != tc.status {
				t.Fatalf("status = %q, want %q (%+v)", got.Status, tc.status, got)
			}
			if got.DeliveredBytes != tc.bytes {
				t.Fatalf("delivered bytes = %d, want %d (%+v)", got.DeliveredBytes, tc.bytes, got)
			}
			if tc.reason == "" {
				if got.Reason != "" {
					t.Fatalf("reason = %q, want none on %s", got.Reason, tc.status)
				}
				return
			}
			if !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("reason = %q, want it to mention %q", got.Reason, tc.reason)
			}
		})
	}
}

// TestClassifyInboundMemberNeverSaysFailedForALandedPayload is the property
// behind the table: "failed" promises the consumer a clean redelivery, so it
// must be unreachable from any evidence in which the runtime said something
// reached the terminal (runtime.NudgeDelivery.Landed: the Delivered flag, or
// a vouching runtime's positive byte count) — with or without an error from
// the send path, because an error that follows a landed paste does not
// unpaste it.
func TestClassifyInboundMemberNeverSaysFailedForALandedPayload(t *testing.T) {
	sendErr := errors.New("boom")
	for _, delivered := range []bool{true, false} {
		for _, submit := range []runtime.NudgeSubmit{
			"", runtime.NudgeSubmitConfirmed, runtime.NudgeSubmitUnconfirmed, runtime.NudgeSubmitUnverified, "something-new",
		} {
			for _, bytes := range []int{0, 1, 41, 1214, 1215, 1216} {
				for _, err := range []error{nil, sendErr} {
					e := InboundMemberEvidence{
						NudgeDelivery: runtime.NudgeDelivery{Delivered: delivered, Bytes: bytes, Submit: submit},
						ExpectedBytes: 1215,
						Err:           err,
					}
					got := ClassifyInboundMember(e)
					if e.Landed() && got.Status == InboundDeliveryFailed {
						t.Fatalf("delivered=%v submit=%q bytes=%d err=%v classified as failed although the runtime said the payload landed: %+v",
							delivered, submit, bytes, err, got)
					}
					if !e.Landed() && got.Status != InboundDeliveryFailed {
						t.Fatalf("delivered=%v submit=%q bytes=%d err=%v classified as %s although nothing landed: %+v",
							delivered, submit, bytes, err, got.Status, got)
					}
					if err != nil && got.Status == InboundDeliveryDelivered {
						t.Fatalf("delivered=%v submit=%q bytes=%d with a send error classified as delivered: an errored send is never a clean delivery claim: %+v",
							delivered, submit, bytes, got)
					}
				}
			}
		}
	}
}
