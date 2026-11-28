package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func terminalReasonForDeadQueuedNudge(item queuedNudge) string {
	if reason := strings.TrimSpace(item.LastError); reason != "" {
		return reason
	}
	return "failed"
}

func repairDeadQueuedNudge(front *nudgequeue.Store, item queuedNudge) bool {
	if item.BeadID == "" {
		return true
	}
	if front == nil {
		return false
	}
	shadow, found, err := front.FindIncludingTerminal(item.ID)
	if err != nil {
		return false
	}
	if found && nudgequeue.IsTerminalState(shadow.State) && !shadow.Open {
		return true
	}
	// A missing, reaped shadow needs no repair. Terminalize tolerates that case.
	return front.Terminalize(item, terminalStateForDeadQueuedNudge(item), terminalReasonForDeadQueuedNudge(item), "", item.DeadAt) == nil
}

func pruneNudgeTombstones(cityPath string, state *nudgeQueueState, front *nudgequeue.Store, now, deadline time.Time, clk clock.Clock) {
	retained := state.Tombstones[:0]
	for i, tombstone := range state.Tombstones {
		if clk.Now().After(deadline) {
			// Persist the untouched suffix ahead of attempted repairs. The next
			// pass resumes there even after a restart or a queue-state sort.
			next := append([]nudgequeue.Tombstone(nil), state.Tombstones[i:]...)
			next = append(next, retained...)
			state.Tombstones = next
			return
		}
		if tombstone.RepairPending && front != nil {
			if _, found, err := front.FindIncludingTerminal(tombstone.ID); err == nil && found {
				item := queuedNudge{ID: tombstone.ID, BeadID: tombstone.BeadID, LastError: tombstone.TerminalReason}
				if err := front.Terminalize(item, tombstone.TerminalState, tombstone.TerminalReason, "", tombstone.TerminalAt); err == nil {
					tombstone.RepairPending = false
					tombstone.RepairedAt = now.UTC()
				}
			} else if err == nil && !found {
				if reason := missingNudgeShadowRepairReason(cityPath, tombstone, now); reason != "" {
					tombstone.RepairPending = false
					tombstone.RepairedAt = now.UTC()
					if nudgeWarningWriter != nil {
						fmt.Fprintf(nudgeWarningWriter, "gc nudge: tombstone %q repaired: %s\n", tombstone.ID, reason) //nolint:errcheck
					}
				}
			}
		}
		if !tombstone.RepairPending && !tombstone.RepairedAt.IsZero() && !tombstone.RepairedAt.Add(nudgequeue.DispositionRetention).After(now) {
			continue
		}
		retained = append(retained, tombstone)
	}
	state.Tombstones = retained
}

func missingNudgeShadowRepairReason(cityPath string, tombstone nudgequeue.Tombstone, now time.Time) string {
	atCeiling := !tombstone.DeletedAt.IsZero() && !tombstone.DeletedAt.Add(nudgequeue.DeadLetterRetention).After(now)
	if tombstone.Source == "wait" && tombstone.Reference != nil && tombstone.Reference.Kind == "bead" && tombstone.Reference.ID != "" {
		if cityPath == "" {
			return ""
		}
		reason, err := repairNudgeTombstoneWaitReference(cityPath, tombstone, now, atCeiling)
		if err != nil {
			if nudgeWarningWriter != nil {
				fmt.Fprintf(nudgeWarningWriter, "gc nudge: repairing tombstone %q wait reference %q: %v; retaining terminal intent\n", tombstone.ID, tombstone.Reference.ID, err) //nolint:errcheck
			}
			return ""
		}
		if reason != "" {
			return reason
		}
	}
	// Give a missing shadow a full recovery window after payload deletion,
	// longer than the stale-shadow sweep window. A successful missing lookup
	// is required; unavailable stores and failed writes never start this clock.
	if atCeiling {
		return "seven-day missing-shadow ceiling"
	}
	return ""
}

