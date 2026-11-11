package tmux

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	nudgeDraftShapeOption = "@gc-nudge-draft-shape"
	nudgeHeldOption       = "@gc-nudge-held"
)

// ownedPasteShape requires a pane digest and a rendering observed after the full
// paste. Legacy line counts cannot establish which opaque paste is in the pane.
func ownedPasteShape(stored, shape, draft string) (time.Time, bool) {
	parts := strings.Split(shape, ":")
	stored = strings.TrimPrefix(stored, "pending:")
	if len(parts) != 5 || parts[0] != stored || len(stored) != len(runtime.NudgePayloadDigest("")) || parts[4] == "" {
		return time.Time{}, false
	}
	if _, err := strconv.ParseUint(stored, 16, 64); err != nil {
		return time.Time{}, false
	}
	at, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || at <= 0 {
		return time.Time{}, false
	}
	lines, err := strconv.Atoi(parts[2])
	if err != nil || lines < 0 {
		return time.Time{}, false
	}
	if strings.Contains(draft, "[Pasted text #") {
		return time.UnixMilli(at), claudeCollapsedPaste.MatchString(draft) && runtime.NudgePayloadDigest(draft) == parts[4]
	}
	return time.UnixMilli(at), normalizedNudgeDigest(draft) == parts[3]
}

// recoverEarlierNudgePaste submits a proven earlier paste once its age and retry
// reservation permit recovery. Activity with the paste still visible is a hold,
// not an acknowledgement, and recoverStagedDraft checks before every Enter.
func (t *Tmux) recoverEarlierNudgePaste(session, target, draft, prefix, message, requestedID string) (bool, time.Time, error) {
	stored, err := t.run("show-options", "-p", "-qv", "-t", exactPaneTarget(target), nudgeDraftPayloadOption)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("%w: reading paste ownership: %w", ErrNudgeDraftDeferred, err)
	}
	stored = strings.TrimSpace(stored)
	shape, err := t.run("show-options", "-p", "-qv", "-t", exactPaneTarget(target), nudgeDraftShapeOption)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("%w: reading paste shape: %w", ErrNudgeDraftDeferred, err)
	}
	since, owned := ownedPasteShape(stored, strings.TrimSpace(shape), draft)
	if !owned {
		return false, time.Time{}, nil
	}
	digest := strings.TrimPrefix(stored, "pending:")
	if _, err := t.nudgeDraftOwnershipHook(session, target, false); err != nil {
		return false, time.Time{}, fmt.Errorf("%w: %w: checking earlier paste ownership: %w", ErrNudgeDraftDeferred, runtime.ErrNudgeDraftUnowned, err)
	}
	attached, err := t.run("display-message", "-t", exactPaneTarget(session), "-p", "#{session_attached}")
	if err != nil {
		return false, time.Time{}, fmt.Errorf("%w: reading earlier paste attachment state: %w", ErrNudgeDraftDeferred, err)
	}
	if strings.TrimSpace(attached) != "0" {
		return false, time.Time{}, fmt.Errorf("%w: %w: earlier paste is not confirmed detached", ErrNudgeDraftDeferred, runtime.ErrNudgeDraftUnowned)
	}
	id, known, err := t.nudgeDraftIdentity(session, target, digest)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("%w: %w", ErrNudgeDraftDeferred, err)
	}
	if !known {
		return false, since, fmt.Errorf("%w: earlier paste has another or unknown retry owner", ErrNudgeDraftDeferred)
	}
	if strings.HasPrefix(stored, "pending:") {
		if err := t.confirmNudgeDraftDigest(target, digest); err != nil {
			return false, since, fmt.Errorf("%w: promoting pending paste: %w", ErrNudgeDraftDeferred, err)
		}
	}
	if digest == runtime.NudgePayloadDigest(message) && (id == "" || requestedID == id) {
		return false, since, nil
	}
	reservationAge := t.cfg.NudgeReservationAge
	if reservationAge <= 0 {
		reservationAge = DefaultConfig().NudgeReservationAge
	}
	if id != "" && time.Since(since) < reservationAge {
		return false, since, &runtime.NudgeRetryOwnerError{ID: id, Cause: ErrNudgeDraftDeferred}
	}
	age := t.cfg.NudgeOwnPasteAge
	if age <= 0 {
		age = DefaultConfig().NudgeOwnPasteAge
	}
	if time.Since(since) < age {
		return false, since, fmt.Errorf("%w: own paste is younger than recovery age", ErrNudgeDraftDeferred)
	}
	if id != "" {
		// The original Enter may take effect before recovery's first
		// observation. Persist intent before observing acceptance as well
		// as before sending another key.
		if _, err := t.run("set-option", "-p", "-t", exactPaneTarget(target), recoveredNudgeOption(id), "pending:"+digest); err != nil {
			return false, since, fmt.Errorf("%w: recording pending recovery: %w", ErrNudgeDraftDeferred, err)
		}
	}
	var observeErr error
	observe := func() (paneSubmitObservation, error) {
		lines, err := t.captureOwnedNudgeDraftInput(target, digest, true)
		if err != nil {
			observeErr = err
			return paneSubmitObservation{}, err
		}
		content := strings.Join(lines, "\n")
		current, ok := nudgeComposer(content, prefix)
		if !ok || (current != "" && current != draft) {
			observeErr = fmt.Errorf("composer changed during owned paste recovery")
			return paneSubmitObservation{}, observeErr
		}
		return paneSubmitObservation{busy: paneContainsBusyIndicator(strings.Split(runtime.RenderPeek(content).Output, "\n")), drafted: current != ""}, nil
	}
	send := func() error {
		if _, _, err := t.nudgeDraftIdentity(session, target, digest); err != nil {
			observeErr = err
			return err
		}
		_, err := t.sendOwnedNudgeDraftSubmitSequence(target, digest, t.nudgeSubmitKeySequence(target), true)
		observeErr = err
		return err
	}
	outcome := recoverStagedDraft(send, func() { t.WakePaneIfDetached(session) }, observe, time.Sleep)
	if outcome != stagedDraftCleared && outcome != stagedDraftSubmittedBusy {
		if observeErr != nil {
			if errors.Is(observeErr, runtime.ErrNudgeDraftUnowned) {
				since = time.Time{}
			}
			return false, since, fmt.Errorf("%w: recovering own paste: %w", ErrNudgeDraftDeferred, observeErr)
		}
		return false, since, fmt.Errorf("%w: own paste remains held (%s)", ErrNudgeDraftDeferred, outcome)
	}
	if id != "" {
		// Expiry may overtake a live queue owner whose retry was delayed. Keep
		// its acknowledgement until that owner terminalizes, so it cannot paste
		// again after another caller recovered its input.
		if _, err := t.run("set-option", "-p", "-t", exactPaneTarget(target), recoveredNudgeOption(id), digest); err != nil {
			return false, since, fmt.Errorf("%w: recording recovered owner receipt: %w", ErrNudgeDraftDeferred, err)
		}
	}
	log.Printf("nudge: own paste held since %s submitted target=%q", since.UTC().Format(time.RFC3339Nano), target)
	if err := t.clearNudgeDraftProvenance(target, digest); err != nil {
		return false, since, fmt.Errorf("%w: clearing recovered paste: %w", ErrNudgeDraftDeferred, err)
	}
	return true, since, nil
}

