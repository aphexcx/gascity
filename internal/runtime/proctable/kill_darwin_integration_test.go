//go:build darwin && integration

package proctable

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestKillByPIDDarwinGroupInspection owns the real kern.proc.all/procargs2
// boundary: an unreaped child must not prevent cleanup, while a live Dolt
// look-alike must protect the entire group from signals.
func TestKillByPIDDarwinGroupInspection(t *testing.T) {
	for _, mode := range []string{"zombie", "infrastructure"} {
		t.Run(mode, func(t *testing.T) {
			leader, child, exited := startDarwinInspectionGroup(t, mode)
			var observed string
			if !waitUntil(func() bool {
				process, err := unix.SysctlKinfoProc("kern.proc.pid", child)
				if err != nil || process == nil {
					observed = fmt.Sprintf("kinfo=%v err=%v", process, err)
					return false
				}
				args, argvErr := rootArgv(child)
				observed = fmt.Sprintf("pid=%d pgid=%d state=%d argv=%q argvErr=%v", child, process.Eproc.Pgid, process.Proc.P_stat, args, argvErr)
				if int(process.Eproc.Pgid) != leader {
					return false
				}
				if mode == "zombie" {
					return process.Proc.P_stat == darwinZombieState && errors.Is(argvErr, syscall.EINVAL)
				}
				return process.Proc.P_stat != darwinZombieState && argvErr == nil && IsCityInfrastructureArgv(args)
			}, 10*time.Second) {
				t.Fatalf("group fixture not ready: %s", observed)
			}
			t.Logf("real Darwin reader before cleanup: %s", observed)

			records, err := SnapshotProcesses()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, record := range records {
				if record.PID == child && record.PGID == leader {
					found = true
				}
			}
			if !found {
				t.Fatal("real snapshot omitted the child from the leader's group")
			}

			err = KillByPID(leader)
			t.Logf("KillByPID(%d) = %v", leader, err)
			if mode == "infrastructure" {
				if !errors.Is(err, errUnsafeRuntimeGroup) {
					t.Fatalf("mixed group cleanup = %v, want unsafe-group refusal", err)
				}
				for _, pid := range []int{leader, child} {
					if err := syscall.Kill(pid, 0); err != nil {
						t.Fatalf("protected PID %d is gone after refusal: %v", pid, err)
					}
					process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
					if err != nil || process == nil || process.Proc.P_stat == darwinZombieState {
						t.Fatalf("protected PID %d is not runnable after refusal: process=%v err=%v", pid, process, err)
					}
				}
				t.Log("refused; leader and live dolt sql-server look-alike remain alive")
				return
			}
			if err != nil {
				t.Fatalf("zombie child prevented runtime cleanup: %v (leader alive=%t)", err, syscall.Kill(leader, 0) == nil)
			}
			select {
			case <-exited:
			case <-time.After(10 * time.Second):
				t.Fatal("leader was not reaped after successful cleanup")
			}
			if err := syscall.Kill(leader, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("leader still exists after cleanup: %v", err)
			}
			t.Log("killed; leader reaped and absent")
		})
	}
}

func startDarwinInspectionGroup(t *testing.T, mode string) (int, int, <-chan struct{}) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("Darwin group integration proof requires python3:", err)
	}
	dir := t.TempDir()
	// The shell keeps its exact argv (dolt sql-server) while blocked on stdin.
	if err := os.WriteFile(filepath.Join(dir, "sql-server"), []byte("read unused\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const script = `import os, signal, sys
child = os.fork()
if child == 0:
    if sys.argv[1] == "zombie":
        os._exit(0)
    os.execv("/bin/sh", ["dolt", "sql-server"])
print(child, flush=True)
while True:
    signal.pause()
`
	stdin, input, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = input.Close() })
	stdout, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdout.Close(); _ = writer.Close() })
	cmd, exited := startKillTestProcess(t, python, []string{"-c", script, mode}, func(cmd *exec.Cmd) {
		cmd.Dir = dir
		cmd.Stdin = stdin
		cmd.Stdout = writer
		cmd.Stderr = os.Stderr
	})
	if err := stdout.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("reading child PID: %v", err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || child <= 1 {
		t.Fatalf("invalid child PID %q: %v", line, err)
	}
	return cmd.Process.Pid, child, exited
}
