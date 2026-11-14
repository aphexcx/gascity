// Package cachevalidation guards cached code at runtime launch boundaries.
package cachevalidation

import (
	"context"
	"os"

	"github.com/gastownhall/gascity/internal/cacheexec"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ValidateLaunch checks code launched from a cached import before handing
// the command to a session carrier. Controller loads already supply a FULL
// verdict; a fresh CLI process upgrades only the imports it executes.
func ValidateLaunch(ctx context.Context, cfg runtime.Config) error {
	env := os.Environ()
	for key, value := range cfg.Env {
		env = append(env, key+"="+value)
	}
	return cacheexec.ValidateCachedShellCommand(ctx, cfg.Command, cfg.WorkDir, env)
}
