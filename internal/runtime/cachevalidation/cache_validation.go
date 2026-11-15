// Package cachevalidation guards cached code at runtime launch boundaries.
package cachevalidation

import (
	"context"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/cacheexec"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ValidateLaunchForCity binds the provider's construction city to launch text.
// Standalone providers may instead receive a binding from their operation caller.
func ValidateLaunchForCity(ctx context.Context, cfg runtime.Config, city string) error {
	if city != "" {
		return ValidateLaunch(cacheexec.WithCity(ctx, city), cfg)
	}
	if cacheexec.HasScope(ctx) {
		return ValidateLaunch(ctx, cfg)
	}
	return ValidateLaunchWithoutCity(ctx, cfg)
}

// ValidateLaunchWithoutCity validates a standalone provider constructed without
// loaded configuration. Cached file ownership checks still apply.
func ValidateLaunchWithoutCity(ctx context.Context, cfg runtime.Config) error {
	return ValidateLaunch(cacheexec.WithoutCity(ctx, "standalone runtime provider constructed without a city"), cfg)
}

// ValidateLaunch checks code launched from a cached import before handing
// the command to a session carrier. Controller loads already supply a FULL
// verdict. Executable configuration and staged inputs conservatively retain
// FULL validation when composition has discarded their precise source fields.
func ValidateLaunch(ctx context.Context, cfg runtime.Config) error {
	env := os.Environ()
	for key, value := range cfg.Env {
		env = append(env, key+"="+value)
	}
	commands := append([]string{cfg.Command}, cfg.PreStart...)
	commands = append(commands, cfg.SessionSetup...)
	commands = append(commands, cfg.SessionLive...)
	for _, command := range commands {
		if err := cacheexec.ValidateConfiguredShellCommand(ctx, command, cfg.WorkDir, env); err != nil {
			return err
		}
	}
	// Staging followed by execution loses the original cached paths. Until
	// launch instructions carry per-file provenance, conservatively validate
	// these inputs at launch, never in the read/copy-only staging helpers.
	sources := append([]string{cfg.OverlayDir, cfg.SessionSetupScript}, cfg.PackOverlayDirs...)
	for _, entry := range cfg.CopyFiles {
		sources = append(sources, entry.Src)
	}
	for _, source := range sources {
		if source == "" {
			continue
		}
		if !filepath.IsAbs(source) {
			source = filepath.Join(cfg.WorkDir, source)
		}
		if err := cacheexec.ValidateCachedExecutable(ctx, source); err != nil {
			return err
		}
	}
	return nil
}
