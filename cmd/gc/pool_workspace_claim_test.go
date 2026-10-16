package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// A workspace prepared for one bead must not claim a sibling just because
// that sibling comes first in the pool-wide work query.
func TestHookClaimPreparedWorkspaceKeepsItsTrigger(t *testing.T) {
	for _, trigger := range []string{"work-2", "work-1"} {
		t.Run(trigger, func(t *testing.T) {
			spy := &stampMetaSpy{}
			ops := poolClaimOps(`[
				{"id":"work-1","status":"open","metadata":{"gc.routed_to":"worker"}},
				{"id":"work-2","status":"open","metadata":{"gc.routed_to":"worker"}}
			]`, map[string]string{}, trigger, spy)
			ops.WorkspaceBranch = func(string) (string, error) { return trigger, nil }
			ops.CurrentClaim = func(string) (string, error) { return "", nil }
			ops.DrainPending = func(string) (bool, error) { return false, nil }
			ops.EmitExecutionStepStarted = func(beads.Bead, string, []string, string) {}
			opts := poolClaimOpts()
			opts.Env = append(opts.Env, "GC_WORKTREE_CLAIM_GUARD=1", "GC_WORKTREE_BEAD_ID="+trigger)
			var stdout, stderr bytes.Buffer
			if code := doHookClaim("query", "/rig", opts, ops, &stdout, &stderr); code != 0 {
				t.Fatalf("claim: %d: %s", code, stderr.String())
			}
			var result hookClaimJSONResult
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.BeadID != trigger {
				t.Fatalf("workspace branch %s claimed %s", trigger, result.BeadID)
			}
		})
	}
}

func TestPoolWorkspaceLaunchAndClaimAgree(t *testing.T) {
	f := newDeferredSurvivorFixture(t)
	hookClaimInitRepo(t, f.path, "main")
	f.cfg.Agents[0].WorkDir = ".worktrees/lane-{{.AgentBase}}"
	f.cfg.Agents[0].PreStart = []string{"prepare-worktree"}
	f.work(f.store, "work-1", "helper", "", false)
	f.work(f.store, "work-2", "helper", "", false)
	f.tick()
	f.tick()
	starts := 0
	dirs := map[string]bool{}
	for _, call := range f.sp.SnapshotCalls() {
		if call.Method != "Start" {
			continue
		}
		starts++
		if dirs[call.Config.WorkDir] {
			t.Fatalf("shared workdir %s", call.Config.WorkDir)
		}
		dirs[call.Config.WorkDir] = true
		claimPreparedPoolStart(t, f, call)
	}
	if starts != 2 {
		t.Fatalf("starts=%d, want 2\n%s", starts, f.stderr.String())
	}
	for _, slot := range []string{"1", "2"} {
		if !dirs[filepath.Join(f.path, ".worktrees", "lane-helper.pool-"+slot)] {
			t.Errorf("missing isolated slot %s: %v", slot, dirs)
		}
	}
}

// Recheck the reported pinned-first-session / second-session-start-pending
// stall through the real desired-state builder and reconciler with Fake runtime.
func TestPoolWorkspacePinnedSiblingDoesNotStallSecondStart(t *testing.T) {
	f := newDeferredSurvivorFixture(t)
	hookClaimInitRepo(t, f.path, "main")
	f.cfg.Agents[0].WorkDir = ".worktrees/lane-{{.AgentBase}}"
	f.cfg.Agents[0].PreStart = []string{"prepare-worktree"}
	f.work(f.store, "work-1", "helper", "", false)
	f.tick()
	firstID := ""
	for _, call := range f.sp.SnapshotCalls() {
		if call.Method != "Start" {
			continue
		}
		claimPreparedPoolStart(t, f, call)
		firstID = call.Config.Env["GC_SESSION_ID"]
	}
	if firstID == "" {
		t.Fatalf("first session never started\n%s", f.stderr.String())
	}
	if err := sessionFrontDoor(f.store).SetMarker(firstID, "pin_awake", "true"); err != nil {
		t.Fatal(err)
	}
	f.work(f.store, "work-2", "helper", "", false)
	f.tick()
	starts := 0
	for _, call := range f.sp.SnapshotCalls() {
		if call.Method != "Start" {
			continue
		}
		starts++
		if call.Config.Env["GC_SESSION_ID"] != firstID {
			claimPreparedPoolStart(t, f, call)
		}
	}
	if starts != 2 {
		t.Fatalf("starts=%d, want pinned first and active second\n%s", starts, f.stderr.String())
	}
	rows, err := loadSessionBeads(f.store)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		t.Logf("session=%s state=%s pending=%s pin=%s trigger=%s claim=%s", row.ID, row.Metadata["state"], row.Metadata["pending_create_claim"], row.Metadata["pin_awake"], row.Metadata[beadmeta.TriggerBeadIDMetadataKey], row.Metadata[beadmeta.CurrentClaimBeadIDMetadataKey])
		if row.Metadata["state"] == "start-pending" || row.Metadata["pending_create_claim"] == "true" {
			t.Fatalf("session remained pending: %v", row.Metadata)
		}
	}
}

