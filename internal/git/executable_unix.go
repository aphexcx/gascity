//go:build !windows

package git

import (
	"os/exec"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/processgroup"
)

// ConfigureCommandCancellation makes cancellation stop a Git command's process
// group. Call the returned cleanup after Wait too, to stop surviving descendants.
func ConfigureCommandCancellation(cmd *exec.Cmd) func() error {
	processgroup.StartCommandInNewGroup(cmd)
	cleanup := sync.OnceValue(func() error {
		if cmd.Process == nil {
			return nil
		}
		// The group remains ours even if its leader exits before cancellation.
		return processgroup.TerminateCommand(cmd, cmd.Process.Pid, 100*time.Millisecond, processgroup.Options{})
	})
	cmd.Cancel = cleanup
	// WaitDelay can return before cancellation when descendants hold the pipes.
	return cleanup
}
