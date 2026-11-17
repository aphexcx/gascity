//go:build windows

package git

import "os/exec"

// ConfigureCommandCancellation retains exec.CommandContext cancellation on
// Windows, where Git probes do not use Unix process groups.
func ConfigureCommandCancellation(_ *exec.Cmd) func() error { return func() error { return nil } }
