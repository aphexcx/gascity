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
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "replay ITEM",
		Short: "Re-enqueue a dead-letter nudge once",
		Long: `Re-enqueue a dead-letter nudge's preserved input as a new item for the same session.

Resolve the pane input or delivery failure before replaying. The original
stays dead with the new item's ID recorded on it; a second replay is refused.
The new item retains the original source, reference and session fence, and
uses the normal delivery bounds. Prints the new item ID on success.

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
			if !dryRun {
				var opened beads.Store
				store, opened, err = openNudgeBeadStoreOwned(cityPath)
				if err != nil {
					return err
				}
				defer closeBeadStoreHandle(opened) //nolint:errcheck // enqueue reports its write outcome
			}
			if doNudgeReplay(cityPath, store, args[0], dryRun, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview the replay without changing the queue")
	return cmd
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
	fmt.Fprintln(stdout, item.ID) //nolint:errcheck
	return 0
}
