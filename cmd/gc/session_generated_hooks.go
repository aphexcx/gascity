package main

import (
	"encoding/json"
	"fmt"
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
		if !generatedHookMatchesOverlay(entry, current, stored.OverlaySources) {
			return false
		}
		added = true
	}
	return added && runtime.CoreFingerprint(before) == storedHash
}

// preserveGeneratedHookSourceBaseline keeps the actual startup evidence across
// successive repairs. Another source may have changed before its first probe.
func preserveGeneratedHookSourceBaseline(patch map[string]string, storedJSON string) error {
	var stored, updated runtime.BreakdownV1
	if err := json.Unmarshal([]byte(storedJSON), &stored); err != nil {
		return fmt.Errorf("reading startup overlay sources: %w", err)
	}
	if err := json.Unmarshal([]byte(patch["core_hash_breakdown"]), &updated); err != nil {
		return fmt.Errorf("reading repaired hash breakdown: %w", err)
	}
	updated.OverlaySources = stored.OverlaySources
	data, err := json.Marshal(updated)
	if err != nil {
		return fmt.Errorf("preserving startup overlay sources: %w", err)
	}
	patch["core_hash_breakdown"] = string(data)
	return nil
}

func generatedHookMatchesOverlay(entry runtime.CopyEntry, cfg runtime.Config, baseline map[string]string) bool {
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
	var source, sourceDir string
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		for _, provider := range runtime.EffectiveOverlayProviderNames(cfg) {
			providerDir := filepath.Join(dir, "per-provider", provider)
			candidate := filepath.Join(providerDir, rel)
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
				source = candidate
				sourceDir = providerDir
			}
		}
	}
	// A match against today's overlay alone cannot establish what the running
	// process loaded. Old snapshots without source evidence fail closed as drift.
	return source != "" && baseline[sourceDir] != "" &&
		baseline[sourceDir] == runtime.OverlaySourceHashes(cfg)[sourceDir] &&
		runtime.HashHookSettingsContent(source, rel) == entry.ContentHash
}
