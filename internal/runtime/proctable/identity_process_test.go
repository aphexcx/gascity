//go:build darwin || linux

package proctable

import (
	"syscall"
	"testing"
)

func TestProcessIdentityOpaqueChild(t *testing.T) {
	cmd, _ := startKillTestProcess(t, "/bin/sleep", []string{"300"}, nil)
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

func TestProcessIdentityBootFence(t *testing.T) {
	old, err := bootQualifiedIdentity("boot-a", "1234")
	if err != nil {
		t.Fatal(err)
	}
	current, err := bootQualifiedIdentity("boot-b", "1234")
	if err != nil {
		t.Fatal(err)
	}
	if err := TerminateProcessGroup(42, old, func(int) (string, error) { return current, nil }, func(int, syscall.Signal) error {
		t.Fatal("same PID/ticks from a different boot must never be signaled")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"", "1234"}, {"boot-a", ""}} {
		if _, err := bootQualifiedIdentity(pair[0], pair[1]); err == nil {
			t.Fatalf("incomplete identity accepted: %q", pair)
		}
	}
}