func repairNudgeTombstoneWaitReference(cityPath string, tombstone nudgequeue.Tombstone, now time.Time, atCeiling bool) (reason string, err error) {
	// The session class may be relocated away from the nudge store. Close
	// only the work handle opened here, never the routes' shared session store.
	opened, err := openNudgeWorkStore(cityPath, cityPath)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, closeBeadStoreHandle(opened)) }()
	sessFront := cliSessionFrontDoor(opened, nil, cityPath)
	wait, err := sessFront.GetWait(tombstone.Reference.ID)
	if errors.Is(err, beads.ErrNotFound) || errors.Is(err, session.ErrNotAWait) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if wait.SessionID != tombstone.SessionID {
		return "", nil
	}
	if wait.Status == "closed" || session.IsWaitTerminalState(wait.State) {
		// A prior pass may have persisted the wait but failed to release its
		// idle hold. Finish that half before expiring the repair receipt.
		if wait.NudgeID == tombstone.ID {
			if err := sessFront.ClearIdleWaitHold(wait.SessionID); err != nil {
				return "", err
			}
		}
		return "terminal wait reference", nil
	}
	if !atCeiling {
		return "", nil
	}
	nudgeID := wait.NudgeID
	if nudgeID == "" {
		nudgeID = waitNudgeID(wait)
	}
	if nudgeID != tombstone.ID {
		// An explicit retry has advanced the wait to a different attempt.
		return "", nil
	}
	// The tombstone is the last durable terminal intent when the shadow is
	// gone. Persist it on the matching wait before starting receipt expiry.
	if err := sessFront.FailWaitFromNudge(wait.ID, now, nudgeID, tombstone.TerminalReason, ""); err != nil {
		return "", err
	}
	if err := sessFront.ClearIdleWaitHold(wait.SessionID); err != nil {
		return "", err
	}
	return "seven-day missing-shadow ceiling; wait finalized", nil
}

// Only the transaction that adds an item to Dead calls this seam. Notifications
// run after the queue commit and unlock, so a failed send never requeues input.
var nudgeDeadLetterNotify = sendNudgeDeadLetter

var nudgeDeadLetterLossNotify = sendNudgeDeadLetterLoss

// Mail providers have independent I/O deadlines. Bound both the caller's wait
// and outstanding sends so an unavailable provider cannot consume claim leases
// or accumulate an unbounded number of background calls.
const nudgeDeadLetterMailBudget = 2 * time.Second

var nudgeDeadLetterMailSlots = make(chan struct{}, 4)

