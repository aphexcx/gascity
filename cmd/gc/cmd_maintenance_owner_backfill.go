package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/federation"
)

// The two buckets `gc maintenance owner-backfill` sorts unlabeled work into.
// There is no THEIRS bucket on purpose: a bead this city cannot attribute to
// itself is not thereby attributed to another city, and the convention makes
// unlabeled legacy work claimable by anyone, so the honest name for the rest
// is "theirs or unknown" and the honest action is to leave it alone.
const (
	ownerBackfillOurs     = "OURS"
	ownerBackfillUnknown  = "THEIRS-OR-UNKNOWN"
	ownerBackfillNoSignal = "no attributable signal"
)

// ownerBackfillRow is one unlabeled open bead with the bucket it landed in and
// the rule that put it there, so a dry run shows its reasoning per row.
type ownerBackfillRow struct {
	Bead   beads.Bead
	Bucket string
	Rule   string
}

// ownerBackfillRows sorts the open and in-progress beads that carry no owner
// label into OURS and THEIRS-OR-UNKNOWN. The only signals it trusts are the
// ones this city minted itself: an assignee that is a session id carrying this
// city's HQ prefix (session ids are city-scoped, so "<prefix>-" is this city's
// and no other's), or a pool: label whose target resolves in this city's
// config. Everything else — an unassigned bead, a foreign session prefix, a
// bare <rig>/<agent> assignee that every federated city could have written
// with the same packs — stays unlabeled. created_by is never consulted: it is
// "mayor" in every city. Labeled and closed beads are not candidates at all,
// and the store's order is kept so the output is stable across runs.
func ownerBackfillRows(items []beads.Bead, hqPrefix string, poolResolves func(target string) bool) []ownerBackfillRow {
	sessionPrefix := ""
	if hqPrefix = strings.TrimSpace(hqPrefix); hqPrefix != "" {
		sessionPrefix = hqPrefix + "-"
	}
	rows := make([]ownerBackfillRow, 0, len(items))
	for _, b := range items {
		switch b.Status {
		case "open", "in_progress":
		default:
			continue
		}
		if federation.HasOwnerLabel(b.Labels) {
			continue
		}
		row := ownerBackfillRow{Bead: b, Bucket: ownerBackfillUnknown, Rule: ownerBackfillNoSignal}
		if assignee := strings.TrimSpace(b.Assignee); sessionPrefix != "" && strings.HasPrefix(assignee, sessionPrefix) {
			row.Bucket = ownerBackfillOurs
			row.Rule = fmt.Sprintf("assignee carries this city's session prefix %q", sessionPrefix)
		} else if label, ok := ownerBackfillResolvedPoolLabel(b.Labels, poolResolves); ok {
			row.Bucket = ownerBackfillOurs
			row.Rule = fmt.Sprintf("pool label %q resolves in this city", label)
		}
		rows = append(rows, row)
	}
	return rows
}

// ownerBackfillResolvedPoolLabel returns the first pool: label whose target is
// an agent this city's config resolves, if any.
func ownerBackfillResolvedPoolLabel(labels []string, poolResolves func(target string) bool) (string, bool) {
	if poolResolves == nil {
		return "", false
	}
	for _, label := range labels {
		if target, ok := strings.CutPrefix(label, "pool:"); ok && poolResolves(target) {
			return label, true
		}
	}
	return "", false
}

func newMaintenanceOwnerBackfillCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		rigName string
		apply   bool
	)
	cmd := &cobra.Command{
		Use:   "owner-backfill",
		Short: "Label this city's legacy beads with owner:<identity> (dry run by default)",
		Long: `Backfill the federation owner label onto beads created before this city set
[federation] identity in city.toml.

Every open or in_progress bead in the store that carries no owner:* label is
listed in one of two buckets with the rule that decided it:

  OURS               the assignee is a session id carrying this city's HQ
                     prefix, or a pool: label names an agent this city's
                     config resolves
  THEIRS-OR-UNKNOWN  nothing this city minted itself points at the bead

The default is a dry run: nothing is written. --apply adds owner:<identity>
to the OURS bucket only, one line per bead changed, and is idempotent: a
labeled bead is no longer a candidate. THEIRS-OR-UNKNOWN is never touched —
unlabeled legacy work stays claimable by anyone, per the federation
convention — and closed beads are never read. The city (HQ) store is the
default scope; --rig backfills one rig's store instead. The command refuses
(exit 2) when [federation] identity is unset.

It runs against the store directly, unlike 'status' and 'dolt-gc', which
route through the supervisor.`,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return exitForCode(cmdMaintenanceOwnerBackfill(rigName, apply, stdout, stderr))
		},
	}
	cmd.Flags().StringVar(&rigName, "rig", "", "backfill this rig's store instead of the city (HQ) store")
	cmd.Flags().BoolVar(&apply, "apply", false, "write owner:<identity> onto the OURS bucket (default: dry run)")
	return cmd
}

