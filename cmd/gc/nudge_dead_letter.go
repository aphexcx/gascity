package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// Only the transaction that adds an item to Dead calls this seam. Notifications
// run after the queue commit and unlock, so a failed send never requeues input.
var nudgeDeadLetterNotify = sendNudgeDeadLetter

func notifyNewNudgeDeadLetters(cityPath string, items []queuedNudge) {
	for _, item := range items {
		if nudgeWarningWriter != nil {
			fmt.Fprintf(nudgeWarningWriter, "gc nudge: action required: nudge %q for %q dead-lettered: %s; resolve the preserved pane input, then gc nudge replay %s\n", item.ID, item.Agent, strings.Join(strings.Fields(deadReason(item)), " "), item.ID) //nolint:errcheck
		}
		if err := nudgeDeadLetterNotify(cityPath, item); err != nil && nudgeWarningWriter != nil {
			fmt.Fprintf(nudgeWarningWriter, "gc nudge: dead-letter mail failed for item %q session %q reason %q: %v\n", item.ID, item.Agent, deadReason(item), err) //nolint:errcheck
		}
	}
}

func nudgeDeadLetterRecipient(item queuedNudge, fallback string, lookup func(string) (beads.Bead, error)) (string, error) {
	if ref := item.Reference; ref != nil && ref.Kind == "bead" && ref.ID != "" {
		b, err := lookup(ref.ID)
		if err != nil {
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

func sendNudgeDeadLetter(cityPath string, item queuedNudge) error {
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		return fmt.Errorf("loading dead-letter mail configuration: %w", err)
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	lookup := func(id string) (beads.Bead, error) {
		var opened []beads.Store
		defer func() {
			for _, store := range opened {
				if err := closeBeadStoreHandle(store); err != nil && nudgeWarningWriter != nil {
					fmt.Fprintf(nudgeWarningWriter, "gc nudge: closing escalation lookup store: %v\n", err) //nolint:errcheck
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
	if lookupErr != nil && nudgeWarningWriter != nil {
		fmt.Fprintf(nudgeWarningWriter, "gc nudge: dead-letter item %q: %v; using session.park_alert_to=%q\n", item.ID, lookupErr, to) //nolint:errcheck
	}
	name := os.Getenv("GC_MAIL")
	if name == "" {
		name = mailProviderNameForCity(cityPath)
	}
	var workStore, msgStore, sessStore beads.Store
	if !strings.HasPrefix(name, "exec:") && name != "fake" && name != "fail" {
		workStore, err = openCityStoreAt(cityPath)
		if err != nil {
			return fmt.Errorf("opening dead-letter mail store: %w", err)
		}
		defer closeBeadStoreHandle(workStore) //nolint:errcheck // send result remains authoritative
		msgStore = resolveMailMessagesStore(cliStorageRoutes(cityPath), workStore, cfg, cityPath, nil)
		sessStore = cliSessionStore(workStore, cfg, cityPath)
	}
	mailer := newMailProviderNamedWithSessionStore(name, msgStore, sessStore, false)
	subject, body := nudgeDeadLetterMail(item)
	_, err = mailer.Send("nudge-dispatcher", to, subject, body)
	return err
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
	return subject, body
}
