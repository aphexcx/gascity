package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

func TestPackCacheReceiptServesFreshProcessFullLoad(t *testing.T) {
	f := newPackCacheFixture(t, 2)
	if _, _, err := loadStartCityConfig(f.city); err != nil {
		t.Fatal(err)
	}
	if scans := strings.Count(f.gitCalls(t), "status --porcelain"); scans != 2 {
		t.Fatalf("cold load scans=%d, want 2", scans)
	}
	config.ResetRemoteCacheValidationCache()
	if _, _, err := loadStartCityConfig(f.city); err != nil {
		t.Fatal(err)
	}
	if scans := strings.Count(f.gitCalls(t), "status --porcelain"); scans != 2 {
		t.Fatalf("fresh-process load repeated scans: %d", scans)
	}
	root := &cobra.Command{Use: "gc"}
	var stderr bytes.Buffer
	registerPackCommands(root, nil, io.Discard, &stderr)
	root.SetArgs([]string{"sample0", "report"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v; %s", err, stderr.String())
	}
	if scans := strings.Count(f.gitCalls(t), "status --porcelain"); scans != 2 {
		t.Fatalf("execution repeated full scan: %d", scans)
	}
	if _, err := os.Stat(f.marker); err != nil {
		t.Fatal(err)
	}
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
	if _, _, err := loadStartCityConfig(f.city); err == nil || !strings.Contains(err.Error(), "local worktree changes") {
		t.Fatalf("reload accepted nested edit: %v", err)
	}
}
