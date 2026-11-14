package cacheexec

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestShellLaunchChecksOnlyReferencedCode(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		wantScans     int
	}{
		{"relative-shell-script", "sh first/run.sh", 1},
		{"relative-python-script", "python3 first/run.sh", 1},
		{"combined-shell-flags", `sh -ec '"$SCRIPT"'`, 1},
		{"login-shell-flags", `bash -lc '"$SCRIPT"'`, 1},
		{"shell-option-operand", `sh -o errexit -c '"$SCRIPT"'`, 1},
		{"read-unset-variable", "cat \"$UNSET\"", 0},
		{"substitution", "echo \"$(\"$SCRIPT\")\"", 1},
		{"conditional-read", "if test -f \"$SCRIPT\"; then cat \"$SCRIPT\"; fi", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			Reset()
			t.Cleanup(Reset)
			root := t.TempDir()
			scans := 0
			for _, name := range []string{"first", "unrelated"} {
				dir := filepath.Join(root, name)
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				// Match the loader's process-local FULL verdict memo.
				checked := false
				Register(dir, func(context.Context) error {
					if !checked {
						scans++
						checked = true
					}
					return nil
				})
			}
			if err := ValidateCachedShellCommand(context.Background(), tc.command, root, []string{"PATH=/usr/bin:/bin", "SCRIPT=" + filepath.Join(root, "first", "run.sh")}); err != nil {
				t.Fatal(err)
			}
			if scans != tc.wantScans {
				t.Fatalf("scanned %d imports; want %d", scans, tc.wantScans)
			}
		})
	}
}

func TestGuardUsesResolvedExecutableInsteadOfChildPATH(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	local, cached := t.TempDir(), t.TempDir()
	for _, dir := range []string{local, cached} {
		if err := os.WriteFile(filepath.Join(dir, "echo"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	scans := 0
	Register(cached, func(context.Context) error { scans++; return nil })
	cmd := &exec.Cmd{
		Path: filepath.Join(local, "echo"),
		Args: []string{"echo", "message"},
		Env:  []string{"PATH=" + cached},
	}
	GuardCachedExecCommand(context.Background(), cmd)
	if cmd.Err != nil || scans != 0 {
		t.Fatalf("guard error = %v, scans = %d; child PATH must not replace the selected executable", cmd.Err, scans)
	}
}

func TestShellFallbackUsesExecutionCityOutsideItsDirectory(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	first, second, cached := t.TempDir(), t.TempDir(), t.TempDir()
	dirty := errors.New("dirty import in first city")
	Register(cached, func(context.Context) error { return dirty })
	PublishScope(first, []string{cached})
	PublishScope(second, nil)
	for _, tc := range []struct {
		city string
		want error
	}{{first, dirty}, {second, nil}} {
		err := ValidateCachedShellCommand(t.Context(), "FLAG=1 /bin/true", t.TempDir(), []string{"GC_CITY_PATH=" + tc.city})
		if !errors.Is(err, tc.want) {
			t.Fatalf("execution city %s: %v; want %v", tc.city, err, tc.want)
		}
	}
}

func TestShellStateChangesCannotHideCachedExecution(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		cachedInitial bool
	}{
		{"false-branch", `if false; then SCRIPT=/outside/decoy; fi; "$SCRIPT"`, true},
		{"subshell", `(SCRIPT=/outside/decoy); "$SCRIPT"`, true},
		{"inline-assignment", `SCRIPT=/outside/decoy "$SCRIPT"`, true},
		{"export", `export SCRIPT="$CACHED"; "$SCRIPT"`, false},
		{"eval-assignment", `eval 'SCRIPT="$CACHED"'; "$SCRIPT"`, false},
		{"loop-variable", `for SCRIPT in "$CACHED"; do "$SCRIPT"; done`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			Reset()
			t.Cleanup(Reset)
			cached := t.TempDir()
			dirty := errors.New("dirty cached import")
			Register(cached, func(context.Context) error { return dirty })
			city := t.TempDir()
			PublishScope(city, []string{cached})
			script := "/outside/decoy"
			if tc.cachedInitial {
				script = filepath.Join(cached, "run.sh")
			}
			err := ValidateCachedShellCommand(context.Background(), tc.command, city, []string{
				"PATH=/usr/bin:/bin", "SCRIPT=" + script, "CACHED=" + filepath.Join(cached, "run.sh"),
			})
			if !errors.Is(err, dirty) {
				t.Fatalf("cached execution escaped FULL validation: %v", err)
			}
		})
	}
}