func notifyNudgeDeadLetters(cityPath string, newlyDead, lost []queuedNudge) {
	items := make([]queuedNudge, 0, len(newlyDead)+len(lost))
	for _, item := range newlyDead {
		if item.UnresolvedDeadLetter() {
			items = append(items, item)
		}
	}
	firstLost := len(items)
	items = append(items, lost...)
	if len(items) == 0 {
		return
	}
	writer, notify, notifyLoss := nudgeWarningWriter, nudgeDeadLetterNotify, nudgeDeadLetterLossNotify
	for i, item := range items {
		if writer != nil {
			if i >= firstLost {
				fmt.Fprintf(writer, "gc nudge: dead-letter loss: item %q session %q removed at seven-day retention cap; preserved input is no longer replayable\n", item.ID, item.Agent) //nolint:errcheck
			} else {
				fmt.Fprintf(writer, "gc nudge: action required: nudge %q for %q dead-lettered: %s; resolve the preserved pane input, then gc nudge replay %s\n", item.ID, item.Agent, strings.Join(strings.Fields(deadReason(item)), " "), item.ID) //nolint:errcheck
			}
		}
	}
	// Explicitly disposed losses still need a log when repair is unfinished,
	// but only unresolved input triggers the existing best-effort loss mail.
	mailItems := items[:firstLost]
	for _, item := range items[firstLost:] {
		if item.UnresolvedDeadLetter() {
			mailItems = append(mailItems, item)
		}
	}
	items = mailItems
	if len(items) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), nudgeDeadLetterMailBudget)
	defer cancel()
	type result struct {
		index   int
		warning string
		err     error
	}
	jobs, results := make(chan int, len(items)), make(chan result, len(items))
	for i := range items {
		jobs <- i
	}
	close(jobs)
	for range min(len(items), cap(nudgeDeadLetterMailSlots)) {
		go func() {
			for i := range jobs {
				select {
				case nudgeDeadLetterMailSlots <- struct{}{}:
				case <-ctx.Done():
					return
				}
				if ctx.Err() != nil {
					<-nudgeDeadLetterMailSlots
					return
				}
				var warning bytes.Buffer
				err := func() error {
					defer func() { <-nudgeDeadLetterMailSlots }()
					if i >= firstLost {
						return notifyLoss(cityPath, items[i], &warning)
					}
					return notify(cityPath, items[i], &warning)
				}()
				// Buffered results and private diagnostics remain safe when the
				// caller has timed out; late sends never touch its writer.
				results <- result{i, warning.String(), err}
			}
		}()
	}
	completed := make([]bool, len(items))
	report := func(i int, err error) {
		if err != nil && writer != nil {
			item, disposition := items[i], "doctor retains the item"
			if i >= firstLost {
				disposition = "loss notification failed; item was removed"
			}
			fmt.Fprintf(writer, "gc nudge: dead-letter mail failed for item %q session %q reason %q: %v; %s\n", item.ID, item.Agent, deadReason(item), err, disposition) //nolint:errcheck
		}
	}
	for range items {
		select {
		case sent := <-results:
			completed[sent.index] = true
			if writer != nil {
				io.WriteString(writer, sent.warning) //nolint:errcheck
			}
			report(sent.index, sent.err)
		case <-ctx.Done():
			for i := range items {
				if !completed[i] {
					report(i, ctx.Err())
				}
			}
			return
		}
	}
}

func nudgeDeadLetterRecipient(item queuedNudge, fallback string, lookup func(string) (beads.Bead, error)) (string, error) {
	if ref := item.Reference; ref != nil && ref.Kind == "bead" && ref.ID != "" {
		b, err := lookup(ref.ID)
		if err != nil {
			if strings.TrimSpace(fallback) == "" {
				return "", fmt.Errorf("reading escalation recipient on bead %q: %w; session.park_alert_to is unset", ref.ID, err)
			}
			return strings.TrimSpace(fallback), fmt.Errorf("reading escalation recipient on bead %q: %w", ref.ID, err)
		}
		if to := strings.TrimSpace(b.Metadata["escalation_recipient"]); to != "" {
			return to, nil
		}
	}
	if to := strings.TrimSpace(fallback); to != "" {
		return to, nil
	}
	return "", fmt.Errorf("no bead escalation_recipient and session.park_alert_to is unset")
}

func sendNudgeDeadLetter(cityPath string, item queuedNudge, log io.Writer) error {
	return sendNudgeDeadLetterMail(cityPath, item, log, false)
}

func sendNudgeDeadLetterLoss(cityPath string, item queuedNudge, log io.Writer) error {
	return sendNudgeDeadLetterMail(cityPath, item, log, true)
}

func sendNudgeDeadLetterMail(cityPath string, item queuedNudge, log io.Writer, lost bool) error {
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		return fmt.Errorf("loading dead-letter mail configuration: %w", err)
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	lookup := func(id string) (beads.Bead, error) {
		var opened []beads.Store
		defer func() {
			for _, store := range opened {
				if err := closeBeadStoreHandle(store); err != nil {
					fmt.Fprintf(log, "gc nudge: closing escalation lookup store: %v\n", err) //nolint:errcheck
				}
			}
		}()
		store, _, err := resolveOwningStoreDir(id, cfg, cityPath, func(path string) (beads.Store, error) {
			s, err := openStoreAtForCityWithConfig(path, cityPath, cfg)
			if err == nil {
				opened = append(opened, s)
			}
			return s, err
		})
		if err != nil {
			return beads.Bead{}, err
		}
		return store.Get(id)
	}
	to, lookupErr := nudgeDeadLetterRecipient(item, cfg.Session.ParkAlertTo, lookup)
	if to == "" {
		return lookupErr
	}
	if lookupErr != nil {
		fmt.Fprintf(log, "gc nudge: dead-letter item %q: %v; using session.park_alert_to=%q\n", item.ID, lookupErr, to) //nolint:errcheck
	}
	subject, body := nudgeDeadLetterMail(item)
	if lost {
		subject, body = nudgeDeadLetterLossMail(item)
	}
	return sendNudgeQueueMail(cityPath, cfg, to, subject, body, "")
}

