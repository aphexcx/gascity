// Package lanectx materializes worker workspace context and pre-start diagnostics.
package lanectx

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

const laneDiagnosticLimit = 1024 * 1024

// LaneEnvironmentPath locates the sourceable path-only environment in a lane.
func LaneEnvironmentPath(workDir string) string {
	return filepath.Join(workDir, ".gc", "worker-env.sh")
}

// LanePreStartLogPath locates the bounded, append-only pre-start diagnostic.
func LanePreStartLogPath(workDir string) string {
	return filepath.Join(workDir, ".gc", "pre-start.log")
}

// LaneEnvironmentScript exports only resolved workspace paths and rig identity.
// It lets a worker restore these values when its tool shell filters launch env.
func LaneEnvironmentScript(env map[string]string) string {
	var b strings.Builder
	for _, key := range []string{"GC_CITY_PATH", "GC_RIG_ROOT", "GC_RIG", "BEADS_DIR", "GC_DIR", "GC_PRESTART_LOG"} {
		b.WriteString("export " + key + "=" + shellquote.Quote(env[key]) + "\n")
	}
	return b.String()
}

// WriteLaneEnvironment atomically materializes the path-only environment after
// pre-start has prepared the lane. An absent GC_PRESTART_LOG disables artifacts.
func WriteLaneEnvironment(env map[string]string) error {
	if env["GC_PRESTART_LOG"] == "" {
		return nil
	}
	root, err := openLaneArtifactRoot(env, true)
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // directory handle
	tmp := ".gc/worker-env-" + rand.Text() + ".tmp"
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating lane environment: %w", err)
	}
	defer root.Remove(tmp) //nolint:errcheck // renamed on success
	_, writeErr := f.WriteString(LaneEnvironmentScript(env))
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return fmt.Errorf("writing lane environment: %w", err)
	}
	if err := root.Rename(tmp, ".gc/worker-env.sh"); err != nil {
		return fmt.Errorf("publishing lane environment: %w", err)
	}
	return nil
}

// PreStartDiagnostics buffers redacted command output until preparation ends.
// It never creates files while a later command may still need an empty lane.
type PreStartDiagnostics struct {
	output string
}

// Capture retains an 8 KiB output tail per command, bounded to 1 MiB in total.
func (d *PreStartDiagnostics) Capture(env map[string]string, output string, commandErr error) {
	if env["GC_PRESTART_LOG"] == "" || len(d.output) >= laneDiagnosticLimit {
		return
	}
	status := "completed"
	if commandErr != nil {
		status = commandErr.Error()
	}
	secrets := runtime.SetupCommandSecrets(env)
	status, _ = runtime.RedactSecretsTail(status, 1024, secrets)
	tail, truncated := runtime.RedactSecretsTail(output, 8*1024, secrets)
	entry := fmt.Sprintf("\n%s session=%s %s\n", time.Now().UTC().Format(time.RFC3339), env["GC_SESSION_ID"], status)
	if truncated {
		entry += "[output tail truncated]\n"
	}
	d.output += boundedDiagnostic(entry+tail+"\n", int64(laneDiagnosticLimit-len(d.output)))
}

// Flush appends captured diagnostics without replacing existing bytes. Create
// must be true only after all pre-start commands succeed. Failed preparation
// appends to an existing log, but leaves a fresh destination untouched so a
// retry can prepare it; the setup error already carries the failure's output.
func (d *PreStartDiagnostics) Flush(env map[string]string, create bool) (result error) {
	if env["GC_PRESTART_LOG"] == "" {
		return nil
	}
	root, err := openLaneArtifactRoot(env, create)
	if err != nil {
		if !create && os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer root.Close() //nolint:errcheck // directory handle
	flags := os.O_WRONLY | os.O_APPEND
	if create {
		flags |= os.O_CREATE
	}
	f, err := root.OpenFile(".gc/pre-start.log", flags, 0o600)
	if err != nil {
		if !create && os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("opening pre-start diagnostic: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			result = errors.Join(result, fmt.Errorf("closing pre-start diagnostic: %w", err))
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("reading pre-start diagnostic size: %w", err)
	}
	remaining := laneDiagnosticLimit - info.Size()
	if remaining <= 0 {
		return nil
	}
	if _, err := f.WriteString(boundedDiagnostic(d.output, remaining)); err != nil {
		return fmt.Errorf("appending pre-start diagnostic: %w", err)
	}
	return nil
}

func boundedDiagnostic(entry string, remaining int64) string {
	const marker = "\n[pre_start log capacity reached; no further output retained]\n"
	if int64(len(entry)+len(marker)) >= remaining {
		// Reserve the marker on the final append, without truncating old data.
		if remaining >= int64(len(marker)) {
			entry = entry[:min(len(entry), int(remaining)-len(marker))] + marker
		} else {
			entry = marker[:remaining]
		}
	}
	return entry
}

func openLaneArtifactRoot(env map[string]string, create bool) (*os.Root, error) {
	workDir := env["GC_DIR"]
	if !filepath.IsAbs(workDir) || env["GC_PRESTART_LOG"] != LanePreStartLogPath(workDir) {
		return nil, fmt.Errorf("lane artifacts require GC_DIR and its resolved GC_PRESTART_LOG")
	}
	physical, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolving lane directory: %w", err)
	}
	for _, scope := range []string{env["GC_CITY_PATH"], env["GC_RIG_ROOT"]} {
		if scope == "" {
			continue
		}
		resolved, err := filepath.EvalSymlinks(scope)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("resolving lane scope %q: %w", scope, err)
		}
		if physical == resolved || filepath.Clean(workDir) == filepath.Clean(scope) {
			return nil, fmt.Errorf("refusing lane artifacts in scope root %q", scope)
		}
	}
	root, err := os.OpenRoot(workDir)
	if err != nil {
		return nil, fmt.Errorf("opening lane directory: %w", err)
	}
	if create {
		if err := root.Mkdir(".gc", 0o700); err != nil && !os.IsExist(err) {
			_ = root.Close()
			return nil, fmt.Errorf("creating lane artifact directory: %w", err)
		}
	}
	return root, nil
}
