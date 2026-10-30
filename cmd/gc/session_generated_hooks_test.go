package main

import (
	"encoding/json"
	"io"
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
	before := templateParamsToConfig(tp)
	sess := env.createSessionBead("worker", "worker")
	env.markSessionActive(&sess)
	baseline, err := sessionHashRebaselineMetadata(before)
	if err != nil {
		t.Fatal(err)
	}
	env.setSessionMetadata(&sess, baseline)
	if err := runtime.StageSessionWorkDirWithWarnings(before, io.Discard); err != nil {
		t.Fatal(err)
	}
	tp.Hints.CopyFiles = stageHookFiles(nil, tp.WorkDir, tp.WorkDir, []string{"codex"})
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
	env.reconcile([]beads.Bead{updated})
	if count := strings.Count(env.stderr.String(), "rebaselined generated hooks for worker"); count != 1 {
		t.Fatalf("repeated rebaseline: %s", env.stderr.String())
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
	for _, kind := range []string{"command", "content", "source", "foreign", "missing breakdown", "removal"} {
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
