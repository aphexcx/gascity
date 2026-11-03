package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// manualStopProvider pins only shutdown operations to verified owners. The
// original provider continues to enumerate the complete runtime population.
type manualStopProvider struct {
	runtime.Provider
	owners  map[string][]runtime.BackendListResult
	mu      sync.Mutex
	stopErr error
}

func providerForManualStopTargets(sp runtime.Provider, targets []runtime.BackendListResult) runtime.Provider {
	if len(targets) == 0 {
		return sp
	}
	p := &manualStopProvider{Provider: sp, owners: make(map[string][]runtime.BackendListResult)}
	for _, target := range targets {
		for _, name := range target.Names {
			p.owners[name] = append(p.owners[name], target)
		}
	}
	return p
}

// GetMeta keeps metadata-dependent worker resolution on the selected owners.
func (p *manualStopProvider) GetMeta(name, key string) (string, error) {
	owners := p.owners[name]
	if len(owners) == 0 {
		return p.Provider.GetMeta(name, key)
	}
	var value string
	seen := false
	for _, owner := range owners {
		got, err := owner.Provider.GetMeta(name, key)
		if runtime.IsSessionNotFound(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("reading runtime %q metadata (%s): %w", name, owner.Label, err)
		}
		if seen && got != value {
			return "", fmt.Errorf("runtime %q has conflicting %s metadata across selected backends", name, key)
		}
		value, seen = got, true
	}
	if !seen {
		return "", runtime.ErrSessionNotFound
	}
	return value, nil
}

// DetectTransport preserves live transport recovery during worker enrichment.
func (p *manualStopProvider) DetectTransport(name string) string {
	if detector, ok := p.Provider.(interface{ DetectTransport(string) string }); ok {
		return detector.DetectTransport(name)
	}
	return ""
}

// RouteACP preserves route registration for configured legacy sessions.
func (p *manualStopProvider) RouteACP(name string) {
	if router, ok := p.Provider.(interface{ RouteACP(string) }); ok {
		router.RouteACP(name)
	}
}

// Unroute preserves cleanup of transport registrations.
func (p *manualStopProvider) Unroute(name string) {
	if router, ok := p.Provider.(interface{ Unroute(string) }); ok {
		router.Unroute(name)
	}
}

// Stop shuts down every verified copy without falling back to another owner.
func (p *manualStopProvider) Stop(name string) error {
	owners := p.owners[name]
	if len(owners) == 0 {
		return p.Provider.Stop(name)
	}
	var result error
	for _, owner := range owners {
		var err error
		if stopper, ok := owner.Provider.(interface{ StopForShutdown(string) error }); ok {
			err = stopper.StopForShutdown(name)
		} else {
			err = owner.Provider.Stop(name)
		}
		if err != nil && !runtime.IsSessionNotFound(err) {
			result = errors.Join(result, fmt.Errorf("%s backend: %w", owner.Label, err))
		}
	}
	p.mu.Lock()
	p.stopErr = errors.Join(p.stopErr, result)
	p.mu.Unlock()
	return result
}

func (p *manualStopProvider) shutdownError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopErr
}

// ListRunningBackends preserves leaf-level absence evidence through the wrapper.
func (p *manualStopProvider) ListRunningBackends(prefix string) []runtime.BackendListResult {
	return runtime.ListBackendSessions("runtime", p.Provider, prefix)
}

// verifiedManualStopBackend retains discovery's ownership and rechecks every
// backend before each destructive operation. Provider may additionally bind a
// native identity (for example, a thread ID) instead of resolving a name again.
type verifiedManualStopBackend struct {
	runtime.Provider
	verify func() error
}

// Stop rechecks ownership before requesting teardown from its original owner.
func (p *verifiedManualStopBackend) Stop(name string) error {
	if err := p.verify(); err != nil {
		return err
	}
	if stopper, ok := p.Provider.(interface{ StopForShutdown(string) error }); ok {
		return stopper.StopForShutdown(name)
	}
	return p.Provider.Stop(name)
}

