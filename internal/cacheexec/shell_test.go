package cacheexec

import (
	"context"
	"errors"
	"fmt"
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
		{"nohup-shell", `nohup sh -c '"$SCRIPT"'`, 1},
		{"nice-shell", `nice -n 5 sh -c '"$SCRIPT"'`, 1},
		{"timeout-shell", `timeout 5s sh -c '"$SCRIPT"'`, 1},
		{"setsid-shell", `setsid sh -c '"$SCRIPT"'`, 1},
		{"shell-option-operand", `sh -o errexit -c '"$SCRIPT"'`, 1},
		{"read-unset-variable", "cat \"$UNSET\"", 0},
		{"substitution", "echo \"$(\"$SCRIPT\")\"", 1},
		{"conditional-read", "if test -f \"$SCRIPT\"; then cat \"$SCRIPT\"; fi", 0},
		{"process-substitution-read", "diff <(printf a) <(printf b)", 0},
		{"process-substitution-execute", "diff <(\"$SCRIPT\") <(printf b)", 1},
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

func TestShellUnrecognizedInterpreterChecksBareScript(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	cached := t.TempDir()
	if err := os.WriteFile(filepath.Join(cached, "run.py"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dirty := errors.New("dirty interpreter operand")
	Register(cached, func(context.Context) error { return dirty })
	if err := ValidateCachedShellCommand(t.Context(), "python3.14 run.py", cached, []string{}); !errors.Is(err, dirty) {
		t.Fatalf("bare cached script bypassed interpreter validation: %v", err)
	}
}

func TestShellGlobCannotHideCachedExecution(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	local, cached := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(cached, "run.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cached, filepath.Join(local, "pack-link")); err != nil {
		t.Fatal(err)
	}
	dirty := errors.New("dirty globbed cache")
	Register(cached, func(context.Context) error { return dirty })
	PublishScope(local, []string{cached})
	for _, command := range []string{"./pack-*/run.sh", "sh ./pack-*/run.sh"} {
		if err := ValidateCachedShellCommand(t.Context(), command, local, []string{}); !errors.Is(err, dirty) {
			t.Errorf("globbed execution %q bypassed validation: %v", command, err)
		}
	}
	if err := ValidateCachedShellCommand(t.Context(), "cat ./pack-*/run.sh", local, []string{}); err != nil {
		t.Fatalf("globbed read must remain QUICK: %v", err)
	}
}

func TestShellReaderAliasesKeepArgumentsAsData(t *testing.T) {
	for _, target := range []string{"gnucp", "coreutils", "busybox"} {
		t.Run(target, func(t *testing.T) {
			Reset()
			t.Cleanup(Reset)
			local, cached := t.TempDir(), t.TempDir()
			binary := filepath.Join(local, target)
			if err := os.WriteFile(binary, nil, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(binary, filepath.Join(local, "cp")); err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(cached, "run.sh")
			if err := os.WriteFile(script, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			Register(cached, func(context.Context) error {
				return errors.New("copying cached data must not request FULL validation")
			})
			env := []string{"PATH=" + local, "SCRIPT=" + script}
			if err := ValidateCachedShellCommand(t.Context(), `cp "$SCRIPT" copied.sh`, local, env); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestShellInterpreterSymlinkCannotHideCachedExecution(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is unavailable")
	}
	for _, name := range []string{"launcher", "cat"} {
		t.Run(name, func(t *testing.T) {
			Reset()
			t.Cleanup(Reset)
			local, cached := t.TempDir(), t.TempDir()
			launcher := filepath.Join(local, name)
			if err := os.Symlink(shell, launcher); err != nil {
				t.Fatal(err)
			}
			dirty := errors.New("dirty cached interpreter payload")
			Register(cached, func(context.Context) error { return dirty })
			env := []string{"PATH=" + local, "SCRIPT=" + filepath.Join(cached, "run.sh")}
			for _, command := range []string{name, launcher} {
				if err := ValidateCachedShellCommand(t.Context(), command+` -c '"$SCRIPT"'`, local, env); !errors.Is(err, dirty) {
					t.Errorf("shell launcher %q bypassed cached payload validation: %v", command, err)
				}
			}
			cmd := &exec.Cmd{Path: launcher, Args: []string{name, "-c", `"$SCRIPT"`}, Dir: local, Env: env}
			GuardCachedExecCommand(t.Context(), cmd)
			if !errors.Is(cmd.Err, dirty) {
				t.Errorf("direct launcher bypassed cached payload validation: %v", cmd.Err)
			}
			if err := ValidateCachedShellCommand(t.Context(), name+" run.sh", cached, env); !errors.Is(err, dirty) {
				t.Errorf("shell launcher bypassed relative cached script validation: %v", err)
			}
			cmd = &exec.Cmd{Path: launcher, Args: []string{name, "run.sh"}, Dir: cached, Env: env}
			GuardCachedExecCommand(t.Context(), cmd)
			if !errors.Is(cmd.Err, dirty) {
				t.Errorf("direct launcher bypassed relative cached script validation: %v", cmd.Err)
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

func TestConfiguredExecutionRetainsSymlinkSourceOwner(t *testing.T) {
	for _, cachedTarget := range []bool{false, true} {
		t.Run(fmt.Sprintf("cached-target=%t", cachedTarget), func(t *testing.T) {
			Reset()
			t.Cleanup(Reset)
			city, cached := t.TempDir(), t.TempDir()
			target := city
			if cachedTarget {
				target = t.TempDir()
				Register(target, func(context.Context) error { return nil })
			}
			local := filepath.Join(target, "local.toml")
			if err := os.WriteFile(local, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(cached, "fragment.toml")
			if err := os.Symlink(local, source); err != nil {
				t.Fatal(err)
			}
			scans := 0
			dirty := errors.New("dirty cached configuration symlink")
			Register(cached, func(context.Context) error { scans++; return dirty })
			PublishScope(city, []string{source})
			if scans != 0 {
				t.Fatal("publishing read-only configuration triggered validation")
			}
			if err := ValidateConfiguredShellCommand(t.Context(), "printf configured", city, []string{}); !errors.Is(err, dirty) {
				t.Fatalf("cached source owner was lost through its symlink: %v", err)
			}
			if scans != 1 {
				t.Fatalf("source validation calls = %d, want 1", scans)
			}
		})
	}
}

func TestConfiguredCommandExplicitCityOverridesAmbientScope(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	dirtyCity, cleanCity, cached := t.TempDir(), t.TempDir(), t.TempDir()
	dirty := errors.New("dirty selected city")
	Register(cached, func(context.Context) error { return dirty })
	PublishScope(dirtyCity, []string{cached})
	PublishScope(cleanCity, nil)
	for _, tc := range []struct {
		selected, ambient string
		want              error
	}{{dirtyCity, cleanCity, dirty}, {cleanCity, dirtyCity, nil}} {
		cmd := &exec.Cmd{Path: os.Args[0], Args: []string{os.Args[0]}}
		cmd.Dir = tc.ambient
		cmd.Env = []string{"GC_CITY_PATH=" + tc.ambient}
		GuardConfiguredExecCommandForCity(t.Context(), cmd, tc.selected)
		if !errors.Is(cmd.Err, tc.want) {
			t.Fatalf("selected city guard = %v, want %v", cmd.Err, tc.want)
		}
		ctx := WithCity(t.Context(), tc.selected)
		if err := ValidateConfiguredShellCommand(ctx, "printf configured", cmd.Dir, cmd.Env); !errors.Is(err, tc.want) {
			t.Fatalf("selected city payload = %v, want %v", err, tc.want)
		}
		if cmd.Dir != tc.ambient || len(cmd.Env) != 1 || cmd.Env[0] != "GC_CITY_PATH="+tc.ambient {
			t.Fatal("validation changed the child's execution environment")
		}
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

func TestShellFallbackDoesNotClaimUnrelatedExecution(t *testing.T) {
	for _, cities := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d-cities", cities), func(t *testing.T) {
			Reset()
			t.Cleanup(Reset)
			cached := t.TempDir()
			dirty := errors.New("unrelated dirty cache")
			Register(cached, func(context.Context) error { return dirty })
			for range cities {
				PublishScope(t.TempDir(), []string{cached})
			}
			dir := t.TempDir()
			if err := ValidateCachedShellCommand(t.Context(), "FLAG=1 /bin/true", dir, []string{}); err != nil {
				t.Fatalf("unrelated city blocked local execution: %v", err)
			}
			if err := ValidateCachedShellCommand(t.Context(), filepath.Join(cached, "run.sh"), dir, []string{}); !errors.Is(err, dirty) {
				t.Fatalf("explicit cached executable lost its owner: %v", err)
			}
		})
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
