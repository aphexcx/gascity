package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// poolIdleRoutedWorkCheck detects a pool template that has gc.routed_to work
// sitting open and unclaimed while a live instance of that same pool is idle
// (holds no current trigger bead) and so could pick the work up right now.
//
// An idle instance alone is not a finding: a pool's min-floor idle workers
// legitimately hold no bead while waiting for routed work to arrive. This
// check only fires when both conditions hold together — idle capacity AND
// unclaimed work already routed to it.
type poolIdleRoutedWorkCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newPoolIdleRoutedWorkCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *poolIdleRoutedWorkCheck {
	return &poolIdleRoutedWorkCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *poolIdleRoutedWorkCheck) Name() string { return "pool-idle-routed-work" }

// CanFix returns false: deciding whether to nudge, investigate, or leave an
// idle instance alone belongs to a human or an order, not this check.
func (c *poolIdleRoutedWorkCheck) CanFix() bool { return false }

// Fix is a no-op. Detection only: it never nudges or reassigns on the
// operator's behalf.
func (c *poolIdleRoutedWorkCheck) Fix(_ *doctor.CheckContext) error { return nil }

// poolIdleRoutedWorkFinding is one pool template, in one store scope, that
// has unclaimed gc.routed_to work sitting beside at least one idle instance.
type poolIdleRoutedWorkFinding struct {
	scope         string
	template      string
	beadIDs       []string
	idleInstances []string
}

func (f poolIdleRoutedWorkFinding) describe() string {
	return fmt.Sprintf("%s pool %s has %d unclaimed routed bead(s) (%s) while %d instance(s) sit idle (%s)",
		f.scope, f.template, len(f.beadIDs), strings.Join(f.beadIDs, ", "), len(f.idleInstances), strings.Join(f.idleInstances, ", "))
}

func (c *poolIdleRoutedWorkCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	if c.cfg == nil {
		return okCheck(c.Name(), "no config available")
	}
	findings, skipped := c.collect()
	if len(findings) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "no pool has unclaimed routed work sitting beside an idle instance")
	}
	details := make([]string, 0, len(findings)+len(skipped))
	for _, f := range findings {
		details = append(details, f.describe())
	}
	details = append(details, skipped...)
	sort.Strings(details)
	if len(findings) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("pool idle-routed-work check skipped %d scope(s)", len(skipped)),
			"fix bead store access, then rerun gc doctor",
			details)
	}
	msg := fmt.Sprintf("%d pool(s) have unclaimed routed work while an instance sits idle", len(findings))
	if len(skipped) > 0 {
		msg = fmt.Sprintf("%s; %d scope(s) skipped", msg, len(skipped))
	}
	return warnCheck(c.Name(),
		msg,
		"nudge the idle instance (gc session nudge <name>) or investigate why it has not claimed the routed work",
		details)
}

// collect scans every in-scope bead store (the city plus every non-suspended,
// path-bearing rig) for pool templates that have both an idle live instance
// and unclaimed gc.routed_to work. It mirrors v2RoutedToNamespaceCheck's scope
// iteration so routed-work sanity checks agree on what "in scope" means.
func (c *poolIdleRoutedWorkCheck) collect() (findings []poolIdleRoutedWorkFinding, skipped []string) {
	scopes := []struct{ label, path string }{{"city", c.cityPath}}
	suspState, _ := loadSuspensionState(fsys.OSFS{}, c.cityPath)
	for _, rig := range c.cfg.Rigs {
		if suspensionstate.EffectiveRigSuspended(suspState, rig.Name, rig.EffectiveSuspendedOnStart()) || strings.TrimSpace(rig.Path) == "" {
			continue
		}
		scopes = append(scopes, struct{ label, path string }{"rig " + rig.Name, rig.Path})
	}
	for _, sc := range scopes {
		if c.newStore == nil || strings.TrimSpace(sc.path) == "" {
			continue
		}
		store, err := c.newStore(sc.path)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: opening bead store: %v", sc.label, err))
			continue
		}
		scopeFindings, err := c.collectStoreFindings(store, sc.label)
		findings = append(findings, scopeFindings...)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: %v", sc.label, err))
		}
	}
	return findings, skipped
}

// collectStoreFindings lists session projections once per store and open work
// once when any eligible pool is idle. Templates are matched in memory because
// session.Store.List already reads the same session-label query for each one.
func (c *poolIdleRoutedWorkCheck) collectStoreFindings(store beads.Store, label string) ([]poolIdleRoutedWorkFinding, error) {
	templates := make(map[string]bool)
	for i := range c.cfg.Agents {
		agent := &c.cfg.Agents[i]
		if !agent.Suspended && agent.SupportsGenericEphemeralSessions() && agent.QualifiedName() != "" {
			templates[agent.QualifiedName()] = true
		}
	}
	if len(templates) == 0 {
		return nil, nil
	}
	sessions, err := cliSessionFrontDoor(store, c.cfg, c.cityPath).List("", "")
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	idleByTemplate := make(map[string][]string)
	for _, info := range sessions {
		if !templates[info.Template] || !poolSessionIsLiveInfo(info) || strings.TrimSpace(info.TriggerBeadID) != "" {
			continue
		}
		name := strings.TrimSpace(info.SessionName)
		if name == "" {
			name = info.ID
		}
		idleByTemplate[info.Template] = append(idleByTemplate[info.Template], name)
	}
	if len(idleByTemplate) == 0 {
		return nil, nil
	}

	// Live preserves bd's raw --status=open filter before blocked/deferred rows
	// are collapsed to open. Both tiers remain necessary for relocated stores.
	items, err := beads.HandlesFor(store).Live.List(beads.ListQuery{
		Status:   "open",
		TierMode: beads.FederatedReadTier,
	})
	if err != nil {
		return nil, fmt.Errorf("listing routed work: %w", err)
	}
	workByTemplate := make(map[string][]string)
	for _, b := range items {
		template := b.Metadata[beadmeta.RoutedToMetadataKey]
		if len(idleByTemplate[template]) == 0 || strings.TrimSpace(b.Assignee) != "" || b.Status != "open" {
			continue
		}
		workByTemplate[template] = append(workByTemplate[template], b.ID)
	}
	var findings []poolIdleRoutedWorkFinding
	for template, beadIDs := range workByTemplate {
		idle := idleByTemplate[template]
		sort.Strings(beadIDs)
		sort.Strings(idle)
		findings = append(findings, poolIdleRoutedWorkFinding{
			scope: label, template: template, beadIDs: beadIDs, idleInstances: idle,
		})
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].template < findings[j].template })
	return findings, nil
}
