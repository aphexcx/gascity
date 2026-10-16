package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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
	// Materialize the pre_start contract using real linked worktrees. The
	// runtime is fake, but Git itself enforces exclusive branch ownership.
	mustGit(t, f.path, "worktree", "add", "-b", call.Config.Env["GC_TRIGGER_BEAD_ID"], call.Config.WorkDir)
	claimPoolStart(t, f, call)
}

func claimPoolStart(t *testing.T, f *deferredSurvivorFixture, call runtime.Call) {
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
	opts := hookClaimOptions{Assignee: call.Config.Env["GC_SESSION_ID"], SessionID: call.Config.Env["GC_SESSION_ID"], RouteTargets: []string{"helper"}, WorkspaceConfig: f.cfg, JSON: true}
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

func TestPoolWorkspaceDrainLaunchClaimsSourceStep(t *testing.T) {
	f := newDeferredSurvivorFixture(t)
	f.cfg.Workspace.Prefix = "gc"
	hookClaimInitRepo(t, f.path, "main")
	f.cfg.Agents[0].WorkDir = ".worktrees/lane-{{.AgentBase}}"
	f.cfg.Agents[0].PreStart = []string{"prepare-worktree"}
	sourceDir := filepath.Join(f.path, ".worktrees", "source")
	source := f.create(f.store, beads.Bead{Title: "source", Type: "task", Metadata: map[string]string{beadmeta.LegacyWorkDirMetadataKey: sourceDir}})
	mustGit(t, f.path, "worktree", "add", "-b", source.ID, sourceDir)
	root := f.create(f.store, beads.Bead{Title: "drain workflow", Type: "task", Metadata: map[string]string{beadmeta.DrainMemberIDMetadataKey: source.ID}})
	step := f.work(f.store, "drain-step", "helper", "", false)
	if err := f.store.SetMetadata(step.ID, beadmeta.RootBeadIDMetadataKey, root.ID); err != nil {
		t.Fatal(err)
	}
	f.tick()
	f.work(f.store, "unrelated", "helper", "", false)
	starts := 0
	for _, call := range f.sp.SnapshotCalls() {
		if call.Method != "Start" {
			continue
		}
		starts++
		if call.Config.WorkDir != sourceDir || call.Config.Env["GC_WORKTREE_CLAIM_GUARD"] != "1" {
			t.Fatalf("drain launch: dir=%s env=%v", call.Config.WorkDir, call.Config.Env)
		}
		claimPoolStart(t, f, call)
	}
	if starts != 1 {
		t.Fatalf("starts=%d, want 1\n%s", starts, f.stderr.String())
	}
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
			var result hookClaimJSONResult
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Reason != "workspace_bound" || !strings.Contains(stderr.String(), "workspace bound to trigger (branch trigger); 1 routed candidate(s) skipped") {
				t.Fatalf("reason=%s stderr=%s", result.Reason, stderr.String())
			}
		})
	}
}

func TestHookWorkspaceBindingLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, branch, last, offered, assignee, want string
		failed                                      bool
	}{
		{name: "idle start later bound", branch: "work-2", last: "work-2", offered: "work-3"},
		{name: "hierarchical child", branch: "work/work-1.1", last: "work-1", offered: "work-1.1", want: "work-1.1"},
		{name: "initial", branch: "work/work-1", offered: "work-1", want: "work-1"},
		{name: "detached continuation", last: "work-1", offered: "work-2", want: "work-2"},
		{name: "continuation adoption", branch: "work-2", last: "work-2", offered: "work-2", want: "work-2"},
		{name: "closed continuation still attached", branch: "work-2", last: "work-2", offered: "work-3", assignee: "session", want: "work-3"},
		{name: "unassigned sibling still filtered", branch: "work-2", last: "work-2", offered: "work-3"},
		{name: "reusable agent branch", branch: "agent/worker", offered: "work-2", want: "work-2"},
		{name: "whole token", branch: "work-10", offered: "work-2", want: "work-2"},
		{name: "ambiguous branch", branch: "work-1-work-2", offered: "work-2", failed: true},
		{name: "probe failed", offered: "work-2", failed: true},
		{name: "receipt failed", branch: "work-1", offered: "work-1", failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := hookClaimOptions{SessionID: "session", IdentityCandidates: []string{"session"}, Env: []string{"GC_WORKTREE_CLAIM_GUARD=1", "GC_WORKTREE_BEAD_ID=work-1", "GC_DIR=/lane"}}
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
			got, _, err := filterHookWorkspaceCandidates([]beads.Bead{{ID: tc.offered, Assignee: tc.assignee}}, "/rig", opts, ops, io.Discard)
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

func TestHookWorkspaceReplacementWithoutReceipt(t *testing.T) {
	cfg := &config.City{Rigs: []config.Rig{{Name: "project", Prefix: "work"}, {Name: "fleet", Prefix: "fleet-work"}}}
	cfg.Workspace.Prefix = "hq"
	for _, tc := range []struct {
		branch        string
		offered       string
		allow, failed bool
	}{
		{branch: "work-1"},
		{branch: "work-123456789abc"},
		{branch: "WORK-1", allow: true},
		{branch: "task/work-1-fix"},
		{branch: "work-1.2"},
		{branch: "hq-abc"},
		{branch: "fleet-work-abc"},
		{branch: "fleet-work-2"},
		{branch: "fleet-work-2-work-2", failed: true},
		{branch: "work-1-work-2", failed: true},
		{branch: "work-2", allow: true},
		{branch: "fleet-work-2", offered: "fleet-work-2", allow: true},
		{branch: "hq-abc.review-loop", offered: "hq-abc.review-loop", allow: true},
		{branch: "hq-abc.review-loop", offered: "hq-abc.review"},
		{branch: "agent/worker", allow: true},
		{branch: "", allow: true},
	} {
		t.Run(tc.branch, func(t *testing.T) {
			opts := hookClaimOptions{SessionID: "replacement", WorkspaceConfig: cfg, Env: []string{"GC_WORKTREE_CLAIM_GUARD=1", "GC_DIR=/lane"}}
			ops := hookClaimOps{
				WorkspaceBranch: func(string) (string, error) { return tc.branch, nil },
				CurrentClaim:    func(string) (string, error) { return "", nil },
				ReadWorkMeta: func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
					for _, known := range []string{"work-1", "work-1.2", "work-2", "work-123456789abc", "hq-abc", "fleet-work-abc", "fleet-work-2", "hq-abc.review-loop"} {
						if id == known {
							return beads.Bead{ID: id, Status: "closed"}, nil
						}
					}
					return beads.Bead{}, beads.ErrNotFound
				},
			}
			offered := tc.offered
			if offered == "" {
				offered = "work-2"
			}
			got, _, err := filterHookWorkspaceCandidates([]beads.Bead{{ID: offered}}, "/rig", opts, ops, io.Discard)
			if (err != nil) != tc.failed {
				t.Fatalf("error=%v want failed=%v", err, tc.failed)
			}
			if !tc.failed && (len(got) == 1) != tc.allow {
				t.Fatalf("candidates=%v want allow=%v", got, tc.allow)
			}
		})
	}
}

func TestHookWorkspaceInferredOwnerRequiresProvenance(t *testing.T) {
	for _, tc := range []struct {
		name, branch, owner, status, action, reason string
		readError                                   error
		otherLeg                                    bool
	}{
		{name: "reusable agent branch", branch: "gc-worker", action: "work", reason: "claimed"},
		{name: "ordinary branch", branch: "gc-hotfix", action: "work", reason: "claimed"},
		{name: "closed bead", branch: "gc-old", status: "closed", action: "drain", reason: "workspace_bound"},
		{name: "open bead", branch: "gc-old", status: "open", action: "drain", reason: "workspace_bound"},
		{name: "closed bead in another store", branch: "gc-old", status: "closed", otherLeg: true, action: "drain", reason: "workspace_bound"},
		{name: "absent longer prefix", branch: "fleet-gc-old", owner: "gc-old", status: "closed", action: "drain", reason: "workspace_bound"},
		{name: "overlapping same prefix", branch: "gc-gc-old", owner: "gc-old", status: "closed", action: "drain", reason: "workspace_bound"},
		{name: "closed wisp", branch: "gc-wisp-abc", status: "closed", action: "drain", reason: "workspace_bound"},
		{name: "closed child with branch suffix", branch: "task/gc-old.review-fix", owner: "gc-old.review", status: "closed", action: "drain", reason: "workspace_bound"},
		{name: "lookup error", branch: "gc-worker", readError: errors.New("store unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := poolClaimOps(`[{"id":"gc-next","status":"open","metadata":{"gc.routed_to":"worker"}}]`, map[string]string{}, tc.branch, &stampMetaSpy{})
			ops.WorkspaceBranch = func(string) (string, error) { return tc.branch, nil }
			ops.CurrentClaim = func(string) (string, error) { return "", nil }
			ops.DrainPending = func(string) (bool, error) { return false, nil }
			ops.EmitExecutionStepStarted = func(beads.Bead, string, []string, string) {}
			baseRead := ops.ReadWorkMeta
			owner := tc.owner
			if owner == "" {
				owner = tc.branch
			}
			reads, drained := 0, false
			ops.ReadWorkMeta = func(ctx context.Context, dir string, env []string, id, assignee string) (beads.Bead, error) {
				if id == "gc-next" {
					return baseRead(ctx, dir, env, id, assignee)
				}
				reads++
				if tc.readError != nil {
					return beads.Bead{}, tc.readError
				}
				if id != owner || tc.status == "" || tc.otherLeg && dir != "/other" {
					return beads.Bead{}, beads.ErrNotFound
				}
				return beads.Bead{ID: id, Status: tc.status}, nil
			}
			ops.WorkLegs = []hookStore{{dir: "/rig"}, {dir: "/other"}}
			ops.DrainAck = func(io.Writer) error { drained = true; return nil }
			opts := poolClaimOpts()
			opts.WorkspaceConfig = &config.City{Rigs: []config.Rig{{Name: "fleet", Prefix: "fleet-gc"}}}
			opts.WorkspaceConfig.Workspace.Prefix = "gc"
			opts.Env = append(opts.Env, "GC_WORKTREE_CLAIM_GUARD=1")
			opts.DrainAck = true
			var stdout, stderr bytes.Buffer
			code := doHookClaim("query", "/rig", opts, ops, &stdout, &stderr)
			if tc.readError != nil {
				if code != 1 || drained || !strings.Contains(stderr.String(), tc.readError.Error()) {
					t.Fatalf("code=%d drained=%v stderr=%s", code, drained, stderr.String())
				}
				return
			}
			var result hookClaimJSONResult
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if code != 0 || result.Action != tc.action || result.Reason != tc.reason || reads == 0 {
				t.Fatalf("code=%d result=%+v reads=%d stderr=%s", code, result, reads, stderr.String())
			}
		})
	}
}

func TestHookWorkspaceDrainMemberRelationship(t *testing.T) {
	for _, tc := range []struct {
		name, rootID, member string
		readError            error
		allow                bool
	}{
		{name: "shared source", rootID: "root", member: "source", allow: true},
		{name: "different source", rootID: "root", member: "other"},
		{name: "ordinary workflow", rootID: "root"},
		{name: "drain root", rootID: "step", member: "source", allow: true},
		{name: "missing root", rootID: "root", readError: beads.ErrNotFound},
		{name: "unreadable root", rootID: "root", readError: errors.New("store unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := hookClaimOptions{Env: []string{"GC_WORKTREE_CLAIM_GUARD=1", "GC_WORKTREE_BEAD_ID=source"}}
			ops := hookClaimOps{
				WorkspaceBranch: func(string) (string, error) { return "source", nil },
				ReadWorkMeta: func(_ context.Context, dir string, _ []string, id, _ string) (beads.Bead, error) {
					if dir != "/rig" || id != tc.rootID {
						t.Fatalf("read dir=%s id=%s", dir, id)
					}
					return beads.Bead{ID: id, Metadata: map[string]string{beadmeta.DrainMemberIDMetadataKey: tc.member}}, tc.readError
				},
			}
			candidate := beads.Bead{ID: "step", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: tc.rootID}}
			if tc.rootID == candidate.ID {
				candidate.Metadata[beadmeta.DrainMemberIDMetadataKey] = tc.member
			}
			got, skipped, err := filterHookWorkspaceCandidates([]beads.Bead{candidate}, "/rig", opts, ops, io.Discard)
			if tc.readError != nil {
				if !errors.Is(err, tc.readError) || len(got) != 0 || skipped {
					t.Fatalf("got=%v skipped=%v err=%v", got, skipped, err)
				}
				return
			}
			if err != nil || (len(got) == 1) != tc.allow || skipped == tc.allow {
				t.Fatalf("got=%v skipped=%v err=%v allow=%v", got, skipped, err, tc.allow)
			}
		})
	}
}

