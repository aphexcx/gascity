//go:build darwin || linux

package proctable

import (
	"errors"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestTerminateProcessGroupIdentityFence(t *testing.T) {
	for _, state := range []string{"gone", "reused", "denied", "term", "kill", "survivor", "reused-before-term", "reused-before-kill", "signal-error"} {
		t.Run(state, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var signals []syscall.Signal
				reads := 0
				started := time.Now()
				read := func(int) (string, error) {
					reads++
					switch {
					case state == "gone", state == "term" && len(signals) == 1, state == "kill" && len(signals) == 2:
						return "", ErrProcessGone
					case state == "reused", state == "reused-before-term" && reads >= 2, state == "reused-before-kill" && time.Since(started) >= runtime.ManagedProcessStopGrace:
						return "replacement", nil
					case state == "denied":
						return "", syscall.EPERM
					default:
						return "original", nil
					}
				}
				err := TerminateProcessGroup(42, "original", read, func(pid int, sig syscall.Signal) error {
					if pid != -42 {
						t.Fatalf("signal target = %d, want group -42", pid)
					}
					if sig == 0 {
						return syscall.ESRCH
					}
					signals = append(signals, sig)
					if state == "signal-error" {
						return syscall.EPERM
					}
					return nil
				})
				wantError := state == "denied" || state == "survivor" || state == "signal-error"
				if (err != nil) != wantError {
					t.Fatalf("error = %v, want error %v", err, wantError)
				}
				want := 0
				switch state {
				case "term", "reused-before-kill", "signal-error":
					want = 1
				case "kill", "survivor":
					want = 2
				}
				if len(signals) != want {
					t.Fatalf("signals = %v, want %d", signals, want)
				}
				if want > 0 && signals[0] != syscall.SIGTERM {
					t.Fatal(signals)
				}
				if want == 2 && signals[1] != syscall.SIGKILL {
					t.Fatal(signals)
				}
			})
		})
	}
}

func TestProcessGroupIdentityProbe(t *testing.T) {
	for _, tc := range []struct {
		name    string
		probe   error
		live    bool
		wantErr error
	}{
		{name: "present", live: true},
		{name: "absent", probe: syscall.ESRCH},
		{name: "permission denied", probe: syscall.EPERM, live: true},
		{name: "probe failure", probe: syscall.EIO, wantErr: syscall.EIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probes := 0
			live, err := ProcessGroupIdentityMatches(42, "original", func(int) (string, error) {
				return "", ErrProcessGone
			}, func(pid int, sig syscall.Signal) error {
				if pid != -42 || sig != 0 {
					t.Fatalf("probe = (%d, %v), want (-42, 0)", pid, sig)
				}
				probes++
				return tc.probe
			})
			if probes != 1 || live != tc.live || !errors.Is(err, tc.wantErr) {
				t.Fatalf("probes=%d live=%v err=%v; want one probe, live=%v err=%v", probes, live, err, tc.live, tc.wantErr)
			}
		})
	}
}

func TestTerminateProcessGroupUsesProbe(t *testing.T) {
	probes := 0
	err := TerminateProcessGroup(42, "original", func(int) (string, error) {
		return "", ErrProcessGone
	}, func(pid int, sig syscall.Signal) error {
		if pid != -42 || sig != 0 {
			t.Fatalf("probe = (%d, %v), want (-42, 0)", pid, sig)
		}
		probes++
		return syscall.EIO
	})
	if probes != 1 || !errors.Is(err, syscall.EIO) {
		t.Fatalf("probes=%d err=%v; want one injected probe and its error", probes, err)
	}
}

func TestProcessIdentityMatchesInvalidAndEmpty(t *testing.T) {
	for _, pid := range []int{-1, 0, 1} {
		if _, err := ProcessIdentityMatches(pid, "start", func(int) (string, error) { t.Fatal("invalid pid read"); return "", nil }); err == nil {
			t.Fatal("invalid identity accepted")
		}
	}
	if live, err := ProcessIdentityMatches(42, "", func(int) (string, error) { return "new", nil }); err != nil || live {
		t.Fatalf("exited-at-spawn identity = %v, %v", live, err)
	}
	if _, err := ProcessIdentityMatches(42, "start", func(int) (string, error) { return "", syscall.EPERM }); !errors.Is(err, syscall.EPERM) {
		t.Fatal(err)
	}
}

func TestTerminateProcessGroupMissingLeaderSurvivor(t *testing.T) {
	cmd, _ := startKillTestProcess(t, "sleep", []string{"60"}, nil)
	synctest.Test(t, func(t *testing.T) {
		signals := 0
		started := time.Now()
		err := TerminateProcessGroup(cmd.Process.Pid, "departed-leader", func(int) (string, error) {
			return "", ErrProcessGone
		}, func(pid int, sig syscall.Signal) error {
			if sig == 0 {
				return syscall.Kill(pid, sig)
			}
			signals++
			return nil
		})
		if err == nil || signals != 0 {
			t.Fatalf("live group without leader identity: err=%v signals=%d; want refusal without signals", err, signals)
		}
		if elapsed := time.Since(started); elapsed != runtime.ManagedProcessStopGrace {
			t.Fatalf("leaderless group grace = %s, want %s", elapsed, runtime.ManagedProcessStopGrace)
		}
	})
}

func TestTerminateProcessGroupWaitsForLeaderlessDrain(t *testing.T) {
	cmd, exited := startKillTestProcess(t, "sleep", []string{"60"}, nil)
	synctest.Test(t, func(t *testing.T) {
		reads, signals := 0, 0
		started := time.Now()
		err := TerminateProcessGroup(cmd.Process.Pid, "departed-leader", func(int) (string, error) {
			reads++
			// Keep the group through the waiter's first probe, then model
			// descendants completing an already-accepted shutdown.
			if reads == 5 {
				if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				select {
				case <-exited:
				case <-time.After(time.Second):
					t.Fatal("test group did not finish draining")
				}
			}
			return "", ErrProcessGone
		}, func(pid int, sig syscall.Signal) error {
			if sig == 0 {
				return syscall.Kill(pid, sig)
			}
			signals++
			return nil
		})
		if err != nil || signals != 0 {
			t.Fatalf("draining leaderless group: err=%v signals=%d; want success without signals", err, signals)
		}
		if elapsed := time.Since(started); elapsed < 10*time.Millisecond || elapsed >= runtime.ManagedProcessStopGrace {
			t.Fatalf("drain observed after %s; want a polling interval before the grace bound", elapsed)
		}
	})
}
