package main

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// Raw bd work queries must not let a shipped release consume a worker ahead
// of pending work. Resetting the outcome makes it claimable again, while an
// existing in-progress owner must still receive its work for finish recovery.
func TestHookClaimShippedWork(t *testing.T) {
	for _, tc := range []struct {
		name, status, assignee, outcome, wantID, wantReason string
		wantClaims                                          []string
	}{
		{name: "released head", status: "open", outcome: beadmeta.WorkOutcomeShipped, wantID: "work-pending", wantReason: "claimed", wantClaims: []string{"work-pending"}},
		{name: "explicit outcome reset", status: "open", wantID: "work-head", wantReason: "claimed", wantClaims: []string{"work-head"}},
		{name: "retained active claim", status: "in_progress", assignee: "worker-1", outcome: beadmeta.WorkOutcomeShipped, wantID: "work-head", wantReason: "existing_assignment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head := beads.Bead{ID: "work-head", Type: "task", Status: tc.status, Assignee: tc.assignee, Metadata: map[string]string{
				beadmeta.RoutedToMetadataKey: "worker", beadmeta.WorkOutcomeMetadataKey: tc.outcome, beadmeta.WorkCommitMetadataKey: "commit-1",
			}}
			pending := beads.Bead{ID: "work-pending", Type: "task", Status: "open", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}}
			store := beads.NewMemStoreFrom(2, []beads.Bead{head, pending}, nil)
			output, err := json.Marshal([]beads.Bead{head, pending})
			if err != nil {
				t.Fatal(err)
			}
			recorder := &turnBoundClaimRecorder{}
			ops := recorder.ops(t, string(output))
			ops.Claim = func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
				recorder.claims = append(recorder.claims, id)
				status := "in_progress"
				if err := store.Update(id, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
					return beads.Bead{}, false, err
				}
				claimed, err := store.Get(id)
				return claimed, true, err
			}
			ops.ReadWorkMeta = func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) { return store.Get(id) }
			var stdout, stderr bytes.Buffer
			code := doHookClaim("bd ready --json", "/rig", hookClaimOptions{
				Assignee: "worker-1", IdentityCandidates: []string{"worker-1"}, RouteTargets: []string{"worker"}, JSON: true,
			}, ops, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("doHookClaim=%d stderr=%s", code, stderr.String())
			}
			result := decodeTurnBoundResult(t, stdout.String())
			if result.Action != "work" || result.BeadID != tc.wantID || result.Reason != tc.wantReason {
				t.Errorf("result=%+v, want %s %s", result, tc.wantReason, tc.wantID)
			}
			if !reflect.DeepEqual(recorder.claims, tc.wantClaims) {
				t.Errorf("claimed=%v, want %v", recorder.claims, tc.wantClaims)
			}
			if tc.outcome == beadmeta.WorkOutcomeShipped {
				after, err := store.Get(head.ID)
				if err != nil || !reflect.DeepEqual(after, head) {
					t.Errorf("shipped work changed: before=%+v after=%+v error=%v", head, after, err)
				}
			}
		})
	}
}

func TestFilterUnreadyHookCandidatesShippedReleaseScope(t *testing.T) {
	input := `[
		{"id":"released","status":"open","assignee":"","metadata":{"gc.work_outcome":"shipped"}},
		{"id":"released-null","status":"open","assignee":null,"metadata":{"gc.work_outcome":"shipped"}},
		{"id":"released-omitted","status":"open","metadata":{"gc.work_outcome":"shipped"}},
		{"id":"retained-open","status":"open","assignee":"worker-1","metadata":{"gc.work_outcome":"shipped"}},
		{"id":"retained-active","status":"in_progress","assignee":"worker-1","metadata":{"gc.work_outcome":"shipped"}},
		{"id":"reset","status":"open","metadata":{"gc.work_outcome":""}},
		{"id":"ordinary","status":"open"}
	]`
	output := filterUnreadyHookCandidates(input, time.Unix(0, 0))
	var rows []beads.Bead
	if err := json.Unmarshal([]byte(output), &rows); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, row := range rows {
		got = append(got, row.ID)
	}
	want := []string{"retained-open", "retained-active", "reset", "ordinary"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered IDs=%v, want %v", got, want)
	}
}