func TestHookWorkspaceReadHonorsCancellation(t *testing.T) {
	original := hookClaimCommandRunnerWithEnvContext
	t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = original })
	hookClaimCommandRunnerWithEnvContext = func(ctx context.Context, _ map[string]string) beads.CommandRunner {
		return func(string, string, ...string) ([]byte, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return []byte(`[{"id":"work-1","status":"closed"}]`), nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := hookReadClaimedBeadWithBdStore(ctx, "/rig", nil, "work-1", "worker")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("read error=%v, want cancellation", err)
	}
}

// Assignment belongs to the session even when its shared continuation workspace
// still names the previous step. Neither adoption nor promotion may drain it.
func TestHookClaimPreparedWorkspacePreservesAssignedWork(t *testing.T) {
	for _, tc := range []struct {
		name, status, assignee, reason string
	}{
		{"adoption", "in_progress", "gc__role-mc-sess1", "existing_assignment"},
		{"continuation sibling", "open", "mc-sess1", "ready_assignment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := []beads.Bead{
				{
					ID: "work-2", Status: tc.status, Assignee: tc.assignee,
					Metadata: map[string]string{"gc.continuation_group": "flow", "gc.routed_to": "worker"},
				},
				{ID: "work-3", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}},
			}
			data, err := json.Marshal(rows)
			if err != nil {
				t.Fatal(err)
			}
			ops := poolClaimOps(string(data), map[string]string{}, "work-1", &stampMetaSpy{})
			ops.WorkspaceBranch = func(string) (string, error) { return "work-1", nil }
			ops.CurrentClaim = func(string) (string, error) { return "work-1", nil }
			ops.DrainPending = func(string) (bool, error) { return false, nil }
			ops.DrainAck = func(io.Writer) error { t.Fatal("drained with assigned work"); return nil }
			ops.ListContinuation = func(context.Context, string, []string, string, string) ([]beads.Bead, error) { return nil, nil }
			ops.EmitExecutionStepStarted = func(beads.Bead, string, []string, string) {}
			opts := poolClaimOpts()
			opts.SessionID = "mc-sess1"
			opts.IdentityCandidates = append(opts.IdentityCandidates, opts.SessionID)
			opts.DrainAck = true
			opts.Env = append(opts.Env, "GC_WORKTREE_CLAIM_GUARD=1", "GC_WORKTREE_BEAD_ID=work-1")
			var stdout, stderr bytes.Buffer
			if code := doHookClaim("query", "/rig", opts, ops, &stdout, &stderr); code != 0 {
				t.Fatalf("claim: %d: %s", code, stderr.String())
			}
			var result hookClaimJSONResult
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Action != "work" || result.BeadID != "work-2" || result.Reason != tc.reason {
				t.Fatalf("result=%+v stderr=%s", result, stderr.String())
			}
		})
	}
}

