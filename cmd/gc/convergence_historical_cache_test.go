package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestConditionHistoricalCacheAfterPinUpgrade(t *testing.T) {
	for _, change := range []string{"clean", "dirty", "changed-head"} {
		t.Run(change, func(t *testing.T) {
			config.ResetRemoteCacheValidationCache()
			t.Cleanup(config.ResetRemoteCacheValidationCache)
			gcHome := t.TempDir()
			t.Setenv("GC_HOME", gcHome)
			cacheRoot := filepath.Join(gcHome, "cache", "repos")
			if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			repo, _ := testutil.InitGitRepo(t)
			if err := os.WriteFile(filepath.Join(repo, "gate.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			const source = "https://example.com/previous-pack.git"
			testutil.RunGit(t, repo, "remote", "add", "origin", source)
			testutil.RunGit(t, repo, "add", ".")
			testutil.RunGit(t, repo, "commit", "-m", "gate")
			commit := testutil.RunGit(t, repo, "rev-parse", "HEAD")
			cache := filepath.Join(cacheRoot, config.RepoCacheKey(source, commit))
			if err := os.Rename(repo, cache); err != nil {
				t.Fatal(err)
			}
			if change != "clean" {
				if err := os.WriteFile(filepath.Join(cache, "gate.sh"), []byte("#!/bin/sh\nexit 2\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				if change == "changed-head" {
					testutil.RunGit(t, cache, "add", ".")
					testutil.RunGit(t, cache, "commit", "-m", "changed gate")
				}
			}
			city := t.TempDir()
			cityConfig := filepath.Join(city, "city.toml")
			if err := os.WriteFile(cityConfig, []byte("[workspace]\nname = \"upgraded\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			// The new city lock no longer references the persisted gate's import.
			if _, _, err := config.LoadWithIncludes(fsys.OSFS{}, cityConfig); err != nil {
				t.Fatal(err)
			}
			store := beads.NewMemStore()
			bead, err := store.Create(beads.Bead{Type: "convergence", Title: "persisted gate", Metadata: map[string]string{
				convergence.FieldGateCondition: filepath.Join(cache, "gate.sh"),
			}})
			if err != nil {
				t.Fatal(err)
			}
			adapter := newConvergenceStoreAdapter(store, nil, false)
			meta, err := adapter.GetMetadata(bead.ID)
			if err != nil {
				t.Fatal(err)
			}
			result := convergence.RunCondition(t.Context(), meta[convergence.FieldGateCondition], convergence.ConditionEnv{CityPath: city}, time.Second, 0)
			switch change {
			case "clean":
				if result.Outcome != convergence.GatePass {
					t.Fatalf("clean historical gate refused: %s: %s", result.Outcome, result.Stderr)
				}
			case "dirty":
				if result.Outcome != convergence.GateError || !strings.Contains(result.Stderr, "has local worktree changes") {
					t.Fatalf("dirty historical gate result: %s: %s", result.Outcome, result.Stderr)
				}
			case "changed-head":
				if result.Outcome != convergence.GateError || !strings.Contains(result.Stderr, "does not match its source and commit address") {
					t.Fatalf("changed historical address result: %s: %s", result.Outcome, result.Stderr)
				}
			}
		})
	}
}
