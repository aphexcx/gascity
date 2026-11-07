package workspacesvc

import (
	"reflect"
	"sort"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// ReloadChange describes a planned service lifecycle operation. Reasons name
// changed fields, never their values, which may contain credentials.
type ReloadChange struct {
	Name    string   `json:"name"`
	Action  string   `json:"action"`
	Reasons []string `json:"reasons"`
}

// PreviewReload reports what Reload would replace without changing services.
func (m *Manager) PreviewReload(cfg *config.City) []ReloadChange {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	refs := m.currentPublicationRefs()
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil
	}
	return serviceReloadChanges(m.entries, cfg, m.rt.PublicationConfig(), refs)
}

func serviceReloadChanges(entries map[string]*entry, cfg *config.City, pub supervisor.PublicationConfig, refs publicationRefs) []ReloadChange {
	var changes []ReloadChange
	seen := make(map[string]bool, len(cfg.Services))
	for _, svc := range cfg.Services {
		seen[svc.Name] = true
		old := entries[svc.Name]
		if old == nil || old.inst == nil {
			changes = append(changes, ReloadChange{Name: svc.Name, Action: "start", Reasons: []string{"no running instance"}})
			continue
		}
		var reasons []string
		if old.spec.SourceDir != svc.SourceDir {
			reasons = append(reasons, "pack source changed")
		}
		if !reflect.DeepEqual(old.spec.Process.Command, svc.Process.Command) {
			reasons = append(reasons, "command changed")
		}
		base := baseStatus(cfg, pub, refs, svc, time.Time{})
		if svc.KindOrDefault() == "proxy_process" && proxyProcessPublicationContextChanged(old.status, base) {
			reasons = append(reasons, "publication env changed")
		}
		if len(reasons) == 0 && !reflect.DeepEqual(old.spec, svc) {
			reasons = append(reasons, "definition changed")
		}
		if len(reasons) > 0 {
			changes = append(changes, ReloadChange{Name: svc.Name, Action: "restart", Reasons: reasons})
		}
	}
	for name, old := range entries {
		if !seen[name] && old.inst != nil {
			changes = append(changes, ReloadChange{Name: name, Action: "stop", Reasons: []string{"service removed"}})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Name < changes[j].Name })
	return changes
}
