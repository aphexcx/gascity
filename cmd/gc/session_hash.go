package main

import (
	"fmt"
	"maps"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// sessionCoreConfigWithOverrides builds the canonical config used for session
// core hashes from a typed session.Info. Drift comparisons and rebaselines share
// this helper so template_overrides participate in the fingerprint everywhere. Start
// paths may keep their pre-start assembly inline when they need setup-specific
// diagnostics before storing first-start metadata.
func sessionCoreConfigWithOverrides(tp TemplateParams, info sessionpkg.Info) runtime.Config {
	agentCfg := templateParamsToConfig(tp)
	applyTemplateOverridesToConfigInfo(&agentCfg, info, tp)
	return agentCfg
}

func sessionCoreConfigForHashInfo(tp TemplateParams, info sessionpkg.Info) runtime.Config {
	return (&startupContentCheck{}).coreConfigForHashInfo(tp, info)
}

func (check *startupContentCheck) coreConfigForHashInfo(tp TemplateParams, info sessionpkg.Info) runtime.Config {
	agentCfg := sessionCoreConfigWithOverrides(tp, info)
	check.retainFingerprint(&agentCfg, info)
	return agentCfg
}

// Rebaselines accept config without delivering content. Carry an observed repair
// regardless of current disk readiness, without probing the cache at all.
func sessionCoreConfigForRebaselineInfo(tp TemplateParams, info sessionpkg.Info) runtime.Config {
	agentCfg := sessionCoreConfigWithOverrides(tp, info)
	return startupContentConfigForRebaseline(agentCfg, info)
}

func startupContentConfigForRebaseline(agentCfg runtime.Config, info sessionpkg.Info) runtime.Config {
	// The config may have been built from a snapshot before the delivery commit.
	// Only the record read at the write boundary supplies this extra input.
	agentCfg.FingerprintExtra = maps.Clone(agentCfg.FingerprintExtra)
	delete(agentCfg.FingerprintExtra, "startup-content")
	if hasObservedStartupContentRepair(info) {
		applyStartupContentFingerprint(&agentCfg)
	}
	return agentCfg
}

// Re-read only when a writer is about to rebaseline. Pending records belong to
// the start's commit/recovery; accepting them would acknowledge an unresolved
// delivery. This uses the tick's store and never probes the import cache.
func sessionInfoAtRebaseline(sessFront *sessionpkg.Store, id string) (sessionpkg.Info, bool, error) {
	info, err := sessFront.Get(id)
	if err != nil {
		return info, false, err
	}
	content, err := sessionpkg.DecodeStartupContent(info.StartupContent)
	if err != nil {
		return info, false, fmt.Errorf("decoding startup content for session %s: %w", id, err)
	}
	return info, !content.Pending, nil
}

// Sources and Skipped record delivery observations even before finalization.
// A clean Pending receipt alone establishes no repair obligation.
func hasObservedStartupContentRepair(info sessionpkg.Info) bool {
	content, err := sessionpkg.DecodeStartupContent(info.StartupContent)
	return err == nil && (content.Skipped || len(content.Sources) > 0)
}
