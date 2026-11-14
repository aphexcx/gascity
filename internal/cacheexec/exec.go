// Package cacheexec guards execution of code from resolved remote pack caches.
// The configuration loader registers each checkout's full-validation callback;
// process runners can then check cached code without depending on config.
package cacheexec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Resolved imports are process-local inputs, not validation verdicts. A QUICK
// config load records the lock identity here so execution can upgrade just the
// checkout containing its executable, without loading or scanning other packs.
var (
	resolvedRemoteCaches sync.Map // absolute checkout path -> resolvedRemoteCache
	cacheRoots           sync.Map // absolute cache root -> struct{}
)

type resolvedRemoteCache struct {
	validate func(context.Context) error
}

// Register records a resolved checkout and the check required before execution.
func Register(dir string, validate func(context.Context) error) {
	original := filepath.Clean(dir)
	if canonical, err := filepath.EvalSymlinks(dir); err == nil {
		dir = canonical
	}
	owner := resolvedRemoteCache{validate: validate}
	resolvedRemoteCaches.Store(original, owner)
	resolvedRemoteCaches.Store(filepath.Clean(dir), owner)
}

// RegisterRoots identifies cache roots so unresolved cached code fails closed.
func RegisterRoots(roots []string) {
	for _, root := range roots {
		cacheRoots.Store(root, struct{}{})
	}
}

// Reset clears process-local ownership, for isolated configuration environments.
func Reset() {
	resolvedRemoteCaches.Clear()
	cacheRoots.Clear()
	activeScopes.Clear()
}

func pathWithinDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ValidateCachedExecutable fully validates the remote import containing path
// before executing it. Local executables keep their existing behavior. Cached
// executables must have been resolved from a locked import in this process;
// QUICK discovery supplies that identity but never supplies a FULL verdict.
func ValidateCachedExecutable(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !strings.ContainsAny(path, `/\`) {
		resolved, err := exec.LookPath(path)
		if err != nil {
			return nil
		} // preserve the executor's command-not-found error
		path = resolved
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolving cached executable %q: %w", path, err)
	}
	// Check the path before following its symlink as well: replacing a cached
	// script with a link to a local file must not bypass the owning import.
	if err := validateCachedExecutablePath(ctx, abs); err != nil {
		return err
	}
	if canonical, err := filepath.EvalSymlinks(abs); err == nil {
		if canonical != abs {
			return validateCachedExecutablePath(ctx, canonical)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("resolving cached executable %q: %w", path, err)
	}
	return nil
}

func validateCachedExecutablePath(ctx context.Context, abs string) error {
	var owner resolvedRemoteCache
	var checkout string
	resolvedRemoteCaches.Range(func(key, value any) bool {
		dir := key.(string)
		if pathWithinDir(abs, dir) && len(dir) > len(checkout) {
			checkout, owner = dir, value.(resolvedRemoteCache)
		}
		return true
	})
	if checkout == "" {
		var unresolved error
		cacheRoots.Range(func(key, _ any) bool {
			root := key.(string)
			if canonical, err := filepath.EvalSymlinks(root); err == nil && pathWithinDir(abs, canonical) || pathWithinDir(abs, root) {
				unresolved = fmt.Errorf("cached executable %q has not been resolved from a locked import", abs)
				return false
			}
			return true
		})
		return unresolved
	}
	return owner.validate(ctx)
}

// GuardConfiguredExecCommand validates configuration dependencies before running
// executable instructions whose per-field source was lost during composition.
// Read-only configuration loading never calls this execution boundary.
func GuardConfiguredExecCommand(ctx context.Context, cmd *exec.Cmd) {
	GuardConfiguredExecCommandForCity(ctx, cmd, "")
}

// GuardConfiguredExecCommandForCity binds executable configuration to its city
// even when the caller runs elsewhere. An empty city uses the command's scope.
// The validation scope does not change the child's environment or directory.
func GuardConfiguredExecCommandForCity(ctx context.Context, cmd *exec.Cmd, city string) {
	ctx = WithCity(ctx, city)
	if cmd.Err == nil {
		cmd.Err = validateActiveRemoteCaches(ctx, cmd.Dir, cmd.Environ())
	}
	GuardCachedExecCommand(ctx, cmd)
}

// GuardCachedExecCommand prevents cmd from starting when its cached code fails
// FULL validation. Call after setting Dir and Env, before Run, Output or Start.
// The refusal uses exec.Cmd.Err so existing execution error handling reports it.
func GuardCachedExecCommand(ctx context.Context, cmd *exec.Cmd) {
	if cmd.Err != nil {
		return
	}
	path := cmd.Path
	if cmd.Dir != "" && !filepath.IsAbs(path) {
		path = filepath.Join(cmd.Dir, path)
	}
	cmd.Err = ValidateCachedExecutable(ctx, path)
	if cmd.Err == nil {
		args := append([]string(nil), cmd.Args...)
		if len(args) > 0 {
			// exec.Command has already resolved Path. A different child PATH
			// must not cause us to validate an executable that will not run.
			args[0] = path
		}
		cmd.Err = validateCachedShellArgs(ctx, args, cmd.Dir, cmd.Environ(), 0)
	}
}
