package tmux

import (
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

// ownedPasteShape requires both a confirmed pane digest and its matching shape.
// Pending, legacy digest-only, invalidated and edited pastes cannot recover a
// different payload: there is no evidence of their shape and age.
func ownedPasteShape(stored, shape, draft string) (time.Time, bool) {
	parts := strings.Split(shape, ":")
	if len(parts) != 3 || parts[0] != stored || len(stored) != len(runtime.NudgePayloadDigest("")) {
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
	match := claudeCollapsedPaste.FindStringSubmatch(draft)
	if match == nil {
		return time.Time{}, false
	}
	shown := 0
	if match[1] != "" {
		shown, err = strconv.Atoi(match[1])
		if err != nil {
			return time.Time{}, false
		}
	}
	return time.UnixMilli(at), shown == lines
}

// recoverEarlierNudgePaste submits only an earlier owned placeholder, never the
// incoming payload. Activity with the placeholder still visible is a hold, not
// an acknowledgement, and recoverStagedDraft checks it before every Enter.
func (t *Tmux) recoverEarlierNudgePaste(session, target, draft, prefix, message string) (bool, time.Time, error) {
	if !claudeCollapsedPaste.MatchString(draft) {
		return false, time.Time{}, nil
	}
	stored, err := t.run("show-options", "-p", "-qv", "-t", exactPaneTarget(target), nudgeDraftPayloadOption)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("%w: reading paste ownership: %w", ErrNudgeDraftDeferred, err)
	}
	stored = strings.TrimSpace(stored)
	if stored == runtime.NudgePayloadDigest(message) {
		return false, time.Time{}, nil
	}
	shape, err := t.run("show-options", "-p", "-qv", "-t", exactPaneTarget(target), nudgeDraftShapeOption)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("%w: reading paste shape: %w", ErrNudgeDraftDeferred, err)
	}
	since, owned := ownedPasteShape(stored, strings.TrimSpace(shape), draft)
	if !owned {
		return false, time.Time{}, nil
	}
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
	age := t.cfg.NudgeOwnPasteAge
	if age <= 0 {
		age = DefaultConfig().NudgeOwnPasteAge
	}
	if time.Since(since) < age {
		return false, since, fmt.Errorf("%w: own paste is younger than recovery age", ErrNudgeDraftDeferred)
	}
	var observeErr error
	observe := func() (paneSubmitObservation, error) {
		lines, err := t.captureOwnedNudgeDraftInput(target, stored)
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
		_, err := t.sendOwnedNudgeDraftSubmitSequence(target, stored, t.nudgeSubmitKeySequence(target))
		observeErr = err
		return err
	}
	outcome := recoverStagedDraft(send, func() { t.WakePaneIfDetached(session) }, observe, time.Sleep)
	if outcome != stagedDraftCleared && outcome != stagedDraftSubmittedBusy {
		if observeErr != nil {
			return false, since, fmt.Errorf("%w: recovering own paste: %w", ErrNudgeDraftDeferred, observeErr)
		}
		return false, since, fmt.Errorf("%w: own paste remains held (%s)", ErrNudgeDraftDeferred, outcome)
	}
	log.Printf("nudge: own paste held since %s submitted target=%q", since.UTC().Format(time.RFC3339Nano), target)
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
