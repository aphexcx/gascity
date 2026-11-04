//go:build linux || darwin

package runtime

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// WithProviderStateDirLock serializes publication and cleanup through a kernel
// lock on the existing provider directory. It creates no lock or status file;
// closing the directory descriptor, including on process exit, releases it.
func WithProviderStateDirLock(dir string, operation func() error) (result error) {
	if err := EnsurePrivateDir(dir); err != nil {
		return err
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return fmt.Errorf("locking provider state directory: %w", err)
	}
	defer func() { result = errors.Join(result, f.Close()) }()
	return operation()
}