func (t *Tmux) nudgeHeldReceipt(target, draft, state string, since, now time.Time) (*runtime.NudgeHeld, error) {
	now = now.UTC().Truncate(time.Millisecond)
	receipt := func() *runtime.NudgeHeld {
		observedSince := since
		if observedSince.IsZero() || observedSince.After(now) {
			observedSince = now
		}
		return &runtime.NudgeHeld{State: state, Since: observedSince.UTC(), AgeMS: max(0, now.Sub(observedSince).Milliseconds())}
	}
	key := state + ":" + runtime.NudgePayloadDigest(draft)
	prior, err := t.run("show-options", "-p", "-qv", "-t", exactPaneTarget(target), nudgeHeldOption)
	if err != nil {
		return receipt(), fmt.Errorf("reading composer hold age: %w", err)
	}
	parts := strings.Split(strings.TrimSpace(prior), ":")
	unchanged := len(parts) == 3 && strings.Join(parts[:2], ":") == key
	if unchanged {
		at, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || at <= 0 || time.UnixMilli(at).After(now) {
			unchanged = false
		} else if since.IsZero() {
			since = time.UnixMilli(at)
		}
	}
	if since.IsZero() || since.After(now) {
		since = now
	}
	since = since.UTC()
	if !unchanged {
		if _, err := t.run("set-option", "-p", "-t", exactPaneTarget(target), nudgeHeldOption, key+":"+strconv.FormatInt(since.UnixMilli(), 10)); err != nil {
			return receipt(), fmt.Errorf("recording composer hold age: %w", err)
		}
		log.Printf("nudge: %s holds composer since %s, delivery held target=%q", state, since.UTC().Format(time.RFC3339Nano), target)
	}
	return receipt(), nil
}
