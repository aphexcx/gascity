package session

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// ErrControllerRestartPending means the controller owns the next incarnation.
// Runtime-only callers wait without holding the session lock.
var ErrControllerRestartPending = errors.New("session has a pending restart owned by the controller")

// WithRestartCoordinator supplies live controller reachability and its wakeup
// operation. Without a reachable controller, direct startup retains its behavior.
func WithRestartCoordinator(running func() bool, poke func() error) ManagerOption {
	return func(m *Manager) { m.controllerRunning, m.pokeController = running, poke }
}

// WithWaitNudgeWithdrawal supplies cleanup against the caller's nudges store.
// The callback is invoked only after a wake cancels waits, never during setup.
func WithWaitNudgeWithdrawal(withdraw func([]string) error) ManagerOption {
	return func(m *Manager) { m.withdrawWaitNudges = withdraw }
}

func (m *Manager) restartOrKillPending(b beads.Bead) bool {
	return pendingConversationRestart(b) || KillPendingMetadata(b.Metadata["state"], b.Metadata["state_reason"], b.Metadata["sleep_reason"], b.Metadata["slept_at"], m.now())
}

func (m *Manager) controllerOwnsRestart(b beads.Bead) bool {
	return m.restartOrKillPending(b) && m.controllerRunning != nil && m.controllerRunning()
}

func (m *Manager) controllerRestartPending(b beads.Bead) bool {
	return wasConfiguredNamedSession(b) && m.controllerOwnsRestart(b)
}

// ControllerRestartPending reports whether a direct Start or Attach would wait
// for the controller. This snapshot is only a preparation-cost hint; the manager
// repeats the authoritative check under the session mutation lock.
func (m *Manager) ControllerRestartPending(id string) (bool, error) {
	b, _, err := m.sessionBead(id)
	if err != nil {
		return false, err
	}
	return m.controllerRestartPending(b), nil
}

// controllerDefersMessage is evaluated once at a message entry, under the
// session lock. A live interrupt can subsequently replace the runtime itself.
func (m *Manager) controllerDefersMessage(b beads.Bead, sessName string) bool {
	if KillPendingMetadata(b.Metadata["state"], b.Metadata["state_reason"], b.Metadata["sleep_reason"], b.Metadata["slept_at"], m.now()) {
		return false
	}
	return (pendingConversationRestart(b) || !m.sp.IsRunning(sessName)) &&
		m.controllerRunning != nil && m.controllerRunning()
}

func (m *Manager) queueControllerMessage(b beads.Bead, sessName, message string) error {
	if err := m.enqueueDeferredSubmitLocked(b, sessName, message); err != nil {
		return err
	}
	if !m.restartOrKillPending(b) {
		switch State(b.Metadata["state"]) {
		case StateAsleep, StateDrained, StateSuspended:
			res, err := m.PersistedStore().WakeSession(b.ID, m.now(), WakeOpts{})
			if err != nil {
				log.Printf("session %s: recording wake after queued message: %v", b.ID, err)
			} else if len(res.NudgeIDs) > 0 {
				withdraw := m.withdrawWaitNudges
				if withdraw == nil {
					withdraw = func(ids []string) error { return nudgequeue.WithdrawWaitNudges(nil, m.cityPath, ids) }
				}
				if err := withdraw(res.NudgeIDs); err != nil {
					log.Printf("session %s: withdrawing wait nudges after queued message: %v", b.ID, err)
				}
			}
		}
	}
	m.pokeRestartCoordinator()
	return nil
}

func (m *Manager) pokeRestartCoordinator() {
	if m.pokeController != nil {
		if err := m.pokeController(); err != nil {
			log.Printf("session: poking restart coordinator: %v", err)
		}
	}
}

func (m *Manager) awaitControllerRestart(ctx context.Context, operation func() error) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for pending restart owned by controller: %w", err)
		}
		err := operation()
		if !errors.Is(err, ErrControllerRestartPending) {
			return err
		}
		m.pokeRestartCoordinator()
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("waiting for pending restart owned by controller: %w", ctx.Err())
		case <-timer.C:
		}
	}
}
