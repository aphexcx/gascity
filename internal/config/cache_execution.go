package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/cacheexec"
)

var runRepoCacheGitContext = defaultRunRepoCacheGitContext

func registerRemoteCacheRoots() {
	cacheexec.RegisterRoots(repoCacheRootCandidates())
}

func rememberRemoteCache(source, root, dir, commit string) {
	registerRemoteCacheRoots()
	cacheexec.Register(dir, func(ctx context.Context) error {
		return validateRemoteCacheExecution(ctx, source, root, dir, commit)
	})
}

func validateRemoteCacheExecution(ctx context.Context, source, root, dir, commit string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := dir + "\x00" + commit
	if v, ok := remoteCacheValidationCache.Load(key); ok {
		if v.(remoteCacheValidationEntry).fingerprint == remoteCacheFingerprint(dir) {
			return nil
		}
	}
	return WithRepoCacheReadLockContext(ctx, root, func() error {
		// Execution never repairs a missing checkout: discovery must have
		// resolved it first. Keep cancellation through both Git subprocesses.
		runGit := func(dir string, args ...string) (string, error) {
			return runRepoCacheGitContext(ctx, dir, args...)
		}
		if err := validateInstalledRemoteCacheWithGit(source, dir, commit, runGit, CacheValidationFull); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		remoteCacheValidationCache.Store(key, remoteCacheValidationEntry{fingerprint: remoteCacheFingerprint(dir)})
		return nil
	})
}

// RegisterHistoricalRemoteCache retains execution validation for a workflow's
// original import after its pin leaves the current configuration. The caller
// must first verify persisted formula provenance and canonical cache containment.
// The cache address independently binds the recovered source and commit; a
// changed HEAD cannot authorize itself merely by being read from this checkout.
func RegisterHistoricalRemoteCache(dir string) {
	cacheexec.Register(dir, func(ctx context.Context) error {
		root := filepath.Dir(dir)
		var source, commit string
		synthetic := false
		if err := WithRepoCacheReadLockContext(ctx, root, func() error {
			var err error
			if _, gitErr := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(gitErr) {
				synthetic, err = builtinpacks.ValidateHistoricalSyntheticRepo(dir)
				if synthetic || err != nil {
					return err
				}
			}
			commit, err = runRepoCacheGitContext(ctx, dir, "rev-parse", "HEAD")
			if err != nil {
				return fmt.Errorf("reading historical cache HEAD: %w", err)
			}
			source, err = runRepoCacheGitContext(ctx, dir, "config", "--get", "remote.origin.url")
			if err != nil {
				return fmt.Errorf("reading historical cache source: %w", err)
			}
			commit, source = strings.TrimSpace(commit), strings.TrimSpace(source)
			if RepoCacheKey(source, commit) != filepath.Base(dir) {
				return fmt.Errorf("historical cache %q does not match its source and commit address", dir)
			}
			return nil
		}); err != nil {
			return err
		}
		if synthetic {
			return ctx.Err()
		}
		return validateRemoteCacheExecution(ctx, source, root, dir, commit)
	})
}

func publishRemoteCacheScope(city string, resolvedPaths []string) {
	cacheexec.PublishScope(city, resolvedPaths)
}
