package doctor

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Session-scoped runtime names append a bead ID to the configured agent base.
var orphanSessionID = regexp.MustCompile(`^[a-zA-Z0-9]+-(wisp-)?[a-zA-Z0-9]+$`)

type orphanSession struct {
	name     string
	reason   string
	provider runtime.Provider
}

type orphanSessionScan struct {
	orphans    []orphanSession
	unreadable []error
	backends   []runtime.BackendListResult
}

// CleanupCandidates returns owned orphan names grouped by the backend that
// listed them. The caller must supply its complete, nonempty desired set;
// unlike doctor reporting, only these exact runtime names are protected.
// No candidates are returned when any cleanup safety check fails.
func (c *OrphanSessionsCheck) CleanupCandidates(ctx *CheckContext, desired map[string]bool) ([]runtime.BackendListResult, error) {
	if len(desired) == 0 {
		return nil, fmt.Errorf("refusing orphan cleanup: empty desired session set")
	}
	for name, wanted := range desired {
		if strings.TrimSpace(name) == "" || !wanted {
			return nil, fmt.Errorf("refusing orphan cleanup: partial desired session set")
		}
	}
	if err := c.validateCleanupRuntime(); err != nil {
		return nil, err
	}
	scan, err := c.scanOrphanSessions(ctx, desired)
	if err := orphanCleanupScanError(scan, err); err != nil {
		return nil, err
	}
	return scan.backends, nil
}

func (c *OrphanSessionsCheck) validateCleanupRuntime() error {
	if c.cfg == nil || len(c.cfg.Agents) == 0 {
		return fmt.Errorf("refusing orphan cleanup: loaded config has no agents (empty expected set)")
	}
	if _, err := c.expectedSessionNames(); err != nil {
		return err
	}
	scoped, ok := c.sp.(interface{ IsCityScoped(string) bool })
	if !ok || !scoped.IsCityScoped(c.cityName) {
		return fmt.Errorf("refusing orphan cleanup: runtime cannot confirm a city-scoped socket for %q (default, shared, or unknown socket)", c.cityName)
	}
	return nil
}

func orphanCleanupScanError(scan orphanSessionScan, err error) error {
	if runtime.IsPartialListError(err) {
		return fmt.Errorf("listing sessions partially failed: %w", err)
	}
	if err != nil {
		return err
	}
	if len(scan.unreadable) > 0 {
		return fmt.Errorf("refusing orphan cleanup: incomplete ownership metadata: %w", errors.Join(scan.unreadable...))
	}
	return nil
}

func (c *OrphanSessionsCheck) orphanScopeError(orphan orphanSession) error {
	scoped, ok := orphan.provider.(interface{ IsCityScoped(string) bool })
	if !ok || !scoped.IsCityScoped(c.cityName) {
		return fmt.Errorf("%s: runtime cannot confirm a city-scoped socket for %q (default, shared, or unknown socket)", orphan.name, c.cityName)
	}
	return nil
}

func (c *OrphanSessionsCheck) expectedSessionNames() (map[string]bool, error) {
	expected := make(map[string]bool)
	if c.cfg == nil {
		return expected, nil
	}
	for _, a := range c.cfg.Agents {
		name := agent.SessionNameFor(c.cityName, a.QualifiedName(), c.sessionTemplate)
		if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("refusing orphan classification: empty or partial expected session set")
		}
		if expected[name] {
			return nil, fmt.Errorf("refusing orphan classification: ambiguous expected session name %q", name)
		}
		expected[name] = true
	}
	return expected, nil
}

func (c *OrphanSessionsCheck) expectedSession(name string, expected map[string]bool, recordedTemplate string) bool {
	// Recorded qualified identity disambiguates pools in different rigs that
	// use the same basename. Only use name inference when it is absent.
	if recordedTemplate != "" {
		if c.cfg != nil {
			for _, a := range c.cfg.Agents {
				if a.QualifiedName() == recordedTemplate {
					return true
				}
			}
		}
		return false
	}
	if expected[name] {
		return true
	}
	for base := range expected {
		if suffix, ok := strings.CutPrefix(name, base+"-"); ok && orphanSessionID.MatchString(suffix) {
			return true
		}
	}
	if c.cfg != nil {
		// PoolSessionName uses the template basename, independent of the
		// legacy session template and rig qualification.
		for _, a := range c.cfg.Agents {
			base := agent.SanitizeQualifiedNameForSession(path.Base(a.QualifiedName()))
			if suffix, ok := strings.CutPrefix(name, base+"-"); ok && orphanSessionID.MatchString(suffix) {
				return true
			}
		}
	}
	return false
}

