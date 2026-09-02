package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// federationIdentity is this city's [federation] identity as the cross-city
// claim fence reads it (federation.MayClaim): the trimmed config value, or ""
// on a nil or non-federated config, which turns the fence off everywhere it is
// applied — the hook claim (hookClaimOptions.FederationIdentity), the
// reconciler's orphan release (releaseOrphanedPoolAssignments) and the
// retired-session re-home (reassignWorkAssignedToRetiredSessionBead).
func federationIdentity(cfg *config.City) string {
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Federation.Identity)
}
