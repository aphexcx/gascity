//go:build !linux && !darwin

package runtime

import "fmt"

// WithProviderStateDirLock reports unsupported local process state locking.
func WithProviderStateDirLock(_ string, _ func() error) error {
	return fmt.Errorf("provider state directory locking is unsupported on this platform")
}
