package tmux

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

const nudgeDraftIdentityOption = "@gc-nudge-draft-identity"

func recoveredNudgeOption(id string) string {
	return "@gc-nudge-recovered-" + runtime.NudgePayloadDigest(id)
}

// ReleaseNudgeReservation retires only the named caller's reservation. It never
// sends keys or clears input, including when the pane has changed owners.
func (p *Provider) ReleaseNudgeReservation(name, id string) error {
	if id == "" {
		return nil
	}
	target := name
	if pane, err := p.tm.FindAgentPane(name); err == nil && pane != "" {
		target = pane
	}
	if _, err := p.tm.run("set-option", "-p", "-u", "-t", exactPaneTarget(target), recoveredNudgeOption(id)); err != nil {
		return err
	}
	value, err := p.tm.run("show-options", "-p", "-qv", "-t", exactPaneTarget(target), nudgeDraftIdentityOption)
	if err != nil {
		return err
	}
	parts := strings.SplitN(strings.TrimSpace(value), ":", 2)
	if len(parts) != 2 || parts[1] != id {
		return nil
	}
	_, err = p.tm.run("if-shell", "-F", "-t", exactPaneTarget(target),
		fmt.Sprintf("#{==:#{%s},%s}", nudgeDraftIdentityOption, strings.TrimSpace(value)),
		shellquote.Join([]string{"set-option", "-p", "-t", exactPaneTarget(target), nudgeDraftIdentityOption, parts[0] + ":"}))
	return err
}

// A nonempty identity reserves submission for the caller that owns retries.
// A known empty identity permits recovery of a one-shot delivery. Missing
// provenance is unknown, so a different payload cannot recover it.
func (t *Tmux) nudgeDraftIdentity(target, digest string) (string, bool, error) {
	value, err := t.run("show-options", "-p", "-qv", "-t", exactPaneTarget(target), nudgeDraftIdentityOption)
	if err != nil {
		return "", false, fmt.Errorf("reading draft retry identity: %w", err)
	}
	parts := strings.SplitN(strings.TrimSpace(value), ":", 2)
	if len(parts) != 2 || parts[0] != digest {
		return "", false, nil
	}
	return parts[1], true, nil
}

// releaseNudgeDraftIdentity retires only this delivery's retry reservation.
// Unverified delivery is terminal for retry callers; any remaining owned paste
// must be recoverable without an identity that its caller has acknowledged.
func (t *Tmux) releaseNudgeDraftIdentity(target, message, id string) error {
	digest := runtime.NudgePayloadDigest(message)
	_, err := t.run("if-shell", "-F", "-t", exactPaneTarget(target),
		fmt.Sprintf("#{==:#{%s},%s:%s}", nudgeDraftIdentityOption, digest, id),
		shellquote.Join([]string{"set-option", "-p", "-t", exactPaneTarget(target), nudgeDraftIdentityOption, digest + ":"}))
	return err
}
