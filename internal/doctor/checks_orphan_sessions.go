package doctor

import (
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
	name   string
	reason string
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
func (c *OrphanSessionsCheck) orphanSessions(ctx *CheckContext) ([]orphanSession, error) {
	expected, err := c.expectedSessionNames()
	if err != nil {
		return nil, err
	}
	running, listErr := c.sp.ListRunning("")
	if listErr != nil && !runtime.IsPartialListError(listErr) {
		return nil, listErr
	}
	prefix, suffix := c.citySessionShape()
	var orphans []orphanSession
	for _, name := range running {
		env, err := c.sessionEnvironment(name)
		if err != nil {
			return nil, fmt.Errorf("reading ownership for session %q: %w", name, err)
		}
		if c.expectedSession(name, expected, env["GC_TEMPLATE"]) {
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
			orphans = append(orphans, orphanSession{name, fmt.Sprintf("matches city session shape %q + agent + %q", prefix, suffix)})
		} else if cityPath != "" && c.matchesRuntimeIdentity(name, env) {
			orphans = append(orphans, orphanSession{name, "GC_CITY_PATH matches this city and name matches runtime agent identity"})
		}
	}
	return orphans, listErr
}

func (c *OrphanSessionsCheck) sessionEnvironment(name string) (map[string]string, error) {
	// tmux's single-key GetMeta cannot distinguish every transport failure
	// from an absent variable. Its batch read preserves those errors and
	// supplies one consistent ownership snapshot before any Stop.
	if p, ok := c.sp.(runtime.EnvironmentBatchProvider); ok {
		return p.GetAllEnvironment(name)
	}
	env := make(map[string]string)
	for _, key := range []string{"GC_CITY_PATH", "GC_AGENT", "GC_TEMPLATE", "GC_SESSION_ID"} {
		value, err := c.sp.GetMeta(name, key)
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
		base := agent.SessionNameFor(c.cityName, identity, c.sessionTemplate)
		poolBase := agent.SanitizeQualifiedNameForSession(path.Base(identity))
		if name == base || (id != "" && (name == base+"-"+id || name == poolBase+"-"+id)) {
			return true
		}
	}
	return false
}
