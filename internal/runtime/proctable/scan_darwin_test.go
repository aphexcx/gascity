//go:build darwin

package proctable

import "testing"

func TestScanPSRecordsExcludesInfrastructure(t *testing.T) {
	env := map[string]string{"GC_SESSION_ID": "ci-x"}
	records := map[int]psRecord{
		1:    {pid: 1, command: "launchd"},
		5000: {pid: 5000, ppid: 1, command: "tmux -u -L city new-session -d -s worker -c /tmp", env: env},
		5001: {pid: 5001, ppid: 5000, command: "claude", env: env},
		5002: {pid: 5002, ppid: 5001, command: "sh", env: env},
	}
	for _, id := range []string{"ci-x", ""} {
		got := scanPSRecords(records, id)
		if len(got) != 1 || got[0].PID != 5001 || got[0].SessionID != "ci-x" {
			t.Errorf("scanPSRecords(%q) = %v, want only pane root 5001", id, got)
		}
	}
}
