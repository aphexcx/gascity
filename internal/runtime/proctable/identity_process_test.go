//go:build darwin || linux

package proctable

import (
	"os/exec"
	"testing"
)

func TestProcessIdentityOpaqueChild(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	identity, err := ProcessIdentity(cmd.Process.Pid)
	if err != nil || identity == "" {
		t.Fatalf("ProcessIdentity: %q, %v", identity, err)
	}
	records, err := SnapshotProcesses()
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.PID == cmd.Process.Pid {
			if record.StartTime != identity {
				t.Fatalf("snapshot identity = %q, direct identity = %q", record.StartTime, identity)
			}
			return
		}
	}
	t.Fatalf("sleep PID %d missing from snapshot", cmd.Process.Pid)
}
