package runtime

import "context"

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