func cmdMaintenanceOwnerBackfill(rigName string, apply bool, stdout, stderr io.Writer) int {
	const cmdName = "gc maintenance owner-backfill"
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return 2
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "%s: loading config: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return 2
	}
	owner, federated := cfg.Federation.OwnerLabel()
	if !federated {
		fmt.Fprintf(stderr, "%s: [federation] identity is not set in city.toml; there is no owner to backfill\n", cmdName) //nolint:errcheck // best-effort stderr
		return 2
	}
	target := bdCityScopeTarget(cityPath, cfg)
	if rigName = strings.TrimSpace(rigName); rigName != "" {
		rig, ok := rigByName(cfg, rigName)
		if !ok {
			fmt.Fprintf(stderr, "%s: unknown rig %q\n", cmdName, rigName) //nolint:errcheck // best-effort stderr
			return 2
		}
		target = bdRigScopeTarget(cityPath, rig)
	}
	store, err := openStoreAtForCityWithConfig(target.ScopeRoot, cityPath, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: opening %s store: %v\n", cmdName, scopeLabel(target), err) //nolint:errcheck // best-effort stderr
		return 1
	}
	items, err := ownerBackfillCandidates(store)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %s store: %v\n", cmdName, scopeLabel(target), err) //nolint:errcheck // best-effort stderr
		return 1
	}
	hqPrefix := config.EffectiveHQPrefix(cfg)
	rows := ownerBackfillRows(items, hqPrefix, func(target string) bool { return config.FindAgent(cfg, target) != nil })

	mode := "dry run"
	if apply {
		mode = "apply"
	}
	fmt.Fprintf(stdout, "owner-backfill (%s): scope %s, identity %s, session prefix %q\n", mode, scopeLabel(target), strings.TrimPrefix(owner, federation.OwnerLabelPrefix), hqPrefix+"-") //nolint:errcheck // best-effort stdout

	ours, unknown := 0, 0
	for _, row := range rows {
		if row.Bucket == ownerBackfillOurs {
			ours++
		} else {
			unknown++
		}
	}
	if !apply {
		for _, row := range rows {
			fmt.Fprintf(stdout, "%s\t%s\tassignee=%s\t%s\n", row.Bucket, row.Bead.ID, ownerBackfillAssignee(row.Bead), row.Rule) //nolint:errcheck // best-effort stdout
		}
		fmt.Fprintf(stdout, "%d %s, %d %s; dry run, nothing written (pass --apply to label %s)\n", ours, ownerBackfillOurs, unknown, ownerBackfillUnknown, ownerBackfillOurs) //nolint:errcheck // best-effort stdout
		return 0
	}

	labeled, skipped, failed := applyOwnerBackfill(store, rows, owner, hqPrefix, func(target string) bool { return config.FindAgent(cfg, target) != nil }, stdout, stderr)
	summary := fmt.Sprintf("%d labeled %s, %d left unlabeled (%s)", labeled, owner, unknown, ownerBackfillUnknown)
	if skipped > 0 {
		summary += fmt.Sprintf(", %d skipped (changed since the list)", skipped)
	}
	fmt.Fprintln(stdout, summary) //nolint:errcheck // best-effort stdout
	if failed > 0 {
		fmt.Fprintf(stderr, "%s: %d bead(s) could not be labeled\n", cmdName, failed) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

// applyOwnerBackfill labels the OURS rows, re-reading each bead live first.
// The rows are a snapshot: between the list and the write a bead can close,
// gain an owner from the other city's sync, or lose the signal that made it
// OURS, and a blind add would then label a closed bead or put two owners on
// one. A bead that no longer classifies OURS on the live read is skipped with
// the reason. When the store proves a revision for the re-read (a positive
// one — a zero revision is "unknown", not "zero") and can write conditionally,
// the write is fenced on it, so a change between the re-read and the write is
// a skip too; otherwise the re-read is the check and the add-if-absent label
// path is the write.
func applyOwnerBackfill(store beads.Store, rows []ownerBackfillRow, owner, hqPrefix string, poolResolves func(target string) bool, stdout, stderr io.Writer) (labeled, skipped, failed int) {
	const cmdName = "gc maintenance owner-backfill"
	conditional, _ := beads.ConditionalWriterFor(store)
	for _, row := range rows {
		if row.Bucket != ownerBackfillOurs {
			continue
		}
		id := row.Bead.ID
		fresh, err := store.Get(id)
		if err != nil {
			failed++
			fmt.Fprintf(stderr, "%s: re-reading %s: %v\n", cmdName, id, err) //nolint:errcheck // best-effort stderr
			continue
		}
		if reason, ok := ownerBackfillStillOurs(fresh, hqPrefix, poolResolves); !ok {
			skipped++
			fmt.Fprintf(stdout, "skipped %s: %s\n", id, reason) //nolint:errcheck // best-effort stdout
			continue
		}
		opts := beads.UpdateOpts{Labels: []string{owner}}
		if conditional != nil && fresh.Revision > 0 {
			err = conditional.UpdateIfMatch(id, fresh.Revision, opts)
			var precondition *beads.PreconditionFailedError
			switch {
			case errors.As(err, &precondition):
				skipped++
				fmt.Fprintf(stdout, "skipped %s: changed while labeling (revision %d is now %d)\n", id, precondition.Expected, precondition.Current) //nolint:errcheck // best-effort stdout
				continue
			case errors.Is(err, beads.ErrConditionalWriteUnsupported):
				err = store.Update(id, opts)
			}
		} else {
			err = store.Update(id, opts)
		}
		if err != nil {
			failed++
			fmt.Fprintf(stderr, "%s: labeling %s: %v\n", cmdName, id, err) //nolint:errcheck // best-effort stderr
			continue
		}
		labeled++
		fmt.Fprintf(stdout, "labeled %s %s\n", id, owner) //nolint:errcheck // best-effort stdout
	}
	return labeled, skipped, failed
}

// ownerBackfillStillOurs re-classifies one live bead with the rule the dry run
// used and names what changed when it no longer qualifies.
func ownerBackfillStillOurs(fresh beads.Bead, hqPrefix string, poolResolves func(target string) bool) (string, bool) {
	rows := ownerBackfillRows([]beads.Bead{fresh}, hqPrefix, poolResolves)
	switch {
	case len(rows) == 0:
		if federation.HasOwnerLabel(fresh.Labels) {
			return "now carries " + federation.OwnerLabelPrefix + federation.OwnerOf(fresh.Labels), false
		}
		return "now " + fresh.Status, false
	case rows[0].Bucket != ownerBackfillOurs:
		return "no longer attributable (" + rows[0].Rule + ")", false
	}
	return "", true
}

// ownerBackfillCandidates reads the open and in_progress beads live: a
// maintenance write must see the store as it is, not a cached projection.
func ownerBackfillCandidates(store beads.Store) ([]beads.Bead, error) {
	var items []beads.Bead
	for _, status := range []string{"open", "in_progress"} {
		got, err := store.List(beads.ListQuery{Status: status, TierMode: beads.FederatedReadTier, Live: true})
		if err != nil {
			return nil, fmt.Errorf("listing %s beads: %w", status, err)
		}
		items = append(items, got...)
	}
	return items, nil
}

func ownerBackfillAssignee(b beads.Bead) string {
	if assignee := strings.TrimSpace(b.Assignee); assignee != "" {
		return assignee
	}
	return "-"
}
