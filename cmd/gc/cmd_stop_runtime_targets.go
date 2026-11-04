package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// resolveRuntimeOnlyStopTargets verifies unresolved shutdown targets before
// allowing the worker boundary to bypass a confirmed deleted session record.
func resolveRuntimeOnlyStopTargets(cityPath string, targets []stopTarget, store beads.Store, sp runtime.Provider) ([]stopTarget, runtime.Provider, error) {
	var resolved []stopTarget
	var bindings []runtime.BackendListResult
	var backends []runtime.BackendListResult
	var result error
	for _, target := range targets {
		if target.sessionID != "" {
			resolved = append(resolved, target)
			continue
		}
		if backends == nil {
			backends = runtime.ListBackendSessions("runtime", sp, "")
		}
		bound, binding, err := bindRuntimeOnlyStopTarget(cityPath, target, store, sp, backends)
		if runtime.IsSessionNotFound(err) {
			continue
		}
		if err != nil {
			result = errors.Join(result, fmt.Errorf("preserving runtime %q: %w", target.name, err))
			continue
		}
		resolved = append(resolved, bound)
		bindings = append(bindings, binding)
	}
	return resolved, providerForManualStopTargets(sp, bindings), result
}

func bindRuntimeOnlyStopTarget(cityPath string, target stopTarget, store beads.Store, sp runtime.Provider, listed []runtime.BackendListResult) (stopTarget, runtime.BackendListResult, error) {
	var empty runtime.BackendListResult
	backends := confirmManualStopBackendAbsence(listed, target.name)
	if _, err := runtime.MergeBackendListResults(backends...); err != nil {
		return target, empty, err
	}
	var id string
	for _, backend := range backends {
		if !slices.Contains(backend.Names, target.name) && !backend.Provider.IsRunning(target.name) {
			continue
		}
		city, ownerID, err := stopSessionOwnership(backend.Provider, target.name)
		if runtime.IsSessionNotFound(err) {
			continue
		}
		if err != nil {
			return target, empty, err
		}
		if city == "" || ownerID == "" {
			return target, empty, fmt.Errorf("incomplete ownership on backend %q", backend.Label)
		}
		if filepath.Clean(city) != filepath.Clean(cityPath) || (id != "" && id != ownerID) {
			return target, empty, fmt.Errorf("conflicting ownership on backend %q", backend.Label)
		}
		id = ownerID
	}
	if id == "" {
		return target, empty, runtime.ErrSessionNotFound
	}
	info := session.Info{ID: id, SessionName: target.name}
	owners, err := manualStopSessionBackends(info, cityPath, backends)
	if err != nil {
		return target, empty, err
	}
	if len(owners) != 1 {
		return target, empty, fmt.Errorf("cannot select one verified backend")
	}
	if store == nil {
		return target, empty, fmt.Errorf("session store unavailable")
	}
	record, err := store.Get(id)
	target.runtimeOnly = stopSessionRecordAbsent(err)
	if err != nil && !target.runtimeOnly {
		return target, empty, fmt.Errorf("checking session record %q: %w", id, err)
	}
	if err == nil && (record.Type != "session" || (record.Metadata["session_name"] != "" && record.Metadata["session_name"] != target.name)) {
		return target, empty, fmt.Errorf("session record %q does not identify this runtime", id)
	}
	backend := backends[owners[0]]
	provider := backend.Provider
	if binder, ok := provider.(interface {
		BindForShutdown(string, string, string) (runtime.Provider, error)
	}); ok {
		provider, err = binder.BindForShutdown(target.name, cityPath, id)
		if err != nil {
			return target, empty, err
		}
	}
	verified := &verifiedManualStopBackend{Provider: provider, verify: func() error {
		if err := verifyManualStopOwnership(sp, info, cityPath, backend.Label, map[string]bool{backend.Label: true}); err != nil {
			return err
		}
		if target.runtimeOnly {
			if _, err := store.Get(id); !stopSessionRecordAbsent(err) {
				if err == nil {
					return fmt.Errorf("session record %q reappeared during shutdown", id)
				}
				return fmt.Errorf("rechecking session record %q: %w", id, err)
			}
		}
		return nil
	}}
	return target, runtime.BackendListResult{Label: backend.Label, Provider: verified, Names: []string{target.name}}, nil
}
