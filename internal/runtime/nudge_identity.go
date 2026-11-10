package runtime

import "context"

// NudgeReservationProvider retires a delivery's retry reservation without
// modifying composer input. Providers without reservations need no operation.
type NudgeReservationProvider interface {
	ReleaseNudgeReservation(name, id string) error
}

// ReleaseNudgeReservation releases an optional provider reservation by identity.
func ReleaseNudgeReservation(p Provider, name, id string) error {
	if id == "" {
		return nil
	}
	if rp, ok := p.(NudgeReservationProvider); ok {
		return rp.ReleaseNudgeReservation(name, id)
	}
	return nil
}

// NudgeRetryOwnerError identifies the queued delivery that must run before this
// caller can use the composer. Cause retains the transport's deferred sentinel.
type NudgeRetryOwnerError struct {
	ID    string
	Cause error
}

// Error describes the reservation blocking this delivery.
func (e *NudgeRetryOwnerError) Error() string {
	return "composer reserved by retry owner " + e.ID + ": " + e.Cause.Error()
}

// Unwrap preserves the delivery deferral classification.
func (e *NudgeRetryOwnerError) Unwrap() error { return e.Cause }

type nudgeIdentityKey struct{}

// WithNudgeIdentity carries a caller's stable retry identity through session
// operations. That caller retains ownership of staged input; an unrelated
// delivery cannot submit it. This metadata is never part of model input.
func WithNudgeIdentity(ctx context.Context, id string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, nudgeIdentityKey{}, id)
}

// NudgeContent constructs text with the delivery identity carried by ctx.
// The identity survives in-process provider routing but is not serialized.
func NudgeContent(ctx context.Context, text string) []ContentBlock {
	content := TextContent(text)
	if ctx != nil {
		content[0].nudgeID, _ = ctx.Value(nudgeIdentityKey{}).(string)
	}
	return content
}

// NudgeIdentity returns the identity attached to a single nudge text block.
func NudgeIdentity(content []ContentBlock) string {
	if len(content) != 1 {
		return ""
	}
	return content[0].nudgeID
}
