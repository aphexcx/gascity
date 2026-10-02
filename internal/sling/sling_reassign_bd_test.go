package sling

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

type assignmentGuardExit struct{}

func (assignmentGuardExit) Error() string { return "assignment precondition failed" }
func (assignmentGuardExit) ExitCode() int { return 13 }

// Reassignment must work on bd's pre-revision schema without stealing a claim
// that arrives after the read. The runner models bd's atomic guard verdict.
func TestReassignBdAssignmentGuards(t *testing.T) {
	for _, tc := range []struct {
		name             string
		status           string
		claimDuringWrite bool
	}{
		{name: "open", status: "open"},
		{name: "deferred", status: "deferred"},
		{name: "blocked", status: "blocked"},
		{name: "intervening claim", status: "open", claimDuringWrite: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := beads.Bead{ID: "bd-42", Status: tc.status, Assignee: "human", Metadata: map[string]string{"gc.work_outcome": "blocked", "gc.work_commit": "old", "unrelated": "keep"}}
			updates := 0
			store := beads.NewBdStore("/city", func(_, _ string, args ...string) ([]byte, error) {
				if slices.Contains(args, "--help") {
					return []byte("--if-assignee --if-status"), nil
				}
				if args[0] == "show" {
					return json.Marshal([]beads.Bead{state})
				}
				if args[0] != "update" {
					return nil, fmt.Errorf("unexpected bd command: %v", args)
				}
				updates++
				flag := func(name string) string {
					i := slices.Index(args, name)
					if i < 0 || i+1 == len(args) {
						t.Fatalf("missing %s in %v", name, args)
					}
					return args[i+1]
				}
				if got := flag("--if-status"); got != tc.status {
					t.Fatalf("raw status guard = %q, want %q", got, tc.status)
				}
				if got := flag("--if-assignee"); got != "human" {
					t.Fatalf("assignee guard = %q", got)
				}
				if tc.claimDuringWrite {
					state.Status, state.Assignee = "in_progress", "worker-session"
					return nil, assignmentGuardExit{}
				}
				state.Status, state.Assignee = flag("--status"), flag("--assignee")
				for _, key := range append(append([]string{}, ParkReleaseMetadataKeys...), "gc.work_outcome", "gc.work_commit") {
					if !slices.Contains(args, key+"=") {
						t.Errorf("missing atomic metadata clear %s in %v", key, args)
					}
					state.Metadata[key] = ""
				}
				return nil, nil
			})
			before, err := store.Get(state.ID)
			if err != nil {
				t.Fatal(err)
			}
			err = reopenForReassignInStore(store, state.ID, before)
			if tc.claimDuringWrite {
				if err == nil {
					t.Fatal("reassignment succeeded over intervening claim")
				}
				if state.Status != "in_progress" || state.Assignee != "worker-session" || !reflect.DeepEqual(state.Metadata, before.Metadata) {
					t.Fatalf("claim or metadata overwritten: %+v", state)
				}
			} else if err != nil || state.Status != "open" || state.Assignee != "" || state.Metadata["gc.work_outcome"] != "" || state.Metadata["unrelated"] != "keep" {
				t.Fatalf("reassignment failed: state=%+v err=%v", state, err)
			}
			if updates != 1 {
				t.Errorf("updates = %d, want one guarded attempt", updates)
			}
		})
	}
}

func TestReassignBdValidatesCanonicalHold(t *testing.T) {
	for _, tc := range []struct{ name, status, deadline, wantErr string }{
		{name: "indefinite", status: "deferred"},
		{name: "expired", status: "deferred", deadline: "2000-01-01T00:00:00Z"},
		{name: "scheduled", status: "deferred", deadline: "2099-01-01T00:00:00Z", wantErr: "clear the scheduled deferral before reassignment"},
		{name: "closed", status: "closed", wantErr: "is closed"},
		{name: "claimed", status: "in_progress", wantErr: "in_progress"},
		{name: "invalid deadline", status: "deferred", deadline: "broken", wantErr: "parsing JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			store := beads.NewBdStore("/city", func(_, _ string, args ...string) ([]byte, error) {
				if slices.Contains(args, "--help") {
					return []byte("--if-assignee --if-status"), nil
				}
				if args[0] == "show" {
					row := map[string]string{"id": "bd-42", "status": tc.status, "assignee": "human"}
					if tc.deadline != "" {
						row["defer_until"] = tc.deadline
					}
					return json.Marshal([]map[string]string{row})
				}
				if args[0] != "update" {
					return nil, fmt.Errorf("unexpected bd command: %v", args)
				}
				writes++
				return nil, nil
			})
			// The optimized SQL projection exposes normalized status but may
			// omit deferral fields. The canonical read must govern the release.
			err := reopenForReassignInStore(store, "bd-42", beads.Bead{ID: "bd-42", Status: "open", Assignee: "human"})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || writes != 0 {
					t.Fatalf("error=%v writes=%d", err, writes)
				}
			} else if err != nil || writes != 1 {
				t.Fatalf("error=%v writes=%d", err, writes)
			}
		})
	}
}
