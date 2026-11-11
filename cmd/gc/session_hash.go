package main

import (
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
	if hasObservedStartupContentRepair(info) {
		applyStartupContentFingerprint(&agentCfg)
	}
	return agentCfg
}

// Sources and Skipped record delivery observations even before finalization.
// A clean Pending receipt alone establishes no repair obligation.
func hasObservedStartupContentRepair(info sessionpkg.Info) bool {
	content, err := sessionpkg.DecodeStartupContent(info.StartupContent)
	return err == nil && (content.Skipped || len(content.Sources) > 0)
}