func sendNudgeQueueMail(cityPath string, cfg *config.City, to, subject, body, dedupKey string) error {
	name := os.Getenv("GC_MAIL")
	if name == "" {
		name = mailProviderNameForCity(cityPath)
	}
	var workStore, msgStore, sessStore beads.Store
	if !strings.HasPrefix(name, "exec:") && name != "fake" && name != "fail" {
		var err error
		workStore, err = openCityStoreAt(cityPath)
		if err != nil {
			return fmt.Errorf("opening nudge mail store: %w", err)
		}
		defer closeBeadStoreHandle(workStore) //nolint:errcheck // send result remains authoritative
		msgStore = resolveMailMessagesStore(cliStorageRoutes(cityPath), workStore, cfg, cityPath, nil)
		sessStore = cliSessionStore(workStore, cfg, cityPath)
	}
	mailer := newMailProviderNamedWithSessionStore(cityPath, name, msgStore, sessStore, false)
	if filer, ok := mailer.(mail.FilingProvider); ok && dedupKey != "" {
		// A dispatcher can exit after delivery, before saving the receipt.
		// Filing lookup also finds a notice its recipient already archived.
		bucket := sha256.Sum256([]byte(dedupKey))
		dir := citylayout.RuntimePath(cityPath, "nudges", "hold-notices", fmt.Sprintf("%02x", bucket[0]))
		bodyHash := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
		return runtime.WithProviderStateDirLock(dir, func() error {
			if _, found, err := filer.LookupFiling(dedupKey, bodyHash); err != nil || found {
				return err
			}
			_, err := filer.SendFiling("nudge-dispatcher", to, subject, body, dedupKey, bodyHash)
			return err
		})
	}
	if sender, ok := mailer.(mail.DedupSender); ok && dedupKey != "" {
		_, _, err := sender.SendDeduped("nudge-dispatcher", to, subject, body, dedupKey)
		return err
	}
	_, err := mailer.Send("nudge-dispatcher", to, subject, body)
	return err
}

type draftHoldNotice struct {
	key  string
	hold nudgequeue.DraftHold
}

// Reserve the session notice under the queue lock. The lease lets another
// poller retry if this one exits before the mail provider returns.
func reserveDraftHoldNotice(state *nudgeQueueState, item queuedNudge, now time.Time, delay time.Duration) *draftHoldNotice {
	key := item.DraftHoldKey()
	if state.DraftHolds == nil {
		state.DraftHolds = make(map[string]nudgequeue.DraftHold)
	}
	hold, ok := state.DraftHolds[key]
	if !ok || !hold.MatchesSession(item.SessionID, item.ContinuationEpoch) {
		hold = nudgequeue.DraftHold{Since: now.UTC(), SessionID: item.SessionID, ContinuationEpoch: item.ContinuationEpoch}
		state.DraftHolds[key] = hold
	}
	if !hold.NotifiedAt.IsZero() || now.Before(hold.Since.Add(delay)) || now.Before(hold.NotifyLease) {
		return nil
	}
	hold.NotifyLease = now.UTC().Add(defaultQueuedNudgeClaimTTL)
	state.DraftHolds[key] = hold
	return &draftHoldNotice{key, hold}
}