// citySessionShape accepts only a prefix whose rendered value depends on the
// city, and which precedes the agent portion of the name. Agent-only defaults
// and templates without an identifiable city prefix need runtime ownership.
func (c *OrphanSessionsCheck) citySessionShape() (prefix, suffix string) {
	if c.cityName == "" || c.sessionTemplate == "" {
		return "", ""
	}
	const probe = "gc-orphan-agent-probe"
	name := agent.SessionNameFor(c.cityName, probe, c.sessionTemplate)
	prefix, suffix, ok := strings.Cut(name, probe)
	if !ok || prefix == "" || strings.Contains(suffix, probe) {
		return "", ""
	}
	other := agent.SessionNameFor(c.cityName+"-other", probe, c.sessionTemplate)
	otherPrefix, _, ok := strings.Cut(other, probe)
	if !ok || prefix == otherPrefix {
		return "", ""
	}
	return prefix, suffix
}

// orphanSessions collects every candidate before Fix performs any Stop. An
// incomplete listing or failed ownership read therefore cannot partially kill
// the visible subset. Both reporting and cleanup use this classification.
func (c *OrphanSessionsCheck) orphanSessions(ctx *CheckContext) (orphanSessionScan, error) {
	return c.scanOrphanSessions(ctx, nil)
}

func (c *OrphanSessionsCheck) scanOrphanSessions(ctx *CheckContext, desired map[string]bool) (orphanSessionScan, error) {
	var scan orphanSessionScan
	expected, err := c.expectedSessionNames()
	if err != nil {
		return scan, err
	}
	backends := runtime.ListBackendSessions("runtime", c.sp, "")
	_, listErr := runtime.MergeBackendListResults(backends...)
	if listErr != nil && !runtime.IsPartialListError(listErr) {
		return scan, listErr
	}
	prefix, suffix := c.citySessionShape()
	for _, backend := range backends {
		candidates := runtime.BackendListResult{Label: backend.Label, Provider: backend.Provider}
		for _, name := range backend.Names {
			env, err := sessionEnvironment(backend.Provider, name)
			if err != nil {
				scan.unreadable = append(scan.unreadable, fmt.Errorf("reading ownership for session %q (%s): %w", name, backend.Label, err))
				continue
			}
			if desired[name] || (desired == nil && c.expectedSession(name, expected, env["GC_TEMPLATE"])) {
				continue
			}
			cityPath := env["GC_CITY_PATH"]
			if cityPath != "" {
				// Explicit foreign ownership wins even if a name has our prefix.
				if ctx == nil || ctx.CityPath == "" || filepath.Clean(cityPath) != filepath.Clean(ctx.CityPath) {
					continue
				}
			}
			if prefix != "" && strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix) && len(name) > len(prefix)+len(suffix) {
				scan.orphans = append(scan.orphans, orphanSession{name, fmt.Sprintf("matches city session shape %q + agent + %q", prefix, suffix), backend.Provider})
				candidates.Names = append(candidates.Names, name)
			} else if cityPath != "" && c.matchesRuntimeIdentity(name, env) {
				scan.orphans = append(scan.orphans, orphanSession{name, "GC_CITY_PATH matches this city and name matches runtime agent identity", backend.Provider})
				candidates.Names = append(candidates.Names, name)
			}
		}
		if len(candidates.Names) > 0 {
			scan.backends = append(scan.backends, candidates)
		}
	}
	return scan, listErr
}

func sessionEnvironment(sp runtime.Provider, name string) (map[string]string, error) {
	// tmux's single-key GetMeta cannot distinguish every transport failure
	// from an absent variable. Its batch read preserves those errors and
	// supplies one consistent ownership snapshot before any Stop.
	if p, ok := sp.(runtime.EnvironmentBatchProvider); ok {
		return p.GetAllEnvironment(name)
	}
	env := make(map[string]string)
	for _, key := range []string{"GC_CITY_PATH", "GC_AGENT", "GC_TEMPLATE", "GC_SESSION_ID"} {
		value, err := sp.GetMeta(name, key)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", key, err)
		}
		env[key] = value
	}
	return env, nil
}

// A shell can inherit GC_CITY_PATH. Without a city-bearing name, also require
// the runtime name to be derived from its recorded agent or pool template.
func (c *OrphanSessionsCheck) matchesRuntimeIdentity(name string, env map[string]string) bool {
	id := env["GC_SESSION_ID"]
	for _, key := range []string{"GC_AGENT", "GC_TEMPLATE"} {
		identity := env[key]
		if identity == "" {
			continue
		}
		if name == identity || (id != "" && name == identity+"-"+id) {
			return true
		}
		base := agent.SessionNameFor(c.cityName, identity, c.sessionTemplate)
		poolBase := agent.SanitizeQualifiedNameForSession(path.Base(identity))
		if name == base || (id != "" && (name == base+"-"+id || name == poolBase+"-"+id)) {
			return true
		}
	}
	return false
}
