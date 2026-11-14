package session

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
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
	if !wasConfiguredNamedSession(b) || m.controllerRunning == nil || !m.controllerRunning() {
		return false
	}
	// Preparation consumes restart markers before the runtime starts. These
	// states retain the controller's ownership across that unlocked interval.
	return m.restartOrKillPending(b) || b.Metadata["state"] == string(StateCreating) || b.Metadata["state"] == string(StateStartPending)
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
	itemID, err := m.enqueueSubmitLocked(b, message)
	if err != nil {
		return err
	}
	if err := m.requestControllerWake(b); err != nil {
		rollbackErr := nudgequeue.WithState(m.cityPath, func(state *nudgequeue.State) error {
			matches := func(item nudgequeue.Item) bool { return item.ID == itemID }
			state.Pending = slices.DeleteFunc(state.Pending, matches)
			state.InFlight = slices.DeleteFunc(state.InFlight, matches)
			return nil
		})
		if rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("rolling back queued message %s: %w", itemID, rollbackErr))
		}
		return err
	}
	m.activateDeferredSubmitLocked(b, sessName)
	m.pokeRestartCoordinator()
	return nil
}

// requestControllerWake supplies demand before handing a session to the
// controller. A reset alone only promises a fresh conversation at the next
// wake; a restart request or committed reset already schedules that wake.
func (m *Manager) requestControllerWake(b beads.Bead) error {
	now := m.now()
	input := LifecycleInputFromMetadata(b.Status, b.Metadata)
	input.Now = now
	view := ProjectLifecycle(input)
	if strings.TrimSpace(b.Metadata["restart_requested"]) == "true" ||
		view.BaseState == BaseStateCreating || view.BaseState == BaseStateStartPending ||
		KillPendingMetadata(b.Metadata["state"], b.Metadata["state_reason"], b.Metadata["sleep_reason"], b.Metadata["slept_at"], now) {
		return nil
	}
	if len(view.Blockers) == 0 && b.Metadata["wait_hold"] != "true" {
		if view.HasWakeCause(WakeCauseExplicit) ||
			(view.BaseState != BaseStateDrained && strings.TrimSpace(b.Metadata["continuation_reset_pending"]) == "true" && strings.TrimSpace(b.Metadata[ResetCommittedAtKey]) != "") {
			return nil
		}
	}
	res, err := m.PersistedStore().WakeSession(b.ID, now, WakeOpts{})
	if err != nil {
		var conflict *WakeConflictError
		if errors.As(err, &conflict) {
			log.Printf("session %s: recording controller wake: %v", b.ID, err)
			return nil
		}
		return fmt.Errorf("recording controller wake for session %s: %w", b.ID, err)
	}
	if len(res.NudgeIDs) > 0 {
		withdraw := m.withdrawWaitNudges
		if withdraw == nil {
			withdraw = func(ids []string) error { return nudgequeue.WithdrawWaitNudges(nil, m.cityPath, ids) }
		}
		if err := withdraw(res.NudgeIDs); err != nil {
			// The wake is already durable; cleanup failure cannot strand the
			// accepted message, but stale wait nudges still need reporting.
			log.Printf("session %s: withdrawing wait nudges after controller wake: %v", b.ID, err)
		}
	}
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
	var nextPoke time.Time
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for pending restart owned by controller: %w", err)
		}
		err := operation()
		if !errors.Is(err, ErrControllerRestartPending) {
			return err
		}
		if !time.Now().Before(nextPoke) {
			m.pokeRestartCoordinator()
			nextPoke = time.Now().Add(time.Second)
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("waiting for pending restart owned by controller: %w", ctx.Err())
		case <-timer.C:
		}
	}
}
