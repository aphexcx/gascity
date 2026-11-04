package proctable

import (
	"errors"
	"strings"
)

// ErrProcessGone means a process identity cannot be read because that process
// no longer exists. Callers may treat it as a safe stale-target outcome.
var ErrProcessGone = errors.New("process is gone")

func bootQualifiedIdentity(boot, ticks string) (string, error) {
	boot = strings.TrimSpace(boot)
	if boot == "" || ticks == "" {
		return "", errors.New("process identity requires boot ID and start ticks")
	}
	return boot + ":" + ticks, nil
}
