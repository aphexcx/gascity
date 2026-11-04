package t3bridge

import (
	"fmt"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/runtime"
)

type shutdownProvider struct {
	*Provider
	name      string
	cityPath  string
	sessionID string
	threadID  string
}

// ShutdownComplete reports teardown only when all eligible threads carrying the
// session name are stopped, without requiring preserved history or ownership to
// disappear. Snapshot uncertainty remains an error, and a recent start cannot
// confirm completion.
func (p *Provider) ShutdownComplete(name string) (bool, error) {
	if p.withinRecentStart(name) {
		return false, nil
	}
	snapshot, err := p.rpcSnapshot()
	if err != nil {
		return false, fmt.Errorf("%w: t3bridge snapshot unavailable for %q: %w", runtime.ErrRuntimeUnavailable, name, err)
	}
	complete := true
	for _, thread := range snapshotThreads(snapshot) {
		if !snapshotThreadEligible(thread) || SessionNameFromMetadata(threadCustomMetadata(thread)) != name {
			continue
		}
		if snapshotThreadBinding(thread) == nil {
			return false, fmt.Errorf("%w: t3bridge snapshot has incomplete binding for %q", runtime.ErrRuntimeUnavailable, name)
		}
		status := snapshotThreadSessionStatus(thread)
		switch classifyT3SessionStatus(status) {
		case t3SessionStatusLive:
			complete = false
		case t3SessionStatusAbsent:
			// Retain stopped history while checking the remaining threads.
		default:
			return false, fmt.Errorf("%w: t3bridge snapshot has unknown session status %q for %q", runtime.ErrRuntimeUnavailable, status, name)
		}
	}
	return complete, nil
}

// BindForShutdown verifies ownership and pins the native thread used by all
// subsequent shutdown mutations. Each mutation fetches a fresh snapshot.
func (p *Provider) BindForShutdown(name, cityPath, sessionID string) (runtime.Provider, error) {
	bound := &shutdownProvider{Provider: p, name: name, cityPath: cityPath, sessionID: sessionID}
	threadID, err := bound.verify(name)
	if err != nil {
		return nil, err
	}
	bound.threadID = threadID
	return bound, nil
}

func (p *shutdownProvider) verify(name string) (string, error) {
	if name != p.name {
		return "", fmt.Errorf("preserving T3 session %q: shutdown is bound to %q", name, p.name)
	}
	snapshot, err := p.rpcFreshSnapshot()
	if err != nil {
		return "", fmt.Errorf("%w: preserving T3 session %q: reading live ownership: %s", runtime.ErrRuntimeUnavailable, name, err.Error())
	}
	thread := snapshotThreadBySessionName(snapshot, name)
	if thread == nil {
		return "", runtime.ErrSessionNotFound
	}
	binding := snapshotThreadBinding(thread)
	if binding == nil || binding.ThreadID == "" || (p.threadID != "" && binding.ThreadID != p.threadID) {
		return "", fmt.Errorf("preserving T3 session %q: verified thread identity changed", name)
	}
	cityPath, sessionID, err := threadSessionOwnership(thread)
	if err != nil || cityPath == "" || p.cityPath == "" || filepath.Clean(cityPath) != filepath.Clean(p.cityPath) || sessionID == "" || sessionID != p.sessionID {
		return "", fmt.Errorf("preserving T3 session %q: thread ownership changed or is unreadable", name)
	}
	return binding.ThreadID, nil
}

// Stop stops only the thread bound at discovery, preserving its history.
func (p *shutdownProvider) Stop(name string) error {
	return p.StopForShutdown(name)
}

// StopForShutdown revalidates ownership before dispatching to the pinned ID.
func (p *shutdownProvider) StopForShutdown(name string) error {
	threadID, err := p.verify(name)
	if err != nil {
		return err
	}
	if err := p.dispatchThreadSessionStop(threadID); err != nil {
		return fmt.Errorf("stopping T3 session %q: %w", name, err)
	}
	p.stopEventWatcher(name)
	p.recordStateChange(threadID, "stopped", "GC session stopped", nil)
	p.clearRecentStart(name)
	p.clearBridgeMeta(name)
	p.clearSnapshotCache()
	return nil
}

// Interrupt revalidates ownership before canceling the pinned thread's turn.
func (p *shutdownProvider) Interrupt(name string) error {
	threadID, err := p.verify(name)
	if err != nil {
		return err
	}
	if err := p.dispatchTurnInterrupt(threadID); err != nil {
		return err
	}
	p.clearSnapshotCache()
	return nil
}

// SendKeys keeps soft cancellation on the same verified native thread.
func (p *shutdownProvider) SendKeys(name string, keys ...string) error {
	for _, key := range keys {
		if key == "Escape" {
			return p.Interrupt(name)
		}
	}
	return nil
}
