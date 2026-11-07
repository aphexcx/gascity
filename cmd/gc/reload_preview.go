package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

type reloadChange struct {
	Kind     string   `json:"kind"`
	Name     string   `json:"name"`
	Template string   `json:"template,omitempty"`
	Action   string   `json:"action"`
	Reasons  []string `json:"reasons"`
}

type reloadApplyInfo struct {
	Source reloadSource `json:"source"`
	At     time.Time    `json:"at"`
}

func (cr *CityRuntime) reloadRevision() string {
	cr.serviceStateMu.RLock()
	defer cr.serviceStateMu.RUnlock()
	return cr.configRev
}

func (cr *CityRuntime) noChangeReloadMessage() string {
	message := fmt.Sprintf("No config changes detected. (rev %s)", shortRev(cr.configRev))
	if cr.lastConfigApply.Source == reloadSourceWatch && !cr.lastConfigApply.At.IsZero() {
		message += fmt.Sprintf("; already applied by the watcher at %s, rev %s",
			cr.lastConfigApply.At.UTC().Format("15:04:05Z"), shortRev(cr.configRev))
	}
	return message
}

func (cr *CityRuntime) previewReloadChanges(cfg *config.City, soft bool) ([]reloadChange, error) {
	changes := make([]reloadChange, 0)
	if cr.svc != nil {
		for _, change := range cr.svc.PreviewReload(cfg) {
			changes = append(changes, reloadChange{Kind: "service", Name: change.Name, Action: change.Action, Reasons: change.Reasons})
		}
	}
	runtimeChanges, err := cr.reloadRuntimeChanges(cfg)
	changes = append(changes, runtimeChanges...)
	if err != nil {
		return changes, err
	}
	replaced := make(map[string]bool, len(runtimeChanges))
	for _, change := range runtimeChanges {
		replaced[change.Name] = true
	}
	snapshot, err := loadSessionBeadSnapshot(cr.sessionsBeadStore().Store)
	if err != nil {
		return changes, fmt.Errorf("previewing session changes: %w", err)
	}
	infos := snapshot.OpenInfos()
	if len(infos) == 0 {
		return changes, nil
	}
	bp := newAgentBuildParams(cr.cityName, cr.cityPath, cfg, cr.sp, time.Time{}, nil, cr.stderr)
	desired, err := resolveReloadSessionTemplates(bp, cfg, infos)
	if err != nil {
		return changes, err
	}
	for _, change := range reloadSessionChanges(infos, desired, cfg, soft) {
		if !replaced[change.Name] {
			changes = append(changes, change)
		}
	}
	return changes, nil
}

func (cr *CityRuntime) reloadRuntimeChanges(cfg *config.City) ([]reloadChange, error) {
	if cr.sp == nil || cr.cfg == nil {
		return nil, nil
	}
	oldName, newName := cr.cfg.Session.Provider, cfg.Session.Provider
	if override := os.Getenv("GC_SESSION"); override != "" {
		oldName, newName = override, override
	}
	reason := ""
	if oldName != newName {
		reason = fmt.Sprintf("session provider changed: %s → %s", displayProviderName(oldName), displayProviderName(newName))
	} else if packRuntimeDeclarationChanged(cr.cfg, cfg, newName) {
		oldRuntime, oldOK := cr.cfg.Runtimes[newName]
		newRuntime, newOK := cfg.Runtimes[newName]
		switch {
		case oldOK != newOK:
			reason = "pack runtime declaration changed: " + newName
		case oldRuntime.Command != newRuntime.Command:
			reason = "pack runtime command changed: " + newName
		default:
			reason = "pack runtime protocol changed: " + newName
		}
	}
	if reason == "" {
		return nil, nil
	}
	running, err := cr.sp.ListRunning("")
	if err != nil {
		return nil, fmt.Errorf("previewing runtime replacement: %w", err)
	}
	sort.Strings(running)
	changes := make([]reloadChange, 0, len(running))
	for _, name := range running {
		changes = append(changes, reloadChange{Kind: "session", Name: name, Action: "stop and restart", Reasons: []string{reason}})
	}
	return changes, nil
}

func resolveReloadSessionTemplates(bp *agentBuildParams, cfg *config.City, infos []session.Info) (map[string]TemplateParams, error) {
	local := *bp
	bp = &local
	bp.readOnly = true
	desired := make(map[string]TemplateParams, len(infos))
	for _, info := range infos {
		template := normalizedSessionTemplateInfo(info, cfg)
		agent := findAgentByTemplate(cfg, template)
		if agent == nil || agent.Suspended || info.Closed || info.SessionNameMetadata == "" {
			continue
		}
		resolveAgent, qualifiedName := canonicalSessionIdentityWithConfigInfo(cfg, agent, info)
		if isManualSessionInfoForAgent(info, agent) {
			qualifiedName = sessionBeadQualifiedNameInfo(bp.cityPath, agent, cfg.Rigs, info)
			resolveAgent = sessionBeadConfigAgent(agent, qualifiedName)
		}
		tp, err := resolveTemplateForSessionBeadInfo(bp, resolveAgent, qualifiedName, buildFingerprintExtra(resolveAgent), info)
		if err != nil {
			return nil, fmt.Errorf("previewing session %q: %w", info.SessionNameMetadata, err)
		}
		tp.ManualSession = isManualSessionInfoForAgent(info, agent)
		desired[info.SessionNameMetadata] = tp
	}
	return desired, nil
}

