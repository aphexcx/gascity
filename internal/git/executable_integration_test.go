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
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
)

// Cancellation must stop a wrapper's descendants, including ones that still
// own the output pipes; returning after WaitDelay alone leaves them running.
func TestExecutableProbeCancelsDescendants(t *testing.T) {
	dir := t.TempDir()
	path, pid, heartbeat := filepath.Join(dir, "git"), filepath.Join(dir, "child.pid"), filepath.Join(dir, "heartbeat")
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\n{ while :; do echo . >> %q; %q 0.05; done; } &\necho $! > %q\nwait\n", heartbeat, sleep, pid)
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
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("probe error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not return after cancellation")
	}
	info, err := os.Stat(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	processgrouptest.AssertFileSizeStable(t, heartbeat, info.Size(), 300*time.Millisecond)
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
