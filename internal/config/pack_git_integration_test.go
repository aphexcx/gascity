//go:build integration && darwin

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A broken PATH Git must not prevent verified cached imports from loading.
// The real CLT fallback must still reject mismatched commits and dirty caches.
func TestLockedCacheWithXcodeLicenseFailure(t *testing.T) {
	repo := initBareRepo(t, "license-test")
	cache := filepath.Join(t.TempDir(), "cache")
	mustGit(t, "", "clone", repo, cache)
	head, err := defaultRunRepoCacheGit(cache, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho 'You have not agreed to the Xcode license agreements.' >&2\nexit 69\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if err := validateLockedRemoteCache("test", cache, head); err != nil {
		t.Fatalf("verified cache should load via fallback: %v", err)
	}
	if err := validateLockedRemoteCache("test", cache, strings.Repeat("0", 40)); err == nil || !strings.Contains(err.Error(), "expected") {
		t.Fatalf("wrong commit must fail: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cache, "untracked"), []byte("change"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateLockedRemoteCache("test", cache, head); err == nil || !strings.Contains(err.Error(), "local worktree changes") {
		t.Fatalf("dirty cache must fail: %v", err)
	}
}
