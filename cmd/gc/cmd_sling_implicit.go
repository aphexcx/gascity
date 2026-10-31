package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sling"
)

// guardedImplicitSlingTarget scopes the fork's local dispatch policy to bare
// provider slots. An explicitly configured agent with the same name is a lane.
func guardedImplicitSlingTarget(a config.Agent) bool {
	return a.Implicit && a.Dir != ""
}

// implicitSlingRefusal implements the fork's code-work rule: a non-city prefix
// OR absence of the operational exemption label requires a configured lane.
// Keep this city policy outside the shared sling domain and provider defaults.
func implicitSlingRefusal(cfg *config.City, a config.Agent, b beads.Bead) string {
	if !guardedImplicitSlingTarget(a) || (sling.IsHQPrefix(cfg, beadPrefix(cfg, b.ID)) && slices.Contains(b.Labels, "mayor-tool")) {
		return ""
	}
	var lanes []string
	for _, candidate := range cfg.Agents {
		if !candidate.Implicit && candidate.Dir == a.Dir {
			lanes = append(lanes, candidate.QualifiedName())
		}
	}
	slices.Sort(lanes)
	available := strings.Join(slices.Compact(lanes), ", ")
	if available == "" {
		available = "none; configure a rig agent with its work directory and prompt"
	}
	slot := fmt.Sprintf("%q", a.QualifiedName())
	if base := config.BuiltinFamily(a.Provider, cfg.Providers); base != "" && base != a.Provider {
		slot += fmt.Sprintf(" (base %s%s)", config.BasePrefixBuiltin, base)
	}
	return fmt.Sprintf("gc sling: refusing code work %q on implicit provider slot %s; configured role lanes for rig %q: %s. Use --allow-implicit to opt in", b.ID, slot, a.Dir, available)
}