func (notice draftHoldNotice) deliver(cityPath string, now time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), nudgeDeadLetterMailBudget)
	defer cancel()
	select {
	case nudgeDeadLetterMailSlots <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("waiting for draft hold mail slot: %w", ctx.Err())
	}
	result := make(chan error, 1)
	go func() {
		defer func() { <-nudgeDeadLetterMailSlots }()
		result <- notice.send(cityPath, now)
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return fmt.Errorf("draft hold mail for %q still pending: %w", notice.key, ctx.Err())
	}
}

func (notice draftHoldNotice) send(cityPath string, now time.Time) error {
	key := notice.key
	cfg, sendErr := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if sendErr == nil {
		to := strings.TrimSpace(cfg.Session.ParkAlertTo)
		if to == "" {
			sendErr = errors.New("session.park_alert_to is unset")
		} else {
			resolveRigPaths(cityPath, cfg.Rigs)
			subject := "NUDGE HOLD: " + key
			body := fmt.Sprintf("Queued nudges for %s are waiting on preserved composer input. Inspect the draft and resolve it to resume delivery. Run gc nudge status %s --json to see the queue.", key, key)
			dedupKey := fmt.Sprintf("nudge-hold/%s/%s/%s/%d", key, notice.hold.SessionID, notice.hold.ContinuationEpoch, notice.hold.Since.UnixNano())
			sendErr = sendNudgeQueueMail(cityPath, cfg, to, subject, body, dedupKey)
		}
	}
	saveErr := nudgequeue.WithState(cityPath, func(state *nudgeQueueState) error {
		hold, ok := state.DraftHolds[key]
		if !ok || hold != notice.hold {
			return nil
		}
		hold.NotifyLease = time.Time{}
		if sendErr == nil {
			hold.NotifiedAt = now.UTC()
		}
		state.DraftHolds[key] = hold
		return nil
	})
	if err := errors.Join(sendErr, saveErr); err != nil {
		return fmt.Errorf("sending draft hold notice for %q: %w", key, err)
	}
	return nil
}

func nudgeDeadLetterMail(item queuedNudge) (subject, body string) {
	sessionID := item.SessionID
	if sessionID == "" {
		sessionID = item.Agent
	}
	reason := strings.Join(strings.Fields(deadReason(item)), " ")
	firstLine, _, _ := strings.Cut(item.Message, "\n")
	preview := []rune(strings.TrimSuffix(firstLine, "\r"))
	if len(preview) > 160 {
		preview = append(preview[:160], '…')
	}
	firstAttempt := "not recorded"
	if !item.FirstAttemptAt.IsZero() {
		firstAttempt = item.FirstAttemptAt.UTC().Format(time.RFC3339Nano)
	}
	subject = fmt.Sprintf("NUDGE DEAD LETTER session %s reason %s item %s", sessionID, reason, item.ID)
	body = fmt.Sprintf("Item: %s\nSession: %s\nAgent: %s\nReason: %s\nFirst line: %q\nFirst attempt: %s\nDead transition: %s\n\nResolve the preserved pane input, then replay once:\ngc nudge replay %s\n",
		item.ID, sessionID, item.Agent, reason, string(preview), firstAttempt, item.DeadAt.UTC().Format(time.RFC3339Nano), item.ID)
	body += fmt.Sprintf("Or dismiss without replay: gc nudge replay %s --dismiss\nRecovery cap: seven days from the dead transition (%s). Disposition receipts remain for one hour, bounded by this cap.\n", item.ID, item.DeadAt.Add(nudgequeue.DeadLetterRetention).UTC().Format(time.RFC3339Nano))
	return subject, body
}

func nudgeDeadLetterLossMail(item queuedNudge) (subject, body string) {
	subject, body = nudgeDeadLetterMail(item)
	subject = strings.Replace(subject, "NUDGE DEAD LETTER", "NUDGE DEAD LETTER LOST", 1)
	body, _, _ = strings.Cut(body, "\n\n")
	body += "\n\nThe seven-day retention cap expired without replay or dismissal. The preserved input was removed from the queue and can no longer be replayed. Recover the original message from its source if it is still needed.\n"
	return subject, body
}
