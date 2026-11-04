package proctable

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ProcessIdentityMatches compares a saved start token with the current kernel
// identity. Only a missing process or a different token proves absence.
func ProcessIdentityMatches(pid int, start string, read func(int) (string, error)) (bool, error) {
	if pid <= 1 {
		return false, fmt.Errorf("invalid recorded process PID %d", pid)
	}
	current, err := read(pid)
	if errors.Is(err, ErrProcessGone) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading process identity for PID %d: %w", pid, err)
	}
	if current == "" {
		return false, fmt.Errorf("empty process identity for PID %d", pid)
	}
	return current == start, nil
}

// ProcessGroupIdentityMatches also checks for surviving descendants when the
// recorded leader has exited. A reused leader belongs to a different generation.
func ProcessGroupIdentityMatches(pid int, start string, read func(int) (string, error)) (bool, error) {
	if pid <= 1 {
		return false, fmt.Errorf("invalid recorded process PID %d", pid)
	}
	current, err := read(pid)
	if errors.Is(err, ErrProcessGone) {
		err = syscall.Kill(-pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		if err == nil || errors.Is(err, syscall.EPERM) {
			return true, nil
		}
		return false, fmt.Errorf("probing process group %d: %w", pid, err)
	}
	if err != nil {
		return false, fmt.Errorf("reading process identity for PID %d: %w", pid, err)
	}
	if current == "" {
		return false, fmt.Errorf("empty process identity for PID %d", pid)
	}
	return current == start, nil
}

// TerminateProcessGroup stops a recorded group leader with TERM, then KILL after
// the managed grace. Each signal is fenced by a fresh kernel identity read, and
// success requires observing the saved group gone, including after KILL.
func TerminateProcessGroup(pid int, start string, read func(int) (string, error), signal func(int, syscall.Signal) error) error {
	if live, err := ProcessGroupIdentityMatches(pid, start, read); err != nil || !live {
		return err
	}
	for _, phase := range []struct {
		signal syscall.Signal
		grace  time.Duration
	}{
		{syscall.SIGTERM, runtime.ManagedProcessStopGrace},
		{syscall.SIGKILL, runtime.ManagedProcessReapGrace},
	} {
		if live, err := ProcessIdentityMatches(pid, start, read); err != nil || !live {
			if err != nil {
				return err
			}
			if groupLive, err := ProcessGroupIdentityMatches(pid, start, read); err != nil || !groupLive {
				return err
			}
			// A previous tracked shutdown may have reaped the leader while
			// its descendants are still exiting. Observe the existing grace
			// without sending a signal that can no longer be identity-fenced.
			if gone, err := waitForProcessIdentityGone(pid, start, phase.grace, read); err != nil || gone {
				return err
			}
			return fmt.Errorf("process group %d survives without its recorded leader; refusing an unfenced signal", pid)
		}
		if err := signal(-pid, phase.signal); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("signaling process group %d: %w", pid, err)
		}
		gone, err := waitForProcessIdentityGone(pid, start, phase.grace, read)
		if err != nil || gone {
			return err
		}
	}
	return fmt.Errorf("process PID %d remains after shutdown", pid)
}

func waitForProcessIdentityGone(pid int, start string, grace time.Duration, read func(int) (string, error)) (bool, error) {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		live, err := ProcessGroupIdentityMatches(pid, start, read)
		if err != nil || !live {
			return !live, err
		}
		select {
		case <-timer.C:
			return false, nil
		case <-ticker.C:
		}
	}
}