func reloadSessionChanges(infos []session.Info, desired map[string]TemplateParams, cfg *config.City, soft bool) []reloadChange {
	var changes []reloadChange
	for _, info := range infos {
		name := strings.TrimSpace(info.SessionNameMetadata)
		if info.Closed || name == "" {
			continue
		}
		template := normalizedSessionTemplateInfo(info, cfg)
		agent := findAgentByTemplate(cfg, template)
		change := reloadChange{Kind: "session", Name: name, Template: template, Action: "drain"}
		switch {
		case agent == nil:
			change.Reasons = []string{"template no longer maps"}
		case agent.Suspended:
			change.Reasons = []string{"template suspended"}
		default:
			tp, ok := desired[name]
			if !ok || info.StartedConfigHash == "" {
				continue
			}
			current := sessionCoreConfigForHashInfo(tp, info)
			if info.StartedConfigHash == runtime.CoreFingerprint(current) {
				continue
			}
			change.Reasons = []string{"template hash changed"}
			if soft {
				change.Action = "accept"
			} else if runtime.IsLegacyOrMismatchedVersion(info.StartedConfigHash) || generatedHookFilesAdded(info.StartedConfigHash, info.CoreHashBreakdown, current) {
				change.Action = "rebaseline"
				change.Reasons = []string{"fingerprint migration"}
			}
		}
		changes = append(changes, change)
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Name < changes[j].Name })
	return changes
}

func (cr *CityRuntime) previewReload(req *reloadRequest, cityRoot string) reloadControlReply {
	reply := reloadControlReply{Outcome: reloadOutcomeFailed, Revision: cr.configRev}
	configName := cr.configName
	if configName == "" {
		configName = cr.cityName
	}
	result, err := tryReloadConfig(cr.tomlPath, configName, cityRoot)
	if result != nil {
		reply.Warnings = result.Warnings
	}
	if err != nil {
		reply.Error = err.Error()
		return reply
	}
	if config.StorageReloadRequiresRestart(cr.cfg, result.Cfg) {
		reply.Error = "config reload: [storage] changed; restart this city to apply it"
		return reply
	}
	applyRuntimeCityIdentity(result.Cfg, cr.cityName)
	reply.Changes, err = cr.previewReloadChanges(result.Cfg, req.soft)
	if err != nil {
		reply.Error = err.Error()
		return reply
	}
	reply.Outcome = reloadOutcomePreview
	reply.TargetRevision = result.Revision
	reply.Message = fmt.Sprintf("Reload preview: current rev %s, target rev %s; no tick run.", shortRev(cr.configRev), shortRev(result.Revision))
	return reply
}

func writeReloadChanges(w io.Writer, changes []reloadChange) {
	if changes == nil {
		return
	}
	services, sessions := 0, 0
	for _, change := range changes {
		if change.Kind == "service" {
			services++
		} else {
			sessions++
		}
	}
	fmt.Fprintf(w, "reload plan: %d service(s), %d session(s)\n", services, sessions) //nolint:errcheck // best-effort output
	for _, change := range changes {
		fmt.Fprintf(w, "  %s %s: %s (%s)\n", change.Kind, change.Name, change.Action, strings.Join(change.Reasons, "; ")) //nolint:errcheck // best-effort output
	}
	if sessions > 0 {
		fmt.Fprintln(w, "  Session drains remain subject to runtime activity and work guards; new pool allocations are decided during the tick.") //nolint:errcheck // best-effort output
	}
}

func softReloadAcceptanceMessage(count int, names []string) string {
	message := fmt.Sprintf("soft reload: accepted config drift on %d session(s)", count)
	if len(names) > 0 {
		message += ": " + strings.Join(names, ", ")
	}
	return message
}

func reloadReplyMessage(reply reloadControlReply) string {
	message := strings.TrimSpace(reply.Message)
	if reply.Revision != "" && !strings.Contains(message, shortRev(reply.Revision)) {
		message += fmt.Sprintf(" (rev %s)", shortRev(reply.Revision))
	}
	return strings.TrimSpace(message)
}

type reloadFailureJSON struct {
	cliJSONErrorOutput
	Revision string         `json:"revision,omitempty"`
	Outcome  reloadOutcome  `json:"outcome"`
	Changes  []reloadChange `json:"changes,omitempty"`
	Warnings []string       `json:"warnings,omitempty"`
}

func writeReloadFailureJSON(stdout, stderr io.Writer, reply reloadControlReply) int {
	message := strings.TrimSpace(reply.Error)
	if message == "" {
		message = reloadReplyMessage(reply)
	}
	if message == "" {
		message = "reload " + string(reply.Outcome)
	}
	code := "reload_" + string(reply.Outcome)
	payload := reloadFailureJSON{
		cliJSONErrorOutput: cliJSONErrorOutput{
			SchemaVersion: "1", OK: false,
			Error: cliJSONError{Code: code, Message: message, ExitCode: 1},
		},
		Revision: reply.Revision, Outcome: reply.Outcome, Changes: reply.Changes, Warnings: reply.Warnings,
	}
	if exitCode := writeCLIJSONLineOrExit(stdout, stderr, "gc reload", payload); exitCode != 0 {
		return exitCode
	}
	writeJSONDiagnostic(stderr, "error", code, message, 1)
	return 1
}
