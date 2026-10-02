//go:build windows

package git

import "os/exec"

func configureExecutableProbe(_ *exec.Cmd) {}
