//go:build windows

package exec

import (
	"os"
	"testing"
)

func mkfifo(t *testing.T, _ string) {
	t.Helper()
	t.Skip("FIFO barriers between a test and its adapter script are unix-only")
}

func releaseBarrier(*testing.T, string) *os.File { return nil }
