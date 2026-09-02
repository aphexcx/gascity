//go:build unix

package exec

import (
	"os"
	"syscall"
	"testing"
)

// mkfifo creates a named pipe at path. Opening a FIFO blocks until both ends
// are present, which is what makes it a barrier between a test and the adapter
// script it drives: the test's write-side open returns exactly when the script
// reaches its read of the pipe, and not before.
func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo %s: %v", path, err)
	}
}

// releaseBarrier pairs a reader with a write-side open that is still blocked
// because the script never reached the pipe, so the test's opener goroutine
// can return on a failure path instead of leaking. A non-blocking read open
// succeeds whether or not a writer is present. The returned file, if any,
// must stay open until the blocked opener has returned.
func releaseBarrier(t *testing.T, path string) *os.File {
	t.Helper()
	r, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Logf("release barrier %s: %v", path, err)
		return nil
	}
	return r
}
