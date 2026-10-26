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
	if !a.Implicit || a.Dir == "" {
		return false
	}
	// Numbered pool members retain the implicit slot's provider identity.
	for _, name := range []string{a.Name, a.Provider} {
		if name == "claude" || name == "codex" || name == "codex-astra" {
			return true
		}
	}
	return false
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
	return fmt.Sprintf("gc sling: refusing code work %q on implicit provider slot %q; configured role lanes for rig %q: %s. Use --allow-implicit to opt in", b.ID, a.QualifiedName(), a.Dir, available)
}
