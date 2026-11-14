package cachevalidation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/cacheexec"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestLaunchValidatesCachedStagingSourcesButCopyStaysQuick(t *testing.T) {
	for _, source := range []string{"pack-overlay", "agent-overlay", "copy-file"} {
		t.Run(source, func(t *testing.T) {
			cacheexec.Reset()
			t.Cleanup(cacheexec.Reset)
			cached, work := t.TempDir(), t.TempDir()
			script := filepath.Join(cached, "run.sh")
			if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			scans := 0
			dirty := errors.New("dirty cached staging input")
			cacheexec.Register(cached, func(context.Context) error { scans++; return dirty })
			cfg := runtime.Config{Command: "sh ./run.sh", WorkDir: work}
			switch source {
			case "pack-overlay":
				cfg.PackOverlayDirs = []string{cached}
			case "agent-overlay":
				cfg.OverlayDir = cached
			case "copy-file":
				cfg.CopyFiles = []runtime.CopyEntry{{Src: script, RelDst: "run.sh"}}
			}
			if err := runtime.StageSessionWorkDir(cfg); err != nil {
				t.Fatal(err)
			}
			if scans != 0 {
				t.Fatalf("copy-only materialization scanned %d imports", scans)
			}
			if err := ValidateLaunch(t.Context(), cfg); !errors.Is(err, dirty) {
				t.Fatalf("staged execution bypassed source validation: %v", err)
			}
		})
	}
}
