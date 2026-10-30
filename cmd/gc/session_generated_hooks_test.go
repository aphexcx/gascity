package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestReconcilerRebaselinesGeneratedHookAdditionOnce(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	tp := env.desiredState["worker"]
	tp.WorkDir = t.TempDir()
	tp.Hints.ProviderName = "codex"
	tp.Hints.PackOverlayDirs = []string{seedCodexOverlay(t)}
	tp.Hints.InstallAgentHooks = []string{"pi"}
	laterOverlay := t.TempDir()
	laterSource := filepath.Join(laterOverlay, "per-provider", "pi", ".pi", "extensions", "gc-hooks.js")
	if err := os.MkdirAll(filepath.Dir(laterSource), 0o700); err != nil {
		t.Fatal(err)
	}
	tp.Hints.PackOverlayDirs = append(tp.Hints.PackOverlayDirs, laterOverlay)
	before := templateParamsToConfig(tp)
	sess := env.createSessionBead("worker", "worker")
	env.markSessionActive(&sess)
	baseline, err := sessionHashRebaselineMetadata(before)
	if err != nil {
		t.Fatal(err)
	}
	env.setSessionMetadata(&sess, baseline)
	// A separate overlay changes after startup, before its first probe. Repair
	// of the delayed Codex hook must not turn this new source into baseline data.
	if err := os.WriteFile(laterSource, []byte("export default function gcHooks() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runtime.StageSessionWorkDirWithWarnings(before, io.Discard); err != nil {
		t.Fatal(err)
	}
	tp.Hints.CopyFiles = stageHookFiles(nil, tp.WorkDir, tp.WorkDir, []string{"codex"})
	// A simultaneous live edit must still run after the core-only repair.
	tp.Hints.SessionLive = []string{"echo updated live configuration"}
	env.desiredState["worker"] = tp
	env.reconcile([]beads.Bead{sess})
	if ds := env.dt.get(sess.ID); ds != nil {
		t.Fatalf("generated hook drained live session: %+v; %s", ds, env.stderr.String())
	}
	if count := strings.Count(env.stderr.String(), "rebaselined generated hooks for worker"); count != 1 {
		t.Fatalf("rebaseline count=%d; %s", count, env.stderr.String())
	}
	updated, err := env.store.Get(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Metadata["started_config_hash"] != runtime.CoreFingerprint(templateParamsToConfig(tp)) {
		t.Fatal("baseline was not persisted")
	}
	for _, key := range []string{"live_hash", "started_live_hash"} {
		if updated.Metadata[key] != baseline[key] {
			t.Fatalf("generated-hook repair overwrote unapplied %s", key)
		}
	}
	env.reconcile([]beads.Bead{updated})
	if count := strings.Count(env.stderr.String(), "rebaselined generated hooks for worker"); count != 1 {
		t.Fatalf("repeated rebaseline: %s", env.stderr.String())
	}
	liveRuns := 0
	for _, call := range env.sp.SnapshotCalls() {
		if call.Method == "RunLive" {
			liveRuns++
		}
	}
	if liveRuns != 1 {
		t.Fatalf("live update executions=%d, want 1", liveRuns)
	}
	updated, err = env.store.Get(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	tp.Hints.CopyFiles = stageHookFiles(nil, tp.WorkDir, tp.WorkDir, []string{"codex", "pi"})
	env.desiredState["worker"] = tp
	env.reconcile([]beads.Bead{updated})
	if ds := env.dt.get(sess.ID); ds == nil {
		t.Fatalf("repair replaced startup evidence for the later overlay: %s", env.stderr.String())
	}
}

func TestGeneratedHookAdditionDoesNotMaskOperatorDrift(t *testing.T) {
	before := runtime.Config{WorkDir: t.TempDir(), ProviderName: "codex", Command: "original", PackOverlayDirs: []string{seedCodexOverlay(t)}}
	stored := runtime.CoreFingerprint(before)
	breakdown, err := json.Marshal(runtime.CoreFingerprintBreakdown(before))
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.StageSessionWorkDirWithWarnings(before, io.Discard); err != nil {
		t.Fatal(err)
	}
	current := before
	current.CopyFiles = stageHookFiles(nil, before.WorkDir, before.WorkDir, []string{"codex"})
	if !generatedHookFilesAdded(stored, string(breakdown), current) {
		t.Fatal("fixture did not identify generated hook")
	}
	for _, kind := range []string{"command", "content", "source", "foreign", "missing breakdown", "missing source evidence", "removal", "changed overlay"} {
		t.Run(kind, func(t *testing.T) {
			changed := current
			changed.CopyFiles = append([]runtime.CopyEntry(nil), current.CopyFiles...)
			oldJSON, oldHash := string(breakdown), stored
			switch kind {
			case "command":
				changed.Command = "operator change"
			case "content":
				changed.CopyFiles[0].ContentHash = "operator change"
			case "source":
				changed.CopyFiles[0].Src = filepath.Join(t.TempDir(), ".codex", "hooks.json")
			case "foreign":
				changed.CopyFiles = append(changed.CopyFiles, runtime.CopyEntry{Src: "foreign", RelDst: "foreign"})
			case "missing breakdown":
				oldJSON = ""
			case "missing source evidence":
				var old runtime.BreakdownV1
				if err := json.Unmarshal(breakdown, &old); err != nil {
					t.Fatal(err)
				}
				old.OverlaySources = nil
				data, err := json.Marshal(old)
				if err != nil {
					t.Fatal(err)
				}
				oldJSON = string(data)
			case "changed overlay":
				source := filepath.Join(before.PackOverlayDirs[0], "per-provider", "codex", ".codex", "hooks.json")
				if err := os.WriteFile(source, []byte(`{"hooks":{}}`), 0o600); err != nil {
					t.Fatal(err)
				}
				changed.CopyFiles[0].ContentHash = runtime.HashHookSettingsContent(source, ".codex/hooks.json")
			case "removal":
				withOld := before
				withOld.CopyFiles = []runtime.CopyEntry{{Src: "old", RelDst: "old"}}
				oldHash = runtime.CoreFingerprint(withOld)
				data, err := json.Marshal(runtime.CoreFingerprintBreakdown(withOld))
				if err != nil {
					t.Fatal(err)
				}
				oldJSON = string(data)
			}
			if generatedHookFilesAdded(oldHash, oldJSON, changed) {
				t.Fatal("operator drift was exempted")
			}
		})
	}
}

func TestReconcilerDrainsNewOverlayHookSource(t *testing.T) {
	for _, kind := range []string{"directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			env := newReconcilerTestEnv()
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
			env.addDesired("worker", "worker", true)
			tp := env.desiredState["worker"]
			tp.WorkDir = t.TempDir()
			tp.Hints.ProviderName = "pi"
			overlayDir := t.TempDir()
			if kind == "symlink" {
				if err := os.MkdirAll(filepath.Join(overlayDir, "per-provider"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), filepath.Join(overlayDir, "per-provider", "pi")); err != nil {
					t.Fatal(err)
				}
			}
			source := filepath.Join(overlayDir, "per-provider", "pi", ".pi", "extensions", "gc-hooks.js")
			if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
				t.Fatal(err)
			}
			tp.Hints.PackOverlayDirs = []string{overlayDir}
			before := templateParamsToConfig(tp)
			sess := env.createSessionBead("worker", "worker")
			env.markSessionActive(&sess)
			baseline, err := sessionHashRebaselineMetadata(before)
			if err != nil {
				t.Fatal(err)
			}
			env.setSessionMetadata(&sess, baseline)
			// This extension did not exist when the running process loaded its hooks.
			if err := os.WriteFile(source, []byte("export default function gcHooks() {}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := runtime.StageSessionWorkDirWithWarnings(before, io.Discard); err != nil {
				t.Fatal(err)
			}
			tp.Hints.CopyFiles = stageHookFiles(nil, tp.WorkDir, tp.WorkDir, []string{"pi"})
			env.desiredState["worker"] = tp
			env.reconcile([]beads.Bead{sess})
			if ds := env.dt.get(sess.ID); ds == nil {
				t.Fatalf("new startup-loaded extension did not drain: %s", env.stderr.String())
			}
			if strings.Contains(env.stderr.String(), "rebaselined generated hooks") || !strings.Contains(env.stderr.String(), "config-drift") {
				t.Fatalf("new overlay source was silently accepted: %s", env.stderr.String())
			}
		})
	}
}
