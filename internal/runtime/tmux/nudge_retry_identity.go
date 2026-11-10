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
func (p *Provider) ReleaseNudgeReservation(name, id string, invalidate bool) error {
	if id == "" {
		return nil
	}
	target, err := p.tm.FindAgentPane(name)
	if err != nil {
		return fmt.Errorf("finding reservation pane for %q: %w", name, err)
	}
	if target == "" {
		panes, err := p.tm.run("list-panes", "-s", "-t", exactPaneTarget(name), "-F", "#{pane_id}")
		if err != nil {
			return fmt.Errorf("verifying reservation pane for %q: %w", name, err)
		}
		ids := strings.Fields(panes)
		if len(ids) != 1 {
			return fmt.Errorf("cannot identify reservation pane for %q among %d panes", name, len(ids))
		}
		target = ids[0]
	}
	if !invalidate {
		if _, err := p.tm.run("set-option", "-p", "-u", "-t", exactPaneTarget(target), recoveredNudgeOption(id)); err != nil {
			return err
		}
	}
	value, err := p.tm.run("show-options", "-p", "-qv", "-t", exactPaneTarget(target), nudgeDraftIdentityOption)
	if err != nil {
		return err
	}
	parts := strings.SplitN(strings.TrimSpace(value), ":", 2)
	if len(parts) != 2 || parts[1] != id {
		return nil
	}
	command := shellquote.Join([]string{"set-option", "-p", "-t", exactPaneTarget(target), nudgeDraftIdentityOption, parts[0] + ":"})
	if invalidate {
		// A withdrawn batch must not regain submission permission. Preserve
		// both its input and any receipt from recovery that already occurred.
		command = shellquote.Join([]string{"set-option", "-p", "-t", exactPaneTarget(target), nudgeDraftPayloadOption, ""}) + " ; " + command
	}
	_, err = p.tm.run("if-shell", "-F", "-t", exactPaneTarget(target),
		fmt.Sprintf("#{==:#{%s},%s}", nudgeDraftIdentityOption, strings.TrimSpace(value)),
		command)
	return err
}

func (t *Tmux) recoveredNudgeStatus(session, target, id string) (runtime.NudgeSubmit, error) {
	if id == "" {
		return "", nil
	}
	value, err := t.run("show-options", "-p", "-qv", "-t", exactPaneTarget(target), recoveredNudgeOption(id))
	if err != nil {
		return "", fmt.Errorf("reading recovered nudge receipt: %w", err)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, "pending:") {
		// A crash or failed acknowledgement after Enter cannot license a fresh
		// paste. Only the unchanged owned draft can resume a pending recovery.
		digest := strings.TrimPrefix(value, "pending:")
		content, err := t.run("capture-pane", "-p", "-e", "-J", "-t", exactPaneTarget(target), "-S", fmt.Sprintf("-%d", promptObservationLines))
		if err != nil {
			return "", fmt.Errorf("observing pending recovery: %w", err)
		}
		draft, observed := nudgeComposer(content, t.resolveIdlePromptPrefix(session))
		if !observed {
			return "", fmt.Errorf("%w: pending recovery composer is unreadable", ErrNudgeDraftDeferred)
		}
		if draft != "" {
			stored, err := t.run("show-options", "-p", "-qv", "-t", exactPaneTarget(target), nudgeDraftPayloadOption)
			if err != nil {
				return "", err
			}
			shape, err := t.run("show-options", "-p", "-qv", "-t", exactPaneTarget(target), nudgeDraftShapeOption)
			if err != nil {
				return "", err
			}
			if _, owned := ownedPasteShape(strings.TrimSpace(stored), strings.TrimSpace(shape), draft); owned && strings.TrimPrefix(strings.TrimSpace(stored), "pending:") == digest {
				return "", nil
			}
		}
		return runtime.NudgeSubmitUnverified, nil
	}
	// Queue survivors keep their attempted batch's identity even when its
	// payload has shrunk. The original submission already included them.
	return runtime.NudgeSubmitConfirmed, nil
}

// A nonempty identity reserves submission for the caller that owns retries.
// A known empty identity permits recovery of a one-shot delivery. Missing
// provenance is unknown, so a different payload cannot recover it.
func (t *Tmux) nudgeDraftIdentity(session, target, digest string) (string, bool, error) {
	value, err := t.run("show-options", "-p", "-qv", "-t", exactPaneTarget(target), nudgeDraftIdentityOption)
	if err != nil {
		return "", false, fmt.Errorf("reading draft retry identity: %w", err)
	}
	parts := strings.SplitN(strings.TrimSpace(value), ":", 2)
	if len(parts) != 2 || parts[0] != digest {
		return "", false, nil
	}
	if id := parts[1]; id != "" && t.cfg.NudgeReservationInvalidated != nil {
		invalid, err := t.cfg.NudgeReservationInvalidated(session, id)
		if err != nil {
			return "", false, fmt.Errorf("%w: %w: reading reservation cancellation: %w", ErrNudgeDraftDeferred, runtime.ErrNudgeDraftUnowned, err)
		}
		if invalid {
			return "", false, fmt.Errorf("%w: %w: retry batch was canceled", ErrNudgeDraftDeferred, runtime.ErrNudgeDraftUnowned)
		}
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
