//go:build !windows

package git

import (
	"os/exec"
	"time"

	"github.com/gastownhall/gascity/internal/processgroup"
)

func configureExecutableProbe(cmd *exec.Cmd) {
	processgroup.StartCommandInNewGroup(cmd)
	cmd.Cancel = func() error {
		// The group remains ours even if its leader exits before cancellation.
		return processgroup.TerminateCommand(cmd, cmd.Process.Pid, 100*time.Millisecond, processgroup.Options{})
	}
}