func claimPreparedPoolStart(t *testing.T, f *deferredSurvivorFixture, call runtime.Call) {
	t.Helper()
	trigger := call.Config.Env["GC_TRIGGER_BEAD_ID"]
	if trigger == "" || call.Config.Env["GC_WORKTREE_BEAD_ID"] != trigger {
		t.Fatalf("startup workspace binding does not match trigger %q", trigger)
	}
	if len(call.Config.PreStart) == 0 {
		t.Fatal("missing pre_start")
	}
	rows, err := f.store.ListByMetadata(map[string]string{beadmeta.RoutedToMetadataKey: "helper"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Force the other bead to precede this session's trigger, independent of
	// store ordering and which session the allocator started first.
	var candidates []beads.Bead
	for _, row := range rows {
		if row.Type == "task" && row.ID != trigger {
			candidates = append(candidates, row)
		}
	}
	for _, row := range rows {
		if row.ID == trigger {
			candidates = append(candidates, row)
		}
	}
	data, err := json.Marshal(candidates)
	if err != nil {
		t.Fatal(err)
	}
	spy := &stampMetaSpy{}
	ops := poolClaimOps(string(data), map[string]string{}, trigger, spy)
	// Materialize the pre_start contract using real linked worktrees. The
	// runtime is fake, but Git itself enforces exclusive branch ownership.
	mustGit(t, f.path, "worktree", "add", "-b", trigger, call.Config.WorkDir)
	ops.CurrentClaim = sessionFrontDoor(f.store).CurrentClaimBeadID
	ops.DrainPending = func(string) (bool, error) { return false, nil }
	ops.Claim = func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
		state := "in_progress"
		if err := f.store.Update(id, beads.UpdateOpts{Status: &state, Assignee: &assignee}); err != nil {
			return beads.Bead{}, false, err
		}
		b, err := f.store.Get(id)
		return b, err == nil, err
	}
	ops.StampSessionClaim = func(sessionID, beadID string) error {
		_, err := sessionFrontDoor(f.store).SetCurrentClaim(sessionID, beadID)
		return err
	}
	ops.StampWorkMeta = func(_ context.Context, _ string, _ []string, id, _ string, patch map[string]string) error {
		return f.store.SetMetadataBatch(id, patch)
	}
	ops.ReadWorkMeta = func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
		return f.store.Get(id)
	}
	ops.EmitExecutionStepStarted = func(beads.Bead, string, []string, string) {}
	opts := hookClaimOptions{Assignee: call.Config.Env["GC_SESSION_ID"], SessionID: call.Config.Env["GC_SESSION_ID"], RouteTargets: []string{"helper"}, JSON: true}
	for key, value := range call.Config.Env {
		opts.Env = append(opts.Env, key+"="+value)
	}
	var stdout, stderr bytes.Buffer
	if code := doHookClaim("query", f.path, opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("claim: %d: %s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.BeadID != trigger {
		t.Fatalf("workspace for %s claimed %s", trigger, result.BeadID)
	}
	t.Logf("session=%s dir=%s pre_start_trigger=%s claimed=%s", opts.Assignee, call.Config.WorkDir, trigger, result.BeadID)
}

func TestHookClaimPreparedWorkspaceDoesNotFallThrough(t *testing.T) {
	for _, status := range []string{"absent", "closed", "claimed", "lost-race"} {
		t.Run(status, func(t *testing.T) {
			rows := []beads.Bead{{ID: "other", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}}}
			if status != "absent" {
				trigger := beads.Bead{ID: "trigger", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}}
				if status == "closed" {
					trigger.Status = "closed"
				}
				if status == "claimed" {
					trigger.Status = "in_progress"
					trigger.Assignee = "sibling"
				}
				rows = append(rows, trigger)
			}
			data, err := json.Marshal(rows)
			if err != nil {
				t.Fatal(err)
			}
			drained := false
			ops := hookClaimOps{
				WorkspaceBranch: func(string) (string, error) { return "trigger", nil },
				CurrentClaim:    func(string) (string, error) { return "", nil },
				Runner:          func(string, string) (string, error) { return string(data), nil },
				Claim: func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, bool, error) {
					if id != "trigger" || status != "lost-race" {
						t.Fatalf("unexpected claim of %s", id)
					}
					return beads.Bead{ID: id, Assignee: "sibling"}, false, nil
				},
				EmitClaimRejected: func(string, string, string) {},
				DrainAck:          func(io.Writer) error { drained = true; return nil },
			}
			var stdout, stderr bytes.Buffer
			code := doHookClaim("query", "/rig", hookClaimOptions{Assignee: "seat", RouteTargets: []string{"worker"}, Env: []string{"GC_WORKTREE_CLAIM_GUARD=1", "GC_WORKTREE_BEAD_ID=trigger"}, DrainAck: true, JSON: true}, ops, &stdout, &stderr)
			if code != 0 || !drained {
				t.Fatalf("code=%d drained=%v stderr=%s", code, drained, stderr.String())
			}
		})
	}
}

func TestHookWorkspaceBindingLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, branch, last, offered, want string
		failed                            bool
	}{
		{name: "idle start later bound", branch: "work-2", last: "work-2", offered: "work-3"},
		{name: "hierarchical child", branch: "work/work-1.1", last: "work-1", offered: "work-1.1", want: "work-1.1"},
		{name: "initial", branch: "work/work-1", offered: "work-1", want: "work-1"},
		{name: "detached continuation", last: "work-1", offered: "work-2", want: "work-2"},
		{name: "continuation adoption", branch: "work-2", last: "work-2", offered: "work-2", want: "work-2"},
		{name: "closed continuation still attached", branch: "work-2", last: "work-2", offered: "work-3"},
		{name: "reusable agent branch", branch: "agent/worker", offered: "work-2", want: "work-2"},
		{name: "whole token", branch: "work-10", offered: "work-2", want: "work-2"},
		{name: "ambiguous branch", branch: "work-1-work-2", offered: "work-2", failed: true},
		{name: "probe failed", offered: "work-2", failed: true},
		{name: "receipt failed", branch: "work-1", offered: "work-1", failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := hookClaimOptions{SessionID: "session", Env: []string{"GC_WORKTREE_CLAIM_GUARD=1", "GC_WORKTREE_BEAD_ID=work-1", "GC_DIR=/lane"}}
			if tc.name == "idle start later bound" {
				opts.Env = []string{"GC_WORKTREE_CLAIM_GUARD=1", "GC_DIR=/lane"}
			}
			ops := hookClaimOps{
				WorkspaceBranch: func(dir string) (string, error) {
					if dir != "/lane" {
						t.Fatalf("dir=%s", dir)
					}
					if tc.name == "probe failed" {
						return "", errors.New("git failed")
					}
					return tc.branch, nil
				},
				CurrentClaim: func(string) (string, error) {
					if tc.name == "receipt failed" {
						return "", errors.New("store failed")
					}
					return tc.last, nil
				},
			}
			got, err := filterHookWorkspaceCandidates([]beads.Bead{{ID: tc.offered}}, opts, ops)
			if (err != nil) != tc.failed {
				t.Fatalf("error=%v want failed=%v", err, tc.failed)
			}
			if tc.failed {
				return
			}
			want := 0
			if tc.want != "" {
				want = 1
			}
			if len(got) != want || want == 1 && got[0].ID != tc.want {
				t.Fatalf("got=%v want=%s", got, tc.want)
			}
		})
	}
}
