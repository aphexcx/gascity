package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

const commandLineToolsGit = "/Library/Developer/CommandLineTools/usr/bin/git"

// Executable describes a verified Git binary and any degraded host state.
type Executable struct {
	Path    string
	Version string
	Warning string
}

// ExecutableError reports a broken Git installation, before repository access.
type ExecutableError struct {
	message string
	cause   error
}

// Error returns an actionable, single-line installation diagnostic.
func (e *ExecutableError) Error() string { return e.message }

// Unwrap preserves the underlying executable failure.
func (e *ExecutableError) Unwrap() error { return e.cause }

// Cache per PATH-selected executable: normal processes resolve once, while a
// caller that deliberately changes PATH does not retain a stale executable.
var executableResolutions sync.Map

// ResolveExecutable verifies Git on PATH, falling back to Command Line Tools
// on macOS only when the selected Git reports an Xcode license failure. Each
// candidate's result and fallback warning are cached for the process.
func ResolveExecutable() (Executable, error) {
	path, err := exec.LookPath("git")
	if err != nil {
		return Executable{}, &ExecutableError{
			message: "git itself is unusable: " + oneLine(err.Error()) + "; install git and ensure it is on PATH",
			cause:   err,
		}
	}
	resolve := cachedExecutableResolution(runtime.GOOS, path, commandLineToolsGit, probeExecutable, func(warning string) {
		fmt.Fprintln(os.Stderr, "warning: "+warning) //nolint:errcheck // diagnostic only
	})
	actual, _ := executableResolutions.LoadOrStore(path, resolve)
	return actual.(func() (Executable, error))()
}

func cachedExecutableResolution(goos, primary, fallback string, probe func(string) (string, int, error), warn func(string)) func() (Executable, error) {
	return sync.OnceValues(func() (Executable, error) {
		result, err := resolveExecutable(goos, primary, fallback, probe)
		if result.Warning != "" {
			warn(result.Warning)
		}
		return result, err
	})
}

func resolveExecutable(goos, primary, fallback string, probe func(string) (string, int, error)) (Executable, error) {
	version, code, err := probe(primary)
	if err == nil {
		return Executable{Path: primary, Version: oneLine(version)}, nil
	}
	diagnostic := fmt.Sprintf("%s: %s", primary, probeFailure(version, err))
	licenseFailure := code == 69 || strings.Contains(strings.ToLower(version), "xcode license")
	if goos != "darwin" || !licenseFailure {
		return Executable{}, &ExecutableError{message: "git itself is unusable: " + diagnostic, cause: err}
	}
	const remedy = "a human can run sudo xcodebuild -license accept"
	fallbackVersion, _, fallbackErr := probe(fallback)
	if fallbackErr == nil {
		return Executable{
			Path:    fallback,
			Version: oneLine(fallbackVersion),
			Warning: fmt.Sprintf("git on PATH is unusable (%s); using Command Line Tools fallback %s; %s", diagnostic, fallback, remedy),
		}, nil
	}
	return Executable{}, &ExecutableError{
		message: fmt.Sprintf("git itself is unusable: %s; Command Line Tools fallback %s: %s; %s", diagnostic, fallback, probeFailure(fallbackVersion, fallbackErr), remedy),
		cause:   errors.Join(err, fallbackErr),
	}
}

func probeFailure(output string, err error) string {
	if output = oneLine(output); output != "" {
		return output + " (" + oneLine(err.Error()) + ")"
	}
	return oneLine(err.Error())
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func probeExecutable(path string) (string, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return probeExecutableContext(ctx, path)
}

func probeExecutableContext(ctx context.Context, path string) (string, int, error) {
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = HermeticEnv()
	configureExecutableProbe(cmd)
	cmd.WaitDelay = 100 * time.Millisecond
	out, err := cmd.CombinedOutput()
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return string(out), code, err
}
