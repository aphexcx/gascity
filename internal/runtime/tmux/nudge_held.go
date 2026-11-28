package tmux

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

const (
	nudgeDraftShapeOption = "@gc-nudge-draft-shape"
	nudgeHeldOption       = "@gc-nudge-held"
)

// holdForUnseenNudgePaste holds delivery to an empty composer while the mark of
// a full paste that Claude never showed still stands: until that paste renders,
// an empty composer cannot tell a late paste from a lost one. The hold is
// bounded. Once the composer has been read empty for NudgeUnseenPasteAge with
// no client attached, the mark is retired, the paste counts as lost, and the
// caller delivers. A nil receipt with a nil error means no such hold applies.
func (t *Tmux) holdForUnseenNudgePaste(target string, now time.Time) (*runtime.NudgeHeld, error) {
	payload, err := t.run("show-options", "-p", "-qv", "-t", exactPaneTarget(target), nudgeDraftPayloadOption)
	if err != nil {
		return nil, fmt.Errorf("%w: checking unrendered paste ownership: %w", ErrNudgeDraftDeferred, err)
	}
	digest := strings.TrimSpace(payload)
	if len(digest) != len(runtime.NudgePayloadDigest("")) {
		return nil, nil
	}
	shape, err := t.run("show-options", "-p", "-qv", "-t", exactPaneTarget(target), nudgeDraftShapeOption)
	if err != nil {
		return nil, fmt.Errorf("%w: checking unrendered paste shape: %w", ErrNudgeDraftDeferred, err)
	}
	shape = strings.TrimSpace(shape)
	if parts := strings.Split(shape, ":"); len(parts) != 5 || parts[0] != digest || parts[4] != "" {
		return nil, nil
	}
	waiting := fmt.Errorf("%w: %w: %w: waiting for earlier paste to render", ErrNudgeDraftDeferred, runtime.ErrNudgeDraftUnmatched, runtime.ErrNudgePasteUnseen)
	// The shape carries this paste's digest and time, so the recorded first
	// sighting of the empty composer belongs to this paste alone.
	held, err := t.nudgeHeldReceipt(target, shape, "own_paste", time.Time{}, now)
	if err != nil {
		return held, errors.Join(waiting, err)
	}
	limit := t.cfg.NudgeUnseenPasteAge
	if limit <= 0 {
		limit = DefaultConfig().NudgeUnseenPasteAge
	}
	if time.Duration(held.AgeMS)*time.Millisecond < limit {
		return held, waiting
	}
	// Retire only the mark that was read, and only while no client is attached.
	out, err := t.run("if-shell", "-F", "-t", exactPaneTarget(target),
		fmt.Sprintf("#{&&:#{==:#{%s},%s},#{&&:#{==:#{%s},%s},#{==:#{session_attached},0}}}", nudgeDraftPayloadOption, digest, nudgeDraftShapeOption, shape),
		shellquote.Join([]string{"set-option", "-p", "-t", exactPaneTarget(target), nudgeDraftPayloadOption, ""}),
		"display-message -p draft-ownership-changed")
	if err != nil {
		return held, errors.Join(waiting, fmt.Errorf("retiring unseen paste: %w", err))
	}
	if strings.TrimSpace(out) != "" {
		return held, waiting
	}
	log.Printf("nudge: paste never shown, composer empty since %s; treating it as lost and resuming delivery target=%q", held.Since.Format(time.RFC3339Nano), target)
	return nil, nil
}

// ownedPasteShape checks the recorded rendering. Until the first observation,
// only a confirmed full paste and a single placeholder of its staged count match.
func ownedPasteShape(stored, shape, draft string) (time.Time, bool) {
	parts := strings.Split(shape, ":")
	pending := strings.HasPrefix(stored, "pending:")
	stored = strings.TrimPrefix(stored, "pending:")
	if len(parts) != 5 || parts[0] != stored || len(stored) != len(runtime.NudgePayloadDigest("")) {
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
	if parts[4] == "" {
		_, digestErr := strconv.ParseUint(parts[3], 16, 64)
		return time.UnixMilli(at), !pending && len(parts[3]) == len(stored) && digestErr == nil && collapsedPasteHasLines(draft, lines)
	}
	if strings.Contains(draft, "[Pasted text #") {
		return time.UnixMilli(at), claudeCollapsedPaste.MatchString(draft) && runtime.NudgePayloadDigest(draft) == parts[4]
	}
	return time.UnixMilli(at), normalizedNudgeDigest(draft) == parts[3]
}

// recoverEarlierNudgePaste submits a proven earlier paste once its age and retry
// reservation permit recovery. Activity with the paste still visible is a hold,
// not an acknowledgement, and recoverStagedDraft checks before every Enter.
func (t *Tmux) recoverEarlierNudgePaste(session, target, draft, message, requestedID string, claude bool, read func(string) composerReading) (bool, time.Time, error) {
	stored, err := t.run("show-options", "-p", "-qv", "-t", exactPaneTarget(target), nudgeDraftPayloadOption)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("%w: reading paste ownership: %w", ErrNudgeDraftDeferred, err)
	}
	stored = strings.TrimSpace(stored)
	shape, err := t.run("show-options", "-p", "-qv", "-t", exactPaneTarget(target), nudgeDraftShapeOption)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("%w: reading paste shape: %w", ErrNudgeDraftDeferred, err)
	}
	shape = strings.TrimSpace(shape)
	since, owned := ownedPasteShape(stored, shape, draft)
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
	if strings.HasSuffix(shape, ":") {
		if !claude {
			return false, time.Time{}, nil
		}
		// The initial composer read preceded the mark reads. Confirm that
		// rendering under the detached ownership guard before binding it.
		lines, err := t.captureOwnedNudgeDraftInput(target, digest, true)
		if err != nil {
			return false, time.Time{}, fmt.Errorf("%w: observing late paste: %w", ErrNudgeDraftDeferred, err)
		}
		if current := read(strings.Join(lines, "\n")); !current.observed || current.draft != draft {
			return false, time.Time{}, fmt.Errorf("%w: %w: late paste rendering changed", ErrNudgeDraftDeferred, runtime.ErrNudgeDraftUnmatched)
		}
		if err := t.saveNudgeDraftShape(context.Background(), target, shape, stored, shape+runtime.NudgePayloadDigest(draft)); err != nil {
			return false, time.Time{}, fmt.Errorf("%w: recording late paste rendering: %w", ErrNudgeDraftDeferred, err)
		}
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
		current := read(content)
		if !current.observed || (current.draft != "" && current.draft != draft) {
			observeErr = fmt.Errorf("composer changed during owned paste recovery")
			return paneSubmitObservation{}, observeErr
		}
		return paneSubmitObservation{busy: paneContainsBusyIndicator(strings.Split(runtime.RenderPeek(content).Output, "\n")), drafted: current.draft != ""}, nil
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