// Interrupt rechecks ownership before canceling the current turn.
func (p *verifiedManualStopBackend) Interrupt(name string) error {
	if err := p.verify(); err != nil {
		return err
	}
	return p.Provider.Interrupt(name)
}

// SendKeys rechecks ownership before sending soft or fallback cancellation keys.
func (p *verifiedManualStopBackend) SendKeys(name string, keys ...string) error {
	if err := p.verify(); err != nil {
		return err
	}
	return p.Provider.SendKeys(name, keys...)
}

// ObserveLivenessWithError retains the owner's error-bearing observations.
func (p *verifiedManualStopBackend) ObserveLivenessWithError(name string, processes []string) (runtime.Liveness, error) {
	return runtime.ObserveLivenessWithError(p.Provider, name, processes)
}

// WaitForIdle retains the owner's soft-interrupt confirmation boundary.
func (p *verifiedManualStopBackend) WaitForIdle(ctx context.Context, name string, timeout time.Duration) error {
	if waiter, ok := p.Provider.(runtime.IdleWaitProvider); ok {
		return waiter.WaitForIdle(ctx, name, timeout)
	}
	return runtime.ErrInteractionUnsupported
}

// WaitForInterruptBoundary retains the owner's cancellation confirmation.
func (p *verifiedManualStopBackend) WaitForInterruptBoundary(ctx context.Context, name string, since time.Time, timeout time.Duration) error {
	if waiter, ok := p.Provider.(runtime.InterruptBoundaryWaitProvider); ok {
		return waiter.WaitForInterruptBoundary(ctx, name, since, timeout)
	}
	return runtime.ErrInteractionUnsupported
}

func verifyManualStopOwnership(sp runtime.Provider, info session.Info, cityPath, label string, discovered map[string]bool) error {
	if len(discovered) != 1 {
		return fmt.Errorf("preserving manual runtime %q: multiple backends claim the name", info.SessionName)
	}
	backends := confirmManualStopBackendAbsence(runtime.ListBackendSessions("runtime", sp, ""), info.SessionName)
	if _, err := runtime.MergeBackendListResults(backends...); err != nil {
		// A vanished server may fail its list operation while its ownership
		// query positively identifies absence. Other backends must still be
		// readable and free of this name before accepting that confirmation.
		absent, othersClear := false, true
		for _, backend := range backends {
			listed := slices.Contains(backend.Names, info.SessionName)
			if backend.Label == label {
				_, _, ownerErr := stopSessionOwnership(backend.Provider, info.SessionName)
				absent = runtime.IsSessionNotFound(ownerErr) && !listed
			} else if backend.Err != nil || listed || backend.Provider.IsRunning(info.SessionName) {
				othersClear = false
			} else if owner, ok := backend.Provider.(interface {
				SessionOwnership(string) (string, string, error)
			}); ok {
				_, _, ownerErr := owner.SessionOwnership(info.SessionName)
				othersClear = othersClear && runtime.IsSessionNotFound(ownerErr)
			}
		}
		if absent && othersClear {
			return runtime.ErrSessionNotFound
		}
		return fmt.Errorf("%w: preserving manual runtime %q: cannot list ownership: %s", runtime.ErrRuntimeUnavailable, info.SessionName, err.Error())
	}
	ownerSeen, present := false, false
	for _, backend := range backends {
		listed := slices.Contains(backend.Names, info.SessionName)
		if !discovered[backend.Label] && !listed && !backend.Provider.IsRunning(info.SessionName) {
			owner, ok := backend.Provider.(interface {
				SessionOwnership(string) (string, string, error)
			})
			if !ok {
				continue
			}
			_, _, err := owner.SessionOwnership(info.SessionName)
			if runtime.IsSessionNotFound(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("%w: preserving manual runtime %q: cannot verify backend %q: %s", runtime.ErrRuntimeUnavailable, info.SessionName, backend.Label, err.Error())
			}
		}
		if !discovered[backend.Label] {
			return fmt.Errorf("preserving manual runtime %q: new backend %q claims the name", info.SessionName, backend.Label)
		}
		ownerSeen = ownerSeen || backend.Label == label
		// A non-running artifact can still need teardown. Read every discovered
		// owner's metadata even when its current live-session listing omits it.
		ownerCity, ownerID, err := stopSessionOwnership(backend.Provider, info.SessionName)
		if runtime.IsSessionNotFound(err) && !listed {
			continue
		}
		if err != nil {
			return fmt.Errorf("%w: preserving manual runtime %q: cannot verify ownership: %s", runtime.ErrRuntimeUnavailable, info.SessionName, err.Error())
		}
		if ownerCity == "" || filepath.Clean(ownerCity) != filepath.Clean(cityPath) || ownerID != info.ID {
			return fmt.Errorf("preserving manual runtime %q: ownership changed or is missing", info.SessionName)
		}
		present = present || backend.Label == label
	}
	if !ownerSeen {
		return fmt.Errorf("preserving manual runtime %q: original backend %q is unavailable", info.SessionName, label)
	}
	if !present {
		return runtime.ErrSessionNotFound
	}
	return nil
}

