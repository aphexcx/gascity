package main

import (
	"fmt"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// SessionLaunchBuilder returns the entry-time launch capability. The accepted
// config snapshot and all expensive inputs are read only when the closure runs.
func (cs *controllerState) SessionLaunchBuilder() worker.SessionRuntimeResolver {
	return func(info session.Info, kind string, metadata map[string]string) (*worker.ResolvedRuntime, error) {
		return buildWorkerEntryLaunch(cs.CityPath(), cs.Config(), cs.SessionsBeadStore().Store, cs.SessionProvider(), info, kind, metadata, false)
	}
}

func workerEntryLaunchBuilder(cityPath string, cfg *config.City, store beads.Store, sp runtime.Provider) worker.SessionRuntimeResolver {
	if cfg == nil || cityPath == "" {
		return nil
	}
	return func(info session.Info, kind string, metadata map[string]string) (*worker.ResolvedRuntime, error) {
		return buildWorkerEntryLaunch(cityPath, cfg, store, sp, info, kind, metadata, true)
	}
}

func buildWorkerEntryLaunch(cityPath string, cfg *config.City, store beads.Store, sp runtime.Provider, info session.Info, kind string, metadata map[string]string, filterCaller bool) (*worker.ResolvedRuntime, error) {
	base, err := resolvedWorkerRuntimeWithConfigAndMetadata(cityPath, cfg, info, kind, metadata)
	if err != nil || cfg == nil {
		return base, err
	}
	a, found := resolveAgentIdentity(cfg, info.Template, "")
	if !found || !session.UseAgentTemplateForProviderResolution(kind, metadata, info.Provider, a.Provider, found) {
		return base, nil
	}
	params := newAgentBuildParams(cfg.EffectiveCityName(), cityPath, cfg, sp, time.Now(), store, os.Stderr)
	if filterCaller {
		params.passthrough = workerLaunchPassthroughEnv()
	}
	agentCfg, identity := canonicalSessionIdentityWithConfigInfo(cfg, &a, info)
	manual := isManualSessionInfoForAgent(info, &a)
	if manual {
		identity = sessionBeadQualifiedNameInfo(cityPath, &a, cfg.Rigs, info)
		agentCfg = sessionBeadConfigAgent(&a, identity)
	}
	selectedProvider, configuredTransport, err := resolveWorkerRuntimeProviderWithConfigAndMetadata(cfg, info, kind, metadata)
	if err != nil {
		return nil, err
	}
	transport := resolvedWorkerRuntimeTransport(info, selectedProvider, configuredTransport, metadata)
	if transport == "" && startedConfigHashProvesWorkerACPTransport(cityPath, cfg, info, kind, selectedProvider, metadata, configuredTransport) {
		transport = config.SessionTransportACP
	}
	launchAgent := agentCfg.Clone()
	launchAgent.Session = firstNonEmptyGCString(transport, config.SessionTransportTmux)
	if workDir := firstNonEmptyGCString(info.WorkDirCanonical, info.WorkDir); workDir != "" {
		launchAgent.WorkDir = workDir
	}
	agentCfg = &launchAgent
	tp, err := resolveTemplateForSessionBeadInfo(params, agentCfg, identity, nil, info)
	if err != nil {
		return nil, fmt.Errorf("building launch for session %q: %w", info.ID, err)
	}
	if info.SessionOrigin != "" {
		tp.Env["GC_SESSION_ORIGIN"] = info.SessionOrigin
	}
	tp.ManualSession = manual
	tp.ConfiguredNamedIdentity = info.ConfiguredNamedIdentity
	tp.ConfiguredNamedMode = info.ConfiguredNamedMode
	launch, _, err := templateParamsToConfigWithDelivery(tp)
	if err != nil {
		return nil, err
	}
	overrides, err := session.ParseTemplateOverrides(metadata)
	if err != nil {
		return nil, fmt.Errorf("reading launch options for session %q: %w", info.ID, err)
	}
	provider := tp.ResolvedProvider
	if len(overrides) > 0 && len(provider.OptionsSchema) > 0 {
		options := make(map[string]string)
		maps.Copy(options, provider.EffectiveDefaults)
		maps.Copy(options, overrides)
		delete(options, "initial_message")
		args, err := config.ResolveExplicitOptions(provider.OptionsSchema, options)
		if err != nil {
			return nil, fmt.Errorf("building launch options for session %q: %w", info.ID, err)
		}
		launch.Command = config.ReplaceSchemaFlags(launch.Command, provider.OptionsSchema, args)
	}
	if !info.ConfiguredNamedSession && shouldPreserveStoredRuntimeCommandForTransport(info.Command, launch.Command, info.Transport, overrides) {
		launch.Command = info.Command
	}
	resume := session.ProviderResume{ResumeFlag: provider.ResumeFlag, ResumeStyle: provider.ResumeStyle, ResumeCommand: provider.ResumeCommand, SessionIDFlag: provider.SessionIDFlag}
	if base != nil {
		// The base resolver already applies stored fallbacks and per-session
		// schema overrides to the resume template.
		resume = base.Resume
	}
	if info.SessionKey != "" && !session.FirstProviderSessionStart(info) && (resume.ResumeFlag != "" || resume.ResumeCommand != "") {
		launch.PromptSuffix, launch.PromptFlag = "", ""
		launch.Nudge = restartPromptNudge(tp.Prompt, tp.Hints.Nudge)
		delete(launch.Env, startupPromptDeliveredEnv)
		if strings.TrimSpace(tp.Prompt) != "" {
			launch.Env[startupPromptDeliveredEnv] = "1"
		}
	}
	return &worker.ResolvedRuntime{
		Command: launch.Command, WorkDir: launch.WorkDir, Provider: provider.Name,
		SessionEnv: launch.Env, Hints: launch,
		Resume: resume,
	}, nil
}

// Explicit empty values also clear these keys when the runtime inherits the
// calling process's environment. Config-authored destination values still win.
func workerLaunchPassthroughEnv() map[string]string {
	env := passthroughEnv()
	identityKeys := session.RuntimeEnvWithSessionContext(session.Info{}, 0, 0, "")
	for _, key := range []string{"GC_TEMPLATE", "GC_SESSION_ORIGIN", "GC_AGENT", "GC_SPAWN_ORIGIN", "GC_WORKTREE_BEAD_ID", "GC_PACKER_PACK", "GC_DIR", "GC_PRESTART_LOG", "GC_STARTUP_PROMPT_DELIVERED", "GC_READY_PROMPT_PREFIX", "GC_WORKTREE_CLAIM_GUARD"} {
		identityKeys[key] = ""
	}
	for key := range env {
		_, identity := identityKeys[key]
		if identity || strings.HasPrefix(key, "GC_TRIGGER_") {
			env[key] = ""
		}
	}
	return env
}
