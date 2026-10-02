package main

import (
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/federation"
)

func convoyAutocloseIdentity(cfg *config.City) string {
	if cfg == nil {
		return ""
	}
	return cfg.Federation.Identity
}

// Automatic closure requires ownership, even when a handoff permits a peer
// to claim the work. Unlabeled legacy convoys retain their original behavior.
func convoyAutocloseOwnerMatches(labels []string, identity string) bool {
	for _, owner := range federation.Owners(labels) {
		if identity == "" || owner != identity {
			return false
		}
	}
	return true
}
