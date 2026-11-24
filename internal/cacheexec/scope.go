package cacheexec

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"sync"
)

// Active dependencies are separate from executable ownership: a removed import
// still owns its old executable paths, but cannot block unrelated local code.
var activeScopes sync.Map // canonical city directory -> []resolvedRemoteCache

type (
	cityContextKey   struct{}
	noCityContextKey struct{}
)

// WithCity binds configuration validation to the explicitly selected city.
// It preserves cancellation and does not change a child command's environment.
// An empty city retains an existing binding; it never infers one from a child.
func WithCity(ctx context.Context, city string) context.Context {
	if city == "" {
		return ctx
	}
	return context.WithValue(ctx, cityContextKey{}, canonicalPath(city))
}

// WithoutCity explicitly identifies execution with no loaded configuration.
// The reason is reported when a conservative validation fallback is needed.
func WithoutCity(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, noCityContextKey{}, reason)
}

// HasScope reports whether the caller supplied a city or an explicit exemption.
func HasScope(ctx context.Context) bool {
	return ctx.Value(cityContextKey{}) != nil || ctx.Value(noCityContextKey{}) != nil
}

// WithCityOrWithoutCity binds a configured provider's city, preserving an
// operation's explicit scope for standalone providers. noCityReason documents
// construction without a loaded city and is reported by validation.
func WithCityOrWithoutCity(ctx context.Context, city, noCityReason string) context.Context {
	if city != "" {
		return WithCity(ctx, city)
	}
	if HasScope(ctx) {
		return ctx
	}
	return WithoutCity(ctx, noCityReason)
}

// PublishScope replaces a city's active dependencies after a successful load.
// Failed loads must not publish partial dependencies. Callbacks are snapshotted
// so a later ownership registration cannot mutate an already loaded scope.
func PublishScope(city string, resolvedPaths []string) {
	owners := make(map[string]resolvedRemoteCache)
	for _, resolvedPath := range resolvedPaths {
		original := filepath.Clean(resolvedPath)
		if abs, err := filepath.Abs(original); err == nil {
			original = abs
		}
		// A changed cached file may point to local configuration. Preserve its
		// owning checkout as well as any owner of the canonical target.
		for _, path := range []string{original, canonicalPath(resolvedPath)} {
			var checkout string
			var owner resolvedRemoteCache
			resolvedRemoteCaches.Range(func(key, value any) bool {
				dir := key.(string)
				if pathWithinDir(path, dir) && len(dir) > len(checkout) {
					checkout, owner = dir, value.(resolvedRemoteCache)
				}
				return true
			})
			if checkout != "" {
				owners[canonicalPath(checkout)] = owner
			}
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

func validateActiveRemoteCaches(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if city, ok := ctx.Value(cityContextKey{}).(string); ok {
		if owners, ok := activeScopes.Load(city); ok {
			return validateScope(ctx, owners.([]resolvedRemoteCache))
		}
		return nil
	}
	if reason, ok := ctx.Value(noCityContextKey{}).(string); ok && reason != "" {
		log.Printf("cache validation without a city: %s", reason)
		return nil
	}
	return fmt.Errorf("configured execution has no explicit city scope")
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
