package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/cacheexec"
	"github.com/gastownhall/gascity/internal/pathutil"
)

var runRepoCacheGitContext = defaultRunRepoCacheGitContext

// RegisterRemoteCacheRoots guards cached executables even without a config load.
// Registration performs no cache validation, Git invocation, or locking.
func RegisterRemoteCacheRoots() {
	cacheexec.RegisterRoots(repoCacheRootCandidates())
}

func rememberRemoteCache(source, root, dir, commit string) {
	RegisterRemoteCacheRoots()
	cacheexec.Register(dir, func(ctx context.Context) error {
		return validateRemoteCacheExecution(ctx, source, root, dir, commit)
	})
}

func validateRemoteCacheExecution(ctx context.Context, source, root, dir, commit string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := dir + "\x00" + commit
	mode := CacheValidationFull
	if _, ok := loadRemoteCacheValidation(dir, commit, remoteCacheFingerprint(dir), time.Now()); ok {
		mode = CacheValidationQuick
	}
	return WithRepoCacheReadLockContext(ctx, root, func() error {
		before, checkedAt := remoteCacheFingerprint(dir), time.Now()
		// Execution never repairs a missing checkout: discovery must have
		// resolved it first. Keep cancellation through both Git subprocesses.
		runGit := func(dir string, args ...string) (string, error) {
			return runRepoCacheGitContext(ctx, dir, args...)
		}
		if err := validateInstalledRemoteCacheWithGit(source, dir, commit, runGit, mode); err != nil {
			remoteCacheValidationCache.Delete(key)
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if mode == CacheValidationFull {
			rememberRemoteCacheValidation(key, dir, commit, before, checkedAt)
		}
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

// RegisterHistoricalRemoteCacheForPath retains the owner of a persisted,
// previously authorized executable after its import leaves the current lock.
// Only canonical cache entries qualify; execution still checks the source,
// commit address, and worktree through RegisterHistoricalRemoteCache.
func RegisterHistoricalRemoteCacheForPath(path string) {
	if !filepath.IsAbs(path) {
		return
	}
	path = pathutil.NormalizePathForCompare(path)
	for _, candidate := range repoCacheRootCandidates() {
		root := pathutil.NormalizePathForCompare(candidate)
		rel, err := filepath.Rel(root, path)
		if err != nil || pathutil.IsOutsideDir(rel) {
			continue
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) < 2 || len(parts[0]) != 64 || !validCacheObjectID(parts[0]) {
			continue
		}
		entry := filepath.Join(root, parts[0])
		if pathutil.PathWithin(entry, path) {
			RegisterHistoricalRemoteCache(entry)
		}
	}
}

func publishRemoteCacheScope(city string, resolvedPaths []string) {
	cacheexec.PublishScope(city, resolvedPaths)
}
