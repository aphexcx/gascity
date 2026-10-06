package doctor

import (
	"fmt"
	"strings"
)

// SupervisorLaunchdSpawnType is what the caller (cmd/gc) read from
// `launchctl print` for the supervisor's launchd job, once per doctor run on
// macOS, so the check stays data-in, data-out and never shells out itself.
type SupervisorLaunchdSpawnType struct {
	// Label is the launchd job label (e.g. "com.gascity.supervisor").
	Label string
	// Loaded reports whether launchctl print found the job.
	Loaded bool
	// Unreadable reports that launchctl print failed without confirming the
	// job is absent, so whether it is loaded, and in which spawn type, is
	// unknown. Detail carries launchctl's output.
	Unreadable bool
	// Detail is launchctl print's output when Unreadable.
	Detail string
	// SpawnType is the job's spawn type as launchctl prints it, lowercased
	// ("interactive", "daemon", ...); empty when the line was absent.
	SpawnType string
}

// SupervisorLaunchdSpawnTypeCheck warns when the supervisor's launchd job is
// not in the interactive spawn type while agents are configured, and when
// launchctl cannot read the job at all. Every session the supervisor spawns
// inherits the job's class, and launchd's default for a job without
// ProcessType is the daemon class, which macOS throttles: on Apple Silicon it
// holds the supervisor and every agent on the efficiency cores at a clamped
// priority (hq-a6cny).
type SupervisorLaunchdSpawnTypeCheck struct {
	probe            SupervisorLaunchdSpawnType
	agentsConfigured bool
}

// NewSupervisorLaunchdSpawnTypeCheck returns a check over a spawn-type probe
// the caller already ran. agentsConfigured gates the spawn-type warning: a
// supervisor with no agents has nothing to starve.
func NewSupervisorLaunchdSpawnTypeCheck(probe SupervisorLaunchdSpawnType, agentsConfigured bool) *SupervisorLaunchdSpawnTypeCheck {
	return &SupervisorLaunchdSpawnTypeCheck{probe: probe, agentsConfigured: agentsConfigured}
}

// Name returns the check identifier.
func (c *SupervisorLaunchdSpawnTypeCheck) Name() string { return "supervisor-launchd-spawn-type" }

// CanFix reports that this check does not support automatic remediation:
// regenerating the plist reloads the supervisor, and moving existing
// sessions to the new class means recreating them.
func (c *SupervisorLaunchdSpawnTypeCheck) CanFix() bool { return false }

// Fix is a no-op; CanFix returns false.
func (c *SupervisorLaunchdSpawnTypeCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns false; this check is not part of the gc start
// warm-up scan.
func (c *SupervisorLaunchdSpawnTypeCheck) WarmupEligible() bool { return false }

// Run reports the supervisor launchd job's spawn type.
func (c *SupervisorLaunchdSpawnTypeCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name(), Status: StatusOK}
	label := c.probe.Label
	switch {
	case c.probe.Unreadable:
		detail := strings.Join(strings.Fields(c.probe.Detail), " ")
		if detail == "" {
			detail = "no output"
		}
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("could not read launchd job %s, so its spawn type is unknown: launchctl print failed (%s)", label, detail)
		r.FixHint = fmt.Sprintf("run 'launchctl print gui/$(id -u)/%s' to see why it fails", label)
	case !c.probe.Loaded:
		r.Message = fmt.Sprintf("launchd job %s is not loaded", label)
	case c.probe.SpawnType == "":
		r.Message = fmt.Sprintf("launchctl print reported no spawn type for %s", label)
	case c.probe.SpawnType == "interactive":
		r.Message = fmt.Sprintf("supervisor launchd job %s runs in the interactive spawn type", label)
	case !c.agentsConfigured:
		r.Message = fmt.Sprintf("supervisor launchd job %s runs in the %s spawn type; no agents are configured", label, c.probe.SpawnType)
	default:
		r.Status = StatusWarning
		r.Message = fmt.Sprintf(
			"supervisor launchd job %s runs in the %s spawn type, not interactive: macOS throttles it and every agent session under it (on Apple Silicon, the efficiency cores at a clamped priority)",
			label, c.probe.SpawnType,
		)
		r.FixHint = fmt.Sprintf(
			"run 'gc supervisor install' to regenerate the plist with ProcessType=Interactive: it rewrites the plist and reloads the supervisor when the plist changes. If the plist already carries the key, 'gc supervisor install --force' reloads the job. Sessions started before keep the old class until they are recreated. Check with: launchctl print gui/$(id -u)/%s | grep 'spawn type'",
			label,
		)
	}
	return r
}
