package config

import "github.com/gastownhall/gascity/internal/cacheexec"

func registerRemoteCacheRoots() {
	cacheexec.RegisterRoots(repoCacheRootCandidates())
}

func rememberRemoteCache(source, root, dir, commit string) {
	registerRemoteCacheRoots()
	cacheexec.Register(dir, func() error {
		return validateInstalledRemoteCacheLocked(source, root, dir, commit, false, CacheValidationFull)
	})
}
