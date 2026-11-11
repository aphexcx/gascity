package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// closedPoolSessionNameWarnings checks the next fresh slot with the same name
// derivation and reservation rules as pool creation. It never allocates a bead.
func closedPoolSessionNameWarnings(cfg *config.City, cityPath string, agents []config.Agent, cityStore beads.Store, openStore func(string) (beads.Store, error)) ([]string, error) {
	if cfg == nil || cityStore == nil || len(agents) == 0 {
		return nil, nil
	}
	suspended := buildSuspendedRigPathsForCity(cfg, cityPath)
	rigStores := make(map[string]beads.Store)
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" || suspended[filepath.Clean(rig.Path)] {
			continue
		}
		if openStore == nil {
			return nil, fmt.Errorf("opening rig %q session census: store opener unavailable", rig.Name)
		}
		store, err := openStore(rig.Path)
		if err != nil {
			return nil, fmt.Errorf("opening rig %q session census: %w", rig.Name, err)
		}
		if store == nil {
			return nil, fmt.Errorf("opening rig %q session census: store unavailable", rig.Name)
		}
		rigStores[rig.Name] = store
	}
	infos, err := collectAllOpenSessionInfos(cityPath, cfg, cityStore, rigStores, suspended)
	if err != nil {
		return nil, err
	}
	store := cliSessionStore(cityStore, cfg, cityPath)
	bp := &agentBuildParams{
		city: cfg, cityName: loadedCityName(cfg, cityPath), cityPath: cityPath, rigs: cfg.Rigs,
		sessionOccupancyInfos: infos,
	}
	var warnings []string
	for _, a := range agents {
		slot, err := claimFreshPoolSlotInfo(bp, &a, map[int]bool{})
		if err != nil {
			// A disabled or occupied pool has no fresh slot to diagnose.
			continue
		}
		_, identity, poolSlot := poolDesiredRequestIdentity(&a, slot)
		alias, err := bp.resolveTmuxAliasForAgent(&a)
		if err != nil {
			return warnings, err
		}
		identifiers, err := derivePoolSessionIdentifiers(cfg, a.QualifiedName(), poolSessionCreateIdentity{
			AgentName: identity, Slot: poolSlot, TransientSlot: usesTransientPoolSlotIdentity(&a),
		}, alias)
		if err != nil {
			return warnings, err
		}
		for _, name := range identifiers.availabilityNames {
			err := session.EnsureSessionNameAvailableWithConfigForOwner(store, cfg, name, "", identity)
			var owner *session.NameOwnerError
			if errors.As(err, &owner) && owner.Closed {
				warnings = append(warnings, fmt.Sprintf("pool %q cannot create its next session: name %q is held by closed session bead %s", a.QualifiedName(), name, owner.BeadID))
			} else if err != nil && !errors.Is(err, session.ErrSessionNameExists) {
				return warnings, err
			}
		}
	}
	return warnings, nil
}

func printClosedPoolSessionNameWarnings(opts slingOpts, deps slingDeps, stderr io.Writer) {
	if opts.Force || deps.Cfg == nil {
		return
	}
	target := opts.Target
	route := agentutil.NormalizePoolRouteTarget(deps.Cfg, agentutil.RoutedToIdentity(&target))
	if route != target.QualifiedName() {
		template, ok := findAgentByQualified(deps.Cfg, route)
		if !ok {
			fmt.Fprintf(stderr, "warning: checking pool session names: pool template %q is not configured\n", route) //nolint:errcheck
			return
		}
		target = template
	}
	store := deps.Store
	if strings.HasPrefix(deps.StoreRef, "rig:") && deps.CityPath != "" {
		// Pool sessions are created in the city's session store, even when
		// the dispatched work belongs to a rig store.
		cityStore, err := slingOpenCityStore(deps.CityPath)
		if err != nil {
			fmt.Fprintf(stderr, "warning: checking pool session names: opening city session store: %v\n", err) //nolint:errcheck
			return
		}
		store = cityStore
	}
	warnings, err := closedPoolSessionNameWarnings(deps.Cfg, deps.CityPath, []config.Agent{target}, store, func(path string) (beads.Store, error) {
		return openAuthoritativeStoreAtForCity(path, deps.CityPath)
	})
	for _, warning := range warnings {
		fmt.Fprintf(stderr, "warning: %s\n", warning) //nolint:errcheck
	}
	if err != nil {
		fmt.Fprintf(stderr, "warning: checking pool session names: %v\n", err) //nolint:errcheck
	}
}
