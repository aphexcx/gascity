//go:build integration && !windows

package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
)

// Probes must stop descendants both on cancellation and when a wrapper exits
// while a child still owns the output pipes.
func TestExecutableProbeCleansDescendants(t *testing.T) {
	for _, tc := range []struct {
		name, finalCommand string
		cancel             bool
	}{
		{"cancellation", "wait", true},
		{"wrapper exits", "exit 0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path, pid, heartbeat := filepath.Join(dir, "git"), filepath.Join(dir, "child.pid"), filepath.Join(dir, "heartbeat")
			ready := filepath.Join(dir, "ready")
			if err := syscall.Mkfifo(ready, 0o600); err != nil {
				t.Fatal(err)
			}
			sleep, err := exec.LookPath("sleep")
			if err != nil {
				t.Fatal(err)
			}
			// The wrapper must not exit until its child has written a heartbeat.
			script := fmt.Sprintf("#!/bin/sh\n{ echo . >> %q; echo ready > %q; while :; do echo . >> %q; %q 0.05; done; } &\necho $! > %q\nread -r ready < %q\necho 'git version 2.49'\n%s\n", heartbeat, ready, heartbeat, sleep, pid, ready, tc.finalCommand)
			if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { processgrouptest.KillFromPIDFile(t, pid) })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, _, err := probeExecutableContext(ctx, path)
				done <- err
			}()
			processgrouptest.WaitForFileSize(t, heartbeat)
			wantErr := exec.ErrWaitDelay
			if tc.cancel {
				cancel()
				wantErr = context.Canceled
			}
			select {
			case err := <-done:
				if !errors.Is(err, wantErr) {
					t.Fatalf("probe error = %v, want %v", err, wantErr)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("probe did not return after wrapper exit or cancellation")
			}
			info, err := os.Stat(heartbeat)
			if err != nil {
				t.Fatal(err)
			}
			processgrouptest.AssertFileSizeStable(t, heartbeat, info.Size(), 300*time.Millisecond)
		})
	}
}

// Deadline diagnostics must name the probe and executable while preserving
// errors.Is so callers can still recognize a timeout.
func TestExecutableProbeDeadlineDiagnostic(t *testing.T) {
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	_, _, err := probeExecutableContext(ctx, "/probe/git")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "/probe/git") || !strings.Contains(err.Error(), "--version") {
		t.Fatalf("probe deadline diagnostic = %v", err)
	}
}

// A real exit-69 executable must be replaced by a verified fallback, and a
// missing fallback must produce the same actionable single-line diagnostic.
func TestExecutableLicenseProcess(t *testing.T) {
	dir := t.TempDir()
	primary, fallback := filepath.Join(dir, "git"), filepath.Join(dir, "clt-git")
	for path, script := range map[string]string{
		primary:  "#!/bin/sh\necho 'You have not agreed to the Xcode license agreements.' >&2\nexit 69\n",
		fallback: "#!/bin/sh\n[ \"$1\" = --version ] || exit 2\necho 'git version 2.49'\n",
	} {
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got, err := resolveExecutable("darwin", primary, fallback, probeExecutable)
	if err != nil || got.Path != fallback || !strings.Contains(got.Warning, "exit status 69") {
		t.Fatalf("fallback: %+v, %v", got, err)
	}
	if err := os.Remove(fallback); err != nil {
		t.Fatal(err)
	}
	_, err = resolveExecutable("darwin", primary, fallback, probeExecutable)
	if err == nil || !strings.Contains(err.Error(), "no such file") || strings.ContainsAny(err.Error(), "\r\n") {
		t.Fatalf("missing fallback: %v", err)
	}
}
