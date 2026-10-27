package main

import (
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime/lanectx"
	"github.com/gastownhall/gascity/internal/shellquote"
)

func hasLaneContext(a *config.Agent, sessionProvider, cityRoot, rigRoot, workDir string) bool {
	switch effectiveSessionProvider(a.Session, sessionProvider) {
	case "", "tmux", "herdr":
	default:
		return false
	}
	return a.WorkDir != "" && len(a.PreStart) > 0 &&
		filepath.IsAbs(workDir) && filepath.Clean(workDir) != filepath.Clean(cityRoot) &&
		(rigRoot == "" || filepath.Clean(workDir) != filepath.Clean(rigRoot))
}

func laneContextPrompt(env map[string]string, environmentFile string) string {
	binding := "Store binding: `" + env["BEADS_DIR"] + "`; the lane does not need its own `.beads` directory.\n"
	if env["BEADS_DIR"] == "" {
		binding = "Store binding: city store at `" + env["GC_CITY_PATH"] + "`.\n"
	}
	prompt := "## Session workspace paths\n\n```sh\n" + lanectx.LaneEnvironmentScript(env) + "```\n\n" + binding
	if environmentFile != "" {
		prompt += "If a tool shell filters these variables, restore them with `. " + shellquote.Quote(environmentFile) + "`.\n"
	}
	if env["GC_PRESTART_LOG"] != "" {
		prompt += "Pre-start diagnostics: `GC_PRESTART_LOG` above (append-only, capped at 1 MiB).\n"
	}
	return prompt
}

func primeLaneContextSuffix(a *config.Agent, cfg *config.City, ctx PromptContext) string {
	if !hasLaneContext(a, cfg.Session.Provider, ctx.CityRoot, ctx.RigRoot, ctx.WorkDir) {
		return ""
	}
	env := map[string]string{
		"GC_CITY_PATH": ctx.CityRoot, "GC_DIR": ctx.WorkDir,
		"GC_RIG_ROOT": ctx.RigRoot, "GC_RIG": ctx.RigName,
	}
	if ctx.RigName != "" {
		env["BEADS_DIR"] = filepath.Join(ctx.RigRoot, ".beads")
	}
	// A pre-upgrade session may not have materialized either artifact yet.
	// Keep its resolved paths visible without advertising nonexistent files.
	logPath := lanectx.LanePreStartLogPath(ctx.WorkDir)
	if info, err := os.Stat(logPath); err == nil && info.Mode().IsRegular() {
		env["GC_PRESTART_LOG"] = logPath
	}
	environmentFile := lanectx.LaneEnvironmentPath(ctx.WorkDir)
	if info, err := os.Stat(environmentFile); err != nil || !info.Mode().IsRegular() {
		environmentFile = ""
	}
	return "\n\n" + laneContextPrompt(env, environmentFile)
}
