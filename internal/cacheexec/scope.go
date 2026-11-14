package cacheexec

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Active dependencies are separate from executable ownership: a removed import
// still owns its old executable paths, but cannot block unrelated local code.
var activeScopes sync.Map // canonical city directory -> []resolvedRemoteCache

// PublishScope replaces a city's active dependencies after a successful load.
// Failed loads must not publish partial dependencies. Callbacks are snapshotted
// so a later ownership registration cannot mutate an already loaded scope.
func PublishScope(city string, packDirs []string) {
	owners := make(map[string]resolvedRemoteCache)
	for _, packDir := range packDirs {
		packDir = canonicalPath(packDir)
		var checkout string
		var owner resolvedRemoteCache
		resolvedRemoteCaches.Range(func(key, value any) bool {
			dir := key.(string)
			if pathWithinDir(packDir, dir) && len(dir) > len(checkout) {
				checkout, owner = dir, value.(resolvedRemoteCache)
			}
			return true
		})
		if checkout != "" {
			owners[checkout] = owner
		}
	}
	keys := make([]string, 0, len(owners))
	for key := range owners {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	active := make([]resolvedRemoteCache, 0, len(keys))
	for _, key := range keys {
		active = append(active, owners[key])
	}
	activeScopes.Store(canonicalPath(city), active)
}

func canonicalPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if canonical, err := filepath.EvalSymlinks(path); err == nil {
		path = canonical
	}
	return filepath.Clean(path)
}

func validateActiveRemoteCaches(ctx context.Context, dir string, env []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if env == nil {
		env = os.Environ()
	}
	// Execution environments identify the city even when a rig or session's
	// working directory lives elsewhere. Prefer the explicit path variable.
	for _, name := range []string{"GC_CITY_PATH", "GC_CITY"} {
		for i := len(env) - 1; i >= 0; i-- {
			if city, ok := strings.CutPrefix(env[i], name+"="); ok && city != "" {
				if owners, ok := activeScopes.Load(canonicalPath(city)); ok {
					return validateScope(ctx, owners.([]resolvedRemoteCache))
				}
				break
			}
		}
	}
	dir = canonicalPath(dir)
	var nearest string
	var owners []resolvedRemoteCache
	activeScopes.Range(func(key, value any) bool {
		city := key.(string)
		if pathWithinDir(dir, city) && len(city) > len(nearest) {
			nearest, owners = city, value.([]resolvedRemoteCache)
		}
		return true
	})
	// Without an associated loaded city, there are no active configuration
	// dependencies. Other cities cannot claim this execution merely because
	// they were loaded in the same process. Explicit cached paths still use
	// the independent ownership registry before this fallback is reached.
	return validateScope(ctx, owners)
}

func validateScope(ctx context.Context, owners []resolvedRemoteCache) error {
	for _, owner := range owners {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := owner.validate(ctx); err != nil {
			return err
		}
	}
	return nil
}
