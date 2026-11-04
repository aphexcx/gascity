package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// resolveRuntimeOnlyStopTargets verifies every shutdown target's provider
// binding and session record before crossing the worker boundary.
func resolveRuntimeOnlyStopTargets(cityPath string, targets []stopTarget, store beads.Store, sp runtime.Provider) ([]stopTarget, []runtime.BackendListResult, error) {
	var resolved []stopTarget
	var bindings []runtime.BackendListResult
	var backends []runtime.BackendListResult
	var result error
	for _, target := range targets {
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
	return resolved, bindings, result
}

func bindRuntimeOnlyStopTarget(cityPath string, target stopTarget, store beads.Store, sp runtime.Provider, listed []runtime.BackendListResult) (stopTarget, runtime.BackendListResult, error) {
	var empty runtime.BackendListResult
	backends := confirmManualStopBackendAbsence(listed, target.name)
	// A resolved bead on a single backend can be verified by its exact
	// ownership query even when bulk enumeration is unavailable.
	resolvedSingle := target.sessionID != "" && len(backends) == 1
	if _, err := runtime.MergeBackendListResults(backends...); err != nil && !resolvedSingle {
		return target, empty, err
	}
	var id string
	for _, backend := range backends {
		city, ownerID, err := stopSessionOwnership(backend.Provider, target.name)
		if runtime.IsSessionNotFound(err) {
			continue
		}
		if err != nil {
			return target, empty, err
		}
		if city == "" || ownerID == "" {
			if city == "" && ownerID == "" && target.resolved {
				if binder, ok := backend.Provider.(interface {
					BindForShutdown(string, string, string) (runtime.Provider, error)
				}); ok {
					provider, err := binder.BindForShutdown(target.name, cityPath, target.sessionID)
					if err != nil {
						return target, empty, err
					}
					return bindConfiguredStopTarget(target, store, sp, backend.Label, provider)
				}
			}
			// Legacy metadata reads cannot distinguish an empty backend from
			// missing ownership. Preserve the manual-stop compatibility rule;
			// typed ownership providers must positively report absence.
			if _, typed := backend.Provider.(interface {
				SessionOwnership(string) (string, string, error)
			}); !typed && !slices.Contains(backend.Names, target.name) && !backend.Provider.IsRunning(target.name) {
				continue
			}
			return target, empty, fmt.Errorf("incomplete ownership on backend %q", backend.Label)
		}
		if filepath.Clean(city) != filepath.Clean(cityPath) || (id != "" && id != ownerID) || (target.sessionID != "" && target.sessionID != ownerID) {
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
	if target.runtimeOnly {
		target.sessionID = ""
	}
	if err != nil && !target.runtimeOnly {
		return target, empty, fmt.Errorf("checking session record %q: %w", id, err)
	}
	if err == nil && (!session.IsSessionBeadOrRepairable(record) || (record.Metadata["session_name"] != "" && record.Metadata["session_name"] != target.name)) {
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
		if resolvedSingle {
			current := runtime.ListBackendSessions("runtime", sp, "")
			if len(current) != 1 || current[0].Label != backend.Label {
				return fmt.Errorf("runtime %q backend selection changed during shutdown", target.name)
			}
			if err := verifyScopedOrphanStopOwnership(current[0].Provider, target.name, cityPath, id); err != nil {
				return err
			}
		} else {
			if err := verifyManualStopOwnership(sp, info, cityPath, backend.Label, map[string]bool{backend.Label: true}); err != nil {
				return err
			}
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

// bindConfiguredStopTarget retains a provider-authorized name binding when a
// configured runtime has no ownership metadata. Providers that require an
// ownership record must refuse that binding themselves.
func bindConfiguredStopTarget(target stopTarget, store beads.Store, sp runtime.Provider, label string, provider runtime.Provider) (stopTarget, runtime.BackendListResult, error) {
	verify := func(allowAbsent bool) error {
		if store == nil {
			return fmt.Errorf("session store unavailable")
		}
		current := confirmManualStopBackendAbsence(runtime.ListBackendSessions("runtime", sp, ""), target.name)
		if _, err := runtime.MergeBackendListResults(current...); err != nil {
			return err
		}
		selected := 0
		boundAbsent := false
		for _, backend := range current {
			// Listing is optional for exec scripts. The bound backend may
			// instead confirm this configured name with its exact liveness probe.
			if slices.Contains(backend.Names, target.name) || (backend.Label == label && backend.Provider.IsRunning(target.name)) {
				selected++
				if backend.Label != label || selected > 1 {
					return fmt.Errorf("runtime %q backend selection changed or conflicts during shutdown", target.name)
				}
				continue
			}
			// A sibling's empty listing alone does not rule out an unlisted
			// runtime. Require its exact ownership probe to confirm absence.
			if _, _, err := stopSessionOwnership(backend.Provider, target.name); !runtime.IsSessionNotFound(err) {
				if err != nil {
					return fmt.Errorf("rechecking runtime %q backend %q: %w", target.name, backend.Label, err)
				}
				return fmt.Errorf("runtime %q absence is unconfirmed on backend %q", target.name, backend.Label)
			}
			boundAbsent = boundAbsent || backend.Label == label
		}
		if selected == 0 && (!allowAbsent || !boundAbsent) {
			return runtime.ErrSessionNotFound
		}
		if target.sessionID != "" {
			record, err := store.Get(target.sessionID)
			if err != nil {
				return fmt.Errorf("checking configured session %q: %w", target.sessionID, err)
			}
			if !session.IsSessionBeadOrRepairable(record) || (record.Metadata["session_name"] != "" && record.Metadata["session_name"] != target.name) {
				return fmt.Errorf("session record %q does not identify this runtime", target.sessionID)
			}
			return nil
		}
		records, err := loadSessionBeads(store)
		if err != nil {
			return err
		}
		for _, record := range records {
			if record.Metadata["session_name"] == target.name {
				return fmt.Errorf("runtime %q gained a session record during shutdown", target.name)
			}
		}
		return nil
	}
	if err := verify(false); err != nil {
		return target, runtime.BackendListResult{}, err
	}
	target.runtimeOnly = target.sessionID == ""
	return target, runtime.BackendListResult{
		Label: label, Names: []string{target.name},
		Provider: &configuredStopBackend{
			verifiedManualStopBackend: &verifiedManualStopBackend{Provider: provider, verify: func() error { return verify(false) }},
			verifyCleanup:             func() error { return verify(true) },
		},
	}, nil
}

// configuredStopBackend retains the cleanup obligation once cancellation is
// dispatched, even when the script exits before reporting an error or completion.
type configuredStopBackend struct {
	*verifiedManualStopBackend
	verifyCleanup   func() error
	stopMu          sync.Mutex
	cancelRequested atomic.Bool
	stopped         atomic.Bool
}

// Stop rechecks the original binding and completes its teardown at most once.
func (p *configuredStopBackend) Stop(name string) error {
	p.stopMu.Lock()
	defer p.stopMu.Unlock()
	if p.stopped.Load() {
		return nil
	}
	backend := *p.verifiedManualStopBackend
	if p.cancelRequested.Load() {
		backend.verify = p.verifyCleanup
	}
	if err := backend.Stop(name); err != nil {
		return err
	}
	p.stopped.Store(true)
	return nil
}

// Interrupt retains cleanup before dispatch, without blocking stop escalation
// on a cancellation call that may outlive the interrupt-wave deadline.
func (p *configuredStopBackend) Interrupt(name string) error {
	if err := p.verify(); err != nil {
		return err
	}
	if p.stopped.Load() {
		return runtime.ErrSessionNotFound
	}
	p.cancelRequested.Store(true)
	return p.Provider.Interrupt(name)
}

// SendKeys retains cleanup before dispatching soft or fallback cancellation.
func (p *configuredStopBackend) SendKeys(name string, keys ...string) error {
	if err := p.verify(); err != nil {
		return err
	}
	if p.stopped.Load() {
		return runtime.ErrSessionNotFound
	}
	p.cancelRequested.Store(true)
	return p.Provider.SendKeys(name, keys...)
}
