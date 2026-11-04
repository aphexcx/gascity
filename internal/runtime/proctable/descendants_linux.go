//go:build linux

package proctable

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// snapshotProcesses walks /proc once for a host-wide process snapshot. PPID,
// PGID, and start time come from the same /proc/<pid>/stat read, so teardown
// selection never has to stitch identity together with per-node live probes.
// There is no liveScanGuard (that guard protects the orphan sweep in
// ScanBySessionID, not this read-only snapshot) and no root filtering.
func snapshotProcesses() ([]ProcessRecord, error) {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, fmt.Errorf("reading process boot identity: %w", err)
	}
	if _, err := bootQualifiedIdentity(string(boot), "0"); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var records []ProcessRecord
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		ppid, pgid, startTime, ok, err := readProcStatIdentity(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil || !ok {
			continue
		}
		startTime, err = bootQualifiedIdentity(string(boot), startTime)
		if err != nil {
			return nil, err
		}
		comm, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue
		}
		rec := ProcessRecord{
			PID:       pid,
			PPID:      ppid,
			PGID:      pgid,
			StartTime: startTime,
			Name:      strings.TrimSpace(string(comm)),
		}
		if env, err := parseEnvironFile(filepath.Join("/proc", e.Name(), "environ")); err == nil {
			rec.SessionID = env["GC_SESSION_ID"]
		}
		records = append(records, rec)
	}
	return records, nil
}
