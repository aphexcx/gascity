package config

import (
	"context"

	"github.com/gastownhall/gascity/internal/cacheexec"
)

var runRepoCacheGitContext = defaultRunRepoCacheGitContext

func registerRemoteCacheRoots() {
	cacheexec.RegisterRoots(repoCacheRootCandidates())
}

func rememberRemoteCache(source, root, dir, commit string) {
	registerRemoteCacheRoots()
	cacheexec.Register(dir, func(ctx context.Context) error {
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
	})
}

func publishRemoteCacheScope(city string, packDirs []string) {
	cacheexec.PublishScope(city, packDirs)
}