// Interrupt sends the graceful hint to every verified copy.
func (p *manualStopProvider) Interrupt(name string) error {
	owners := p.owners[name]
	if len(owners) == 0 {
		return p.Provider.Interrupt(name)
	}
	var result error
	for _, owner := range owners {
		if err := owner.Provider.Interrupt(name); err != nil {
			result = errors.Join(result, fmt.Errorf("%s backend: %w", owner.Label, err))
		}
	}
	return result
}

// SendKeys sends provider-specific interrupt keys to every verified copy.
func (p *manualStopProvider) SendKeys(name string, keys ...string) error {
	owners := p.owners[name]
	if len(owners) == 0 {
		return p.Provider.SendKeys(name, keys...)
	}
	var result error
	for _, owner := range owners {
		if err := owner.Provider.SendKeys(name, keys...); err != nil {
			result = errors.Join(result, fmt.Errorf("%s backend: %w", owner.Label, err))
		}
	}
	return result
}

// WaitForIdle preserves the provider's soft-interrupt confirmation and fallback.
func (p *manualStopProvider) WaitForIdle(ctx context.Context, name string, timeout time.Duration) error {
	return p.waitForOwners(ctx, name, timeout, func(ctx context.Context, sp runtime.Provider, remaining time.Duration) error {
		if waiter, ok := sp.(runtime.IdleWaitProvider); ok {
			return waiter.WaitForIdle(ctx, name, remaining)
		}
		return runtime.ErrInteractionUnsupported
	})
}

// WaitForInterruptBoundary confirms cancellation on every supporting owner.
func (p *manualStopProvider) WaitForInterruptBoundary(ctx context.Context, name string, since time.Time, timeout time.Duration) error {
	return p.waitForOwners(ctx, name, timeout, func(ctx context.Context, sp runtime.Provider, remaining time.Duration) error {
		if waiter, ok := sp.(runtime.InterruptBoundaryWaitProvider); ok {
			return waiter.WaitForInterruptBoundary(ctx, name, since, remaining)
		}
		return runtime.ErrInteractionUnsupported
	})
}

