package proctable

import (
	"errors"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ScanAll returns all live agent root processes with a non-empty
// GC_SESSION_ID.
func ScanAll() ([]runtime.LiveRuntime, error) {
	return ScanBySessionID("")
}

// HasSessionProcess reports whether a process carries the non-empty session ID.
// This read-only query exposes no PIDs that a caller could reap, so it does not
// need the live-scan guard used by ScanBySessionID and the orphan sweeper.
func HasSessionProcess(id string) (bool, error) {
	if id == "" {
		return false, errors.New("checking session process: empty session ID")
	}
	processes, err := scanBySessionID(id)
	return len(processes) > 0, err
}
