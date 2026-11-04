//go:build linux || darwin

package runtime

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

func TestProviderStateDirectoryLockExcludesAnotherHandle(t *testing.T) {
	dir := t.TempDir()
	if err := WithProviderStateDirLock(dir, func() error {
		other, err := os.Open(dir)
		if err != nil {
			return err
		}
		defer other.Close() //nolint:errcheck
		err = syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			t.Fatalf("second handle lock = %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := WithProviderStateDirLock(dir, func() error { return nil }); err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("directory lock created artifacts: %v, %v", entries, err)
	}
}
