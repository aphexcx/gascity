//go:build linux || darwin

package proctable

import (
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRuntimeSignalsPreserveInfrastructureInAgentGroup(t *testing.T) {
	for _, tc := range []struct {
		name        string
		peerArgv    []string
		peerGroup   int
		inspectErr  error
		snapshotErr error
		wantTarget  int
		groupErr    error
	}{
		{"legacy dolt shares agent group", []string{"/opt/bin/dolt", "sql-server"}, 100, nil, nil, 0, nil},
		{"watchdog shares agent group", []string{"gc", ManagedDoltScopeWatchdogVerb}, 100, nil, nil, 0, nil},
		{"isolated dolt", []string{"dolt", "sql-server"}, 200, nil, nil, -100, nil},
		{"ordinary agent child", []string{"sh", "-c", "work"}, 100, nil, nil, -100, nil},
		{"unreadable group member", nil, 100, errors.New("unreadable argv"), nil, 0, nil},
		{"unavailable process table", nil, 100, nil, errors.New("snapshot failed"), 0, nil},
		{"ordinary group syscall failure permits PID fallback", []string{"agent"}, 100, nil, nil, 100, syscall.EPERM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var targets []int
			var signals []syscall.Signal
			snapshot := func() ([]ProcessRecord, error) {
				return []ProcessRecord{{PID: 100, PGID: 100}, {PID: 101, PGID: tc.peerGroup}, {PID: 102, PPID: 100, PGID: 100}}, tc.snapshotErr
			}
			argv := func(pid int) ([]string, error) {
				if pid == 100 || pid == 102 {
					return []string{"agent"}, nil
				}
				return tc.peerArgv, tc.inspectErr
			}
			kill := func(pid int, sig syscall.Signal) error {
				targets = append(targets, pid)
				signals = append(signals, sig)
				if pid < 0 {
					return tc.groupErr
				}
				return nil
			}
			guarded := func(pid int, sig syscall.Signal) error {
				return signalRuntimeWith(pid, sig, snapshot, argv, kill)
			}
			// The ordinary child is hidden inside its parent's scan envelope.
			// A mixed group must fail before signaling, not kill just the root
			// and claim cleanup succeeded while that child is still running.
			err := killByPID(100, guarded, func(int) bool { return true }, func(int) bool { return false }, 0, 0)
			if tc.wantTarget == 0 {
				if !errors.Is(err, errUnsafeRuntimeGroup) || len(targets) != 0 {
					t.Fatalf("unsafe group cleanup: err=%v targets=%v, want refusal without signals", err, targets)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantTargets := []int{tc.wantTarget, tc.wantTarget}
			wantSignals := []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}
			if tc.groupErr != nil {
				wantTargets = []int{-100, tc.wantTarget, -100, tc.wantTarget}
				wantSignals = []syscall.Signal{syscall.SIGTERM, syscall.SIGTERM, syscall.SIGKILL, syscall.SIGKILL}
			}
			if !slices.Equal(targets, wantTargets) || !slices.Equal(signals, wantSignals) {
				t.Fatalf("targets=%v signals=%v; want targets=%v signals=%v", targets, signals, wantTargets, wantSignals)
			}
		})
	}
}

func TestKillByPIDRefusesLowPIDs(t *testing.T) {
	for _, pid := range []int{-1, 0, 1} {
		if err := KillByPID(pid); err == nil {
			t.Errorf("KillByPID(%d) succeeded, want error", pid)
		}
	}
}

func TestKillByPIDAlreadyGoneIsSuccess(t *testing.T) {
	// Spawn a short-lived process and wait for it to exit, then try to kill it.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawning test process: %v", err)
	}
	pid := cmd.ProcessState.Pid()
	// Process is already gone. KillByPID should return nil (ESRCH → success).
	if err := KillByPID(pid); err != nil {
		t.Fatalf("KillByPID(%d) for already-dead process: %v", pid, err)
	}
}

func TestSignalPIDGroupThenFallback(t *testing.T) {
	var got []int
	err := signalPIDWith(12345, syscall.SIGTERM, func(pid int, sig syscall.Signal) error {
		if sig != syscall.SIGTERM {
			t.Fatalf("signal = %v, want SIGTERM", sig)
		}
		got = append(got, pid)
		if pid < 0 {
			return syscall.ESRCH
		}
		return nil
	})
	if err != nil {
		t.Fatalf("signalPIDWith(): %v", err)
	}
	want := []int{-12345, 12345}
	if len(got) != len(want) {
		t.Fatalf("signal calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("signal calls = %v, want %v", got, want)
		}
	}
}

