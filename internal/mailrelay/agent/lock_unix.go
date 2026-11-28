//go:build unix

package agent

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// errLocked reports that another process holds the folder.
var errLocked = errors.New("held by another process")

// lockDir takes an advisory lock on a folder itself, so no lock file is
// left behind. Without wait it fails at once when the folder is held.
func lockDir(dir string, wait bool) (func(), error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("locking %s: %w", dir, err)
	}
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("locking %s: %w", dir, errLocked)
		}
		return nil, fmt.Errorf("locking %s: %w", dir, err)
	}
	return func() { _ = f.Close() }, nil
}
