package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestPackCacheFullLoadsReuseVerdictUntilReloadEvent(t *testing.T) {
	for _, reload := range []bool{false, true} {
		t.Run(map[bool]string{false: "reads", true: "reload"}[reload], func(t *testing.T) {
			f := newPackCacheFixture(t, 3)
			var counts []int
			previous := 0
			for i := range 3 {
				if reload && i == 1 {
					config.InvalidateRemoteCacheValidations()
				}
				if _, _, err := config.LoadWithIncludesOptions(fsys.OSFS{}, filepath.Join(f.city, "city.toml"), config.LoadOptions{SkipRevisionSnapshot: true}); err != nil {
					t.Fatal(err)
				}
				total := strings.Count(f.gitCalls(t), "status --porcelain")
				counts = append(counts, total-previous)
				previous = total
			}
			want := []int{3, 0, 0}
			if reload {
				want[1] = 3
			}
			t.Logf("FULL scans per load: %v", counts)
			if !reflect.DeepEqual(counts, want) {
				t.Fatalf("FULL scans = %v; want %v", counts, want)
			}
		})
	}
}

func TestPackCacheReloadEventRejectsNestedEdit(t *testing.T) {
	f := newPackCacheFixture(t, 1)
	if _, _, err := config.LoadWithIncludesOptions(fsys.OSFS{}, filepath.Join(f.city, "city.toml"), config.LoadOptions{SkipRevisionSnapshot: true}); err != nil {
		t.Fatal(err)
	}
	// Keep the root's stat fingerprint unchanged, as a nested tracked edit does.
	info, err := os.Stat(f.caches[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f.caches[0], info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	config.InvalidateRemoteCacheValidations()
	if _, _, err := config.LoadWithIncludesOptions(fsys.OSFS{}, filepath.Join(f.city, "city.toml"), config.LoadOptions{SkipRevisionSnapshot: true}); err == nil || !strings.Contains(err.Error(), `has local worktree changes; run "gc import install"`) {
		t.Fatalf("reload accepted dirty cache: %v", err)
	}
}
