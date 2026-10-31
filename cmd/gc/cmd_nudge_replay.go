package main

import (
	"fmt"
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/spf13/cobra"
)

func newNudgeReplayCmd(stdout, stderr io.Writer) *cobra.Command {
	var dryRun, dismiss bool
	cmd := &cobra.Command{
		Use:   "replay ITEM",
		Short: "Re-enqueue a dead-letter nudge once",
		Long: `Re-enqueue a dead-letter nudge's preserved input as a new item for the same session.

Resolve the pane input or delivery failure before replaying. The original
stays dead with the new item's ID recorded on it; a second replay is refused.
The new item retains the original source, reference and session fence, and
uses the normal delivery bounds. Prints the new item ID on success.

Use --dismiss to record an explicit dismissal without sending the input.
Unresolved items and disposition receipts remain for seven days from the
dead transition. Cap expiry removes the item and reports unresolved input
loss by log and mail. Superseded reminders cannot be replayed and do not
produce failure alerts or unresolved doctor entries.

Use --dry-run to preview without writing. Find dead items with gc doctor
or gc nudge status. Dead transitions send one mail to the referenced bead's
escalation_recipient metadata, falling back to session.park_alert_to. An
unset recipient or mail failure is logged and leaves the doctor row visible.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				return err
			}
			var store beads.NudgesStore
			if !dryRun && !dismiss {
				var opened beads.Store
				store, opened, err = openNudgeBeadStoreOwned(cityPath)
				if err != nil {
					return err
				}
				defer closeBeadStoreHandle(opened) //nolint:errcheck // enqueue reports its write outcome
			}
			var code int
			if dismiss {
				code = doNudgeDismiss(cityPath, args[0], dryRun, stdout, stderr)
			} else {
				code = doNudgeReplay(cityPath, store, args[0], dryRun, stdout, stderr)
			}
			if code != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview the replay without changing the queue")
	cmd.Flags().BoolVar(&dismiss, "dismiss", false, "Dismiss the dead letter without replaying its input")
	return cmd
}

func doNudgeDismiss(cityPath, id string, dryRun bool, stdout, stderr io.Writer) int {
	dismiss := func(state *nudgequeue.State) error {
		return nudgequeue.DismissDeadLetter(state, id, time.Now())
	}
	var err error
	if dryRun {
		var state nudgequeue.State
		state, err = nudgequeue.LoadState(cityPath)
		if err == nil {
			err = dismiss(&state)
		}
	} else {
		err = withNudgeQueueState(cityPath, dismiss)
	}
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge replay --dismiss: %v\n", err) //nolint:errcheck
		return 1
	}
	verb := "Dismissed"
	if dryRun {
		verb = "Would dismiss"
	}
	fmt.Fprintf(stdout, "%s %s without replay\n", verb, id) //nolint:errcheck
	return 0
}

func doNudgeReplay(cityPath string, store beads.NudgesStore, id string, dryRun bool, stdout, stderr io.Writer) int {
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge replay: %v\n", err) //nolint:errcheck
		return 1
	}
	item, err := nudgequeue.PrepareReplay(&state, id, newQueuedNudgeID(), time.Now(), defaultQueuedNudgeTTL)
	if err != nil {
		fmt.Fprintf(stderr, "gc nudge replay: %v\n", err) //nolint:errcheck
		return 1
	}
	if dryRun {
		fmt.Fprintf(stdout, "Would replay %s for session %s (session_id=%q source=%q) with preserved input %q\n", id, item.Agent, item.SessionID, item.Source, item.Message) //nolint:errcheck
		return 0
	}
	if err := enqueueQueuedNudgeWithStore(cityPath, store, item); err != nil {
		fmt.Fprintf(stderr, "gc nudge replay: %v\n", err) //nolint:errcheck
		return 1
	}
	if err := startNudgeReplayPoller(cityPath, store, item); err != nil {
		fmt.Fprintf(stderr, "gc nudge replay: queued %s but could not start its delivery poller: %v\n", item.ID, err) //nolint:errcheck
	}
	fmt.Fprintln(stdout, item.ID) //nolint:errcheck
	return 0
}

func startNudgeReplayPoller(cityPath string, store beads.NudgesStore, item queuedNudge) error {
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		return err
	}
	// Enqueue already signals the supervisor. Legacy cities need the same
	// poller startup as other queue producers, even if the old poller exited.
	if nudgeDispatcherIsSupervisor(cfg) {
		return nil
	}
	if store.Store == nil {
		return fmt.Errorf("session store unavailable")
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	sessStore := cliSessionStore(store.Store, cfg, cityPath)
	id, err := resolveSessionID(sessStore, firstNonEmpty(item.SessionID, item.Agent))
	if err != nil {
		return err
	}
	info, err := sessionFrontDoor(sessStore).Get(id)
	if err != nil {
		return err
	}
	maybeStartNudgePoller(resolveNudgeTargetFromSessionInfo(cityPath, cfg, info))
	return nil
}
