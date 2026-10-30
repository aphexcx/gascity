package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/runtime"
)

// generatedHookFilesAdded recognizes only an absent-to-present probe of an
// unchanged configured hook overlay. Reconstructing the old fingerprint keeps
// simultaneous operator changes, removals, and edits on the ordinary drift path.
func generatedHookFilesAdded(storedHash, storedJSON string, current runtime.Config) bool {
	var stored runtime.BreakdownV1
	if json.Unmarshal([]byte(storedJSON), &stored) != nil || stored.Version != runtime.FingerprintVersion {
		return false
	}
	previous := make(map[string]bool, len(stored.CopyFiles))
	for _, entry := range stored.CopyFiles {
		previous[entry.RelDst] = true
	}
	before := current
	before.CopyFiles = nil
	added := false
	for _, entry := range current.CopyFiles {
		if previous[entry.RelDst] {
			before.CopyFiles = append(before.CopyFiles, entry)
			continue
		}
		if !generatedHookMatchesOverlay(entry, current) {
			return false
		}
		added = true
	}
	return added && runtime.CoreFingerprint(before) == storedHash
}

func generatedHookMatchesOverlay(entry runtime.CopyEntry, cfg runtime.Config) bool {
	if !entry.Probed || entry.ContentHash == "" || cfg.WorkDir == "" {
		return false
	}
	var rel string
	for _, provider := range orderedWorkDirHookProviders {
		for _, candidate := range provider.relPaths {
			if filepath.Clean(entry.Src) == filepath.Join(cfg.WorkDir, candidate) {
				rel = candidate
			}
		}
	}
	if rel == "" {
		return false
	}
	dirs := append(append([]string(nil), cfg.PackOverlayDirs...), cfg.OverlayDir)
	var source string
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		for _, provider := range runtime.EffectiveOverlayProviderNames(cfg) {
			candidate := filepath.Join(dir, "per-provider", provider, rel)
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
				source = candidate
			}
		}
	}
	return source != "" && runtime.HashHookSettingsContent(source, rel) == entry.ContentHash
}