func TestHookWorkspaceExplicitAgentWithoutSessionReceipt(t *testing.T) {
	for _, tc := range []struct {
		branch string
		want   int
	}{
		{"agent/worker", 1},
		{"work-1", 0},
	} {
		t.Run(tc.branch, func(t *testing.T) {
			opts := hookClaimOptions{Env: []string{"GC_WORKTREE_CLAIM_GUARD=1", "GC_WORKTREE_BEAD_ID=work-1"}}
			ops := hookClaimOps{
				WorkspaceBranch: func(string) (string, error) { return tc.branch, nil },
				CurrentClaim:    func(string) (string, error) { return "", errors.New("missing session ID") },
			}
			got, _, err := filterHookWorkspaceCandidates([]beads.Bead{{ID: "work-2"}}, "/rig", opts, ops, io.Discard)
			if err != nil || len(got) != tc.want {
				t.Fatalf("candidates=%v err=%v want=%d", got, err, tc.want)
			}
		})
	}
}

// A workspace refusal in one store must survive the fan-out without hiding a
// later successful claim or a mutation failure, or counting as queue divergence.
func TestHookWorkspaceFederatedDrain(t *testing.T) {
	for _, tc := range []struct {
		name, later, reason, action string
		claimError                  bool
	}{
		{"only foreign work", "other", "workspace_bound", "drain", false},
		{"later empty", "", "workspace_bound", "drain", false},
		{"later lost race", "work-1", "workspace_bound", "drain", false},
		{"later bound work", "work-1", "claimed", "work", false},
		{"later write failure", "work-1", "claims_errored", "drain", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			capture := captureDivergenceEmitter(t, &stdout)
			opts := hookFanoutClaimOpts()
			opts.Env = []string{"GC_WORKTREE_CLAIM_GUARD=1", "GC_WORKTREE_BEAD_ID=work-1"}
			stores := []hookStore{{dir: "rig-a", env: opts.Env}, {dir: "city", env: opts.Env}}
			run := func(_ string, dir string, _ []string) (string, error) {
				id, priority := "other", 0
				if dir == "city" {
					id, priority = tc.later, 1
				}
				if id == "" {
					return "[]", nil
				}
				// Force the guarded rig leg first; tied stores rotate by clock.
				data, err := json.Marshal([]beads.Bead{{ID: id, Status: "open", Priority: &priority, Metadata: map[string]string{"gc.routed_to": "worker"}}})
				return string(data), err
			}
			ops := hookFanoutBaseOps(func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
				if id != "work-1" {
					t.Fatalf("claimed unrelated bead %s", id)
				}
				if tc.claimError {
					return beads.Bead{}, false, errors.New("store write failed")
				}
				if tc.name == "later lost race" {
					return beads.Bead{ID: id, Assignee: "sibling"}, false, nil
				}
				return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee}, true, nil
			})
			ops.WorkspaceBranch = func(string) (string, error) { return "work-1", nil }
			ops.CurrentClaim = func(string) (string, error) { return "", nil }
			code := claimHookWorkWithRunner("query", "city", opts.Env, stores, opts, ops, run,
				func(string, error) { t.Fatal("query failed") }, &stdout, &stderr)
			var result hookClaimJSONResult
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stderr.String(), "workspace bound to work-1") {
				t.Fatalf("guarded rig leg was not attempted first: %s", stderr.String())
			}
			if code != 0 || result.Action != tc.action || result.Reason != tc.reason || capture.calls != 0 {
				t.Fatalf("code=%d result=%+v divergence=%d stderr=%s", code, result, capture.calls, stderr.String())
			}
		})
	}
}