func (p *manualStopProvider) waitForOwners(ctx context.Context, name string, timeout time.Duration, wait func(context.Context, runtime.Provider, time.Duration) error) error {
	owners := p.owners[name]
	if len(owners) == 0 {
		owners = []runtime.BackendListResult{{Label: "runtime", Provider: p.Provider}}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	var result error
	supported := false
	for _, owner := range owners {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		err := wait(ctx, owner.Provider, time.Until(deadline))
		if errors.Is(err, runtime.ErrInteractionUnsupported) {
			continue
		}
		supported = true
		if err != nil {
			result = errors.Join(result, fmt.Errorf("%s backend: %w", owner.Label, err))
		}
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(result, err)
	}
	if !supported {
		return runtime.ErrInteractionUnsupported
	}
	return result
}

// IsRunning reports whether any verified copy remains running.
func (p *manualStopProvider) IsRunning(name string) bool {
	owners := p.owners[name]
	if len(owners) == 0 {
		return p.Provider.IsRunning(name)
	}
	for _, owner := range owners {
		if owner.Provider.IsRunning(name) {
			return true
		}
	}
	return false
}

// ObserveLivenessWithError preserves each owner's error-bearing observation.
func (p *manualStopProvider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	owners := p.owners[name]
	if len(owners) == 0 {
		return runtime.ObserveLivenessWithError(p.Provider, name, processNames)
	}
	var result runtime.Liveness
	var resultErr error
	for _, owner := range owners {
		live, err := runtime.ObserveLivenessWithError(owner.Provider, name, processNames)
		result.Running = result.Running || live.Running
		result.Alive = result.Alive || live.Alive
		resultErr = errors.Join(resultErr, err)
	}
	return result, resultErr
}

// confirmManualStopBackendAbsence treats a leaf's absent server as empty only
// when its ownership query independently confirms this target is absent too.
// The confirmation applies to that backend and target, never to siblings.
func confirmManualStopBackendAbsence(backends []runtime.BackendListResult, name string) []runtime.BackendListResult {
	backends = slices.Clone(backends)
	for i, backend := range backends {
		if len(backend.Names) != 0 || !runtime.IsRuntimeServerAbsent(backend.Err) {
			continue
		}
		if _, _, err := stopSessionOwnership(backend.Provider, name); runtime.IsSessionNotFound(err) {
			backends[i].Err = nil
		}
	}
	return backends
}

// resolveManualStopSessionTargets includes persisted named sessions alongside
// manual sessions: both persist runtime identities outside pool enumeration.
// store is the caller's already-resolved session-class store.
func resolveManualStopSessionTargets(store beads.Store, cfg *config.City, cityPath string, sp runtime.Provider) ([]runtime.BackendListResult, error) {
	infos, err := sessionFrontDoor(store).ListAll(session.ListAllOptions{IncludeClosed: true})
	if err != nil {
		return nil, fmt.Errorf("listing manual sessions: %w", err)
	}
	var manual []session.Info
	for _, info := range infos {
		cfgAgent := findAgentByTemplate(cfg, sessionBeadStoredTemplateInfo(info))
		named := isNamedSessionInfo(info)
		if named || isManualSessionInfoForAgent(info, cfgAgent) {
			manual = append(manual, info)
		}
	}
	if len(manual) == 0 {
		return nil, nil
	}
	listedBackends := runtime.ListBackendSessions("runtime", sp, "")
	var targets []runtime.BackendListResult
	var ownershipErr error
	for _, info := range manual {
		info.SessionName = strings.TrimSpace(info.SessionName)
		if info.SessionName == "" {
			if info.Closed || isFailedCreateSessionInfo(info) {
				continue // terminal cleanup may already have retired the identity
			}
			ownershipErr = errors.Join(ownershipErr, fmt.Errorf("manual session %q has no runtime identity", info.ID))
			continue
		}
		backends := confirmManualStopBackendAbsence(listedBackends, info.SessionName)
		if _, err := runtime.MergeBackendListResults(backends...); err != nil {
			ownershipErr = errors.Join(ownershipErr, fmt.Errorf("listing manual session %q runtime %q: %w", info.ID, info.SessionName, err))
			continue
		}
		owners, err := manualStopSessionBackends(info, cityPath, backends)
		ownershipErr = errors.Join(ownershipErr, err)
		discovered := make(map[string]bool, len(owners))
		for _, i := range owners {
			discovered[backends[i].Label] = true
		}
		var sessionTargets []runtime.BackendListResult
		var bindingErr error
		for _, i := range owners {
			backend := backends[i]
			provider := backend.Provider
			if binder, ok := provider.(interface {
				BindForShutdown(string, string, string) (runtime.Provider, error)
			}); ok {
				provider, err = binder.BindForShutdown(info.SessionName, cityPath, info.ID)
				if err != nil {
					bindingErr = errors.Join(bindingErr, fmt.Errorf("%w: preserving manual runtime %q: binding native identity: %s", runtime.ErrRuntimeUnavailable, info.SessionName, err.Error()))
					provider = backend.Provider
				}
			}
			verified := &verifiedManualStopBackend{Provider: provider, verify: func() error {
				if bindingErr != nil {
					return bindingErr
				}
				return verifyManualStopOwnership(sp, info, cityPath, backend.Label, discovered)
			}}
			sessionTargets = append(sessionTargets, runtime.BackendListResult{Label: backend.Label, Provider: verified, Names: []string{info.SessionName}})
		}
		ownershipErr = errors.Join(ownershipErr, bindingErr)
		targets = append(targets, sessionTargets...)
	}
	return targets, ownershipErr
}

func manualStopSessionBackends(info session.Info, cityPath string, backends []runtime.BackendListResult) ([]int, error) {
	var owners []int
	var ownershipErr, conflictErr error
	for i, backend := range backends {
		// Persisted identities can outlive running/listed state (for example,
		// a Pending pod). Only the ownership probe can confirm their absence.
		ownerCity, ownerID, err := stopSessionOwnership(backend.Provider, info.SessionName)
		if runtime.IsSessionNotFound(err) {
			// Listings and boolean liveness may be cached. The ownership read
			// positively confirms absence for this exact name on this leaf.
			continue
		}
		if err != nil {
			ownershipErr = errors.Join(ownershipErr, fmt.Errorf("reading manual session %q runtime %q ownership (%s): %w", info.ID, info.SessionName, backend.Label, err))
			continue
		}
		if ownerCity == "" || ownerID == "" {
			if shared, ok := backend.Provider.(interface {
				IsUnmanagedSession(string) (bool, error)
			}); ok {
				unmanaged, err := shared.IsUnmanagedSession(info.SessionName)
				if runtime.IsSessionNotFound(err) {
					continue
				}
				if err != nil {
					ownershipErr = errors.Join(ownershipErr, fmt.Errorf("reading manual session %q runtime %q identity (%s): %w", info.ID, info.SessionName, backend.Label, err))
					continue
				}
				if unmanaged {
					ownershipErr = errors.Join(ownershipErr, fmt.Errorf("persisted manual session %q runtime %q has no ownership record (%s)", info.ID, info.SessionName, backend.Label))
					continue
				}
			}
			if slices.Contains(backend.Names, info.SessionName) || backend.Provider.IsRunning(info.SessionName) {
				conflictErr = errors.Join(conflictErr, fmt.Errorf("manual session %q runtime %q has incomplete ownership (%s)", info.ID, info.SessionName, backend.Label))
				continue // the orphan sweep reports visible unknown owners
			}
			if _, ok := backend.Provider.(interface {
				SessionOwnership(string) (string, string, error)
			}); ok {
				ownershipErr = errors.Join(ownershipErr, fmt.Errorf("manual session %q runtime %q has incomplete ownership (%s)", info.ID, info.SessionName, backend.Label))
			}
			continue // legacy metadata reads cannot distinguish missing keys from absence
		}
		// A persisted name can be reused by another city or another session.
		// Every backend holding it must agree before routing an explicit stop.
		if cityPath == "" || ownerCity == "" || filepath.Clean(ownerCity) != filepath.Clean(cityPath) || ownerID != info.ID {
			err := fmt.Errorf("manual session %q runtime %q has conflicting ownership (%s)", info.ID, info.SessionName, backend.Label)
			conflictErr = errors.Join(conflictErr, err)
			if !slices.Contains(backend.Names, info.SessionName) && !backend.Provider.IsRunning(info.SessionName) {
				ownershipErr = errors.Join(ownershipErr, err)
			}
			continue
		}
		owners = append(owners, i)
	}
	// Inspect every leaf before deciding: a visible foreign session cannot
	// hide an owned or unreadable Pending artifact on another backend.
	if len(owners) > 0 {
		ownershipErr = errors.Join(ownershipErr, conflictErr)
	}
	if ownershipErr != nil {
		return nil, ownershipErr
	}
	return owners, nil
}

func verifyScopedOrphanStopOwnership(sp runtime.Provider, name, expectedCity, expectedID string) error {
	city, id, err := stopSessionOwnership(sp, name)
	if err != nil {
		return fmt.Errorf("verifying scoped orphan %q ownership: %w", name, err)
	}
	if (city == "") != (expectedCity == "") || filepath.Clean(city) != filepath.Clean(expectedCity) || id != expectedID {
		return fmt.Errorf("preserving scoped orphan %q: ownership changed or is missing", name)
	}
	return nil
}

func stopSessionOwnership(sp runtime.Provider, name string) (string, string, error) {
	if owner, ok := sp.(interface {
		SessionOwnership(string) (string, string, error)
	}); ok {
		return owner.SessionOwnership(name)
	}
	if batch, ok := sp.(runtime.EnvironmentBatchProvider); ok {
		env, err := batch.GetAllEnvironment(name)
		return env["GC_CITY_PATH"], env["GC_SESSION_ID"], err
	}
	cityPath, err := sp.GetMeta(name, "GC_CITY_PATH")
	if err != nil {
		return "", "", err
	}
	id, err := sp.GetMeta(name, "GC_SESSION_ID")
	return cityPath, id, err
}

// ownedOrphanStopTargets discovers leaves without a shared server scope, such
// as ACP and Kubernetes. Both host city and session ID must be explicit; name
// shape alone never authorizes cleanup on these backends.
func ownedOrphanStopTargets(sp runtime.Provider, cityPath string, desired map[string]bool) ([]runtime.BackendListResult, []string, error) {
	backends := listSessionBackendsWithAbsenceCheck(sp)
	if _, err := runtime.MergeBackendListResults(backends...); err != nil {
		return nil, nil, fmt.Errorf("listing orphan runtimes: %w", err)
	}
	var targets []runtime.BackendListResult
	var unknown []string
	for _, backend := range backends {
		for _, name := range backend.Names {
			if desired[name] {
				continue
			}
			ownerCity, ownerID, err := stopSessionOwnership(backend.Provider, name)
			if runtime.IsSessionNotFound(err) {
				// A listed runtime can disappear before its ownership read.
				continue
			}
			if err != nil {
				return nil, nil, fmt.Errorf("reading orphan runtime %q ownership (%s): %w", name, backend.Label, err)
			}
			if ownerCity != "" && cityPath != "" && filepath.Clean(ownerCity) != filepath.Clean(cityPath) {
				continue
			}
			if cityPath == "" || ownerCity == "" || ownerID == "" {
				if shared, ok := backend.Provider.(interface {
					IsUnmanagedSession(string) (bool, error)
				}); ok {
					unmanaged, err := shared.IsUnmanagedSession(name)
					if runtime.IsSessionNotFound(err) {
						continue
					}
					if err != nil {
						return nil, nil, fmt.Errorf("reading orphan runtime %q identity (%s): %w", name, backend.Label, err)
					}
					if unmanaged {
						continue
					}
				}
				unknown = append(unknown, name)
				continue
			}
			provider := backend.Provider
			if binder, ok := provider.(interface {
				BindForShutdown(string, string, string) (runtime.Provider, error)
			}); ok {
				provider, err = binder.BindForShutdown(name, cityPath, ownerID)
				if err != nil {
					return nil, nil, fmt.Errorf("binding orphan runtime %q: %w", name, err)
				}
			}
			info := session.Info{ID: ownerID, SessionName: name}
			verified := &verifiedManualStopBackend{Provider: provider, verify: func() error {
				return verifyManualStopOwnership(sp, info, cityPath, backend.Label, map[string]bool{backend.Label: true})
			}}
			targets = append(targets, runtime.BackendListResult{Label: backend.Label, Provider: verified, Names: []string{name}})
		}
	}
	slices.Sort(unknown)
	return targets, slices.Compact(unknown), nil
}