func TestSignalPIDGroupSuccessSkipsFallback(t *testing.T) {
	var got []int
	err := signalPIDWith(12345, syscall.SIGTERM, func(pid int, sig syscall.Signal) error {
		if sig != syscall.SIGTERM {
			t.Fatalf("signal = %v, want SIGTERM", sig)
		}
		got = append(got, pid)
		return nil
	})
	if err != nil {
		t.Fatalf("signalPIDWith(): %v", err)
	}
	want := []int{-12345}
	if !slices.Equal(got, want) {
		t.Fatalf("signal calls = %v, want %v", got, want)
	}
}

// TestKillByPIDConfirmedDeadBeforeReturn drives the injected core: a process
// still runnable after SIGKILL (e.g. wedged in D-state) must yield an error so
// a caller can refuse to start a racing replacement, while one that becomes
// dead (gone or zombie) after SIGKILL returns nil.
func TestKillByPIDConfirmedDeadBeforeReturn(t *testing.T) {
	t.Run("survives SIGKILL -> error", func(t *testing.T) {
		var signals []syscall.Signal
		kill := func(_ int, sig syscall.Signal) error {
			// Record every delivery attempt. signalPIDWith signals the process
			// group (negative pid) first and returns on success, so with this
			// always-succeeding fake these are the group deliveries; the
			// assertion below only checks the final escalation is SIGKILL.
			signals = append(signals, sig)
			return nil
		}
		termLive := func(int) bool { return true } // never exits on SIGTERM
		runLive := func(int) bool { return true }  // survives SIGKILL too
		err := killByPID(4321, kill, termLive, runLive, 5*time.Millisecond, 5*time.Millisecond)
		if err == nil {
			t.Fatal("killByPID returned nil for a process that survived SIGKILL")
		}
		if !strings.Contains(err.Error(), "not confirmed dead") {
			t.Fatalf("error = %v, want 'not confirmed dead'", err)
		}
		if len(signals) == 0 || signals[len(signals)-1] != syscall.SIGKILL {
			t.Fatalf("signals = %v, want SIGKILL escalation", signals)
		}
	})

	t.Run("dies after SIGKILL -> nil", func(t *testing.T) {
		kill := func(int, syscall.Signal) error { return nil }
		termLive := func(int) bool { return true } // ignores SIGTERM
		var kills int
		runLive := func(int) bool {
			kills++
			return kills <= 1 // alive on first confirm poll, dead after
		}
		if err := killByPID(4321, kill, termLive, runLive, 5*time.Millisecond, time.Second); err != nil {
			t.Fatalf("killByPID: %v", err)
		}
	})

	t.Run("exits during SIGTERM grace -> no SIGKILL", func(t *testing.T) {
		var sawKill bool
		kill := func(_ int, sig syscall.Signal) error {
			if sig == syscall.SIGKILL {
				sawKill = true
			}
			return nil
		}
		var polls int
		termLive := func(int) bool {
			polls++
			return polls <= 1 // alive at entry, exits before grace elapses
		}
		runLive := func(int) bool { return false }
		if err := killByPID(4321, kill, termLive, runLive, time.Second, time.Second); err != nil {
			t.Fatalf("killByPID: %v", err)
		}
		if sawKill {
			t.Fatal("SIGKILL sent even though the process exited during grace")
		}
	})
}

func TestWaitUntilRespectsZeroTimeout(t *testing.T) {
	if !waitUntil(func() bool { return true }, 0) {
		t.Fatal("waitUntil should observe an already-satisfied condition at zero timeout")
	}
	if waitUntil(func() bool { return false }, 0) {
		t.Fatal("waitUntil should report false when the condition never holds at zero timeout")
	}
}

// TestKillLivenessFuncsAreIdentityBound pins the recycled-PID protection at the
// wiring, not just at the helper.
//
// The SIGTERM-grace probe used to be a bare kill(0) existence check, and it is
// what gates the SIGKILL wave: a target that answered SIGTERM and was reaped,
// whose PID was recycled before the grace expired, still read as "alive", so
// SIGKILL was sent to the new owner. signalPIDWith tries kill(-pid) first, so if
// that owner leads a process group — every tmux pane command and every Setpgid'd
// daemon does — the whole unrelated group dies and the call reports success.
//
// Both probes must therefore be bound to the ORIGINAL target's start-time
// identity: asked about a different live PID they must answer false. Restoring
// either to a bare existence check fails this.
func TestKillLivenessFuncsAreIdentityBound(t *testing.T) {
	self := os.Getpid()
	termLive, runLive := killLivenessFuncsForPID(self)

	if !termLive(self) {
		t.Error("termLive(self) = false; the probe must recognize its own target")
	}
	if !runLive(self) {
		t.Error("runLive(self) = false; the probe must recognize its own target")
	}

	// PID 1 is always live and is never us, so it stands in for a recycled PID
	// now owned by an unrelated process.
	const recycled = 1
	if termLive(recycled) {
		t.Error("termLive reported a DIFFERENT live pid as our target: a recycled PID would receive a process-group SIGKILL")
	}
	if runLive(recycled) {
		t.Error("runLive reported a DIFFERENT live pid as our target")
	}
}
