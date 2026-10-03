//go:build gascity_native_beads

package beads

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

func TestDoltliteAssignmentReadsHiddenDeferral(t *testing.T) {
	for _, deadline := range []string{"", "2000-01-01T00:00:00Z", "2099-01-01T00:00:00Z"} {
		t.Run("deadline="+deadline, func(t *testing.T) {
			store := newDoltliteStoreWithIssues(t, []testDoltliteIssue{{ID: "bd-held", Status: "deferred", Assignee: "human", IssueType: "task"}})
			projection, err := store.Get("bd-held")
			if err != nil {
				t.Fatal(err)
			}
			if projection.Status != "open" || projection.DeferUntil != nil || projection.IndefinitelyDeferred {
				t.Fatalf("fixture must expose limited SQL projection: %+v", projection)
			}
			store.BdStore = NewBdStore(store.BdStore.dir, func(_, _ string, args ...string) ([]byte, error) {
				if args[0] != "show" {
					t.Fatalf("unexpected mutation: %v", args)
				}
				row := map[string]string{"id": "bd-held", "status": "deferred", "assignee": "human"}
				if deadline != "" {
					row["defer_until"] = deadline
				}
				return json.Marshal([]map[string]string{row})
			})
			writer, ok := AssignmentConditionalWriterFor(WorkStore{Store: store})
			if !ok {
				t.Fatal("missing authoritative assignment capability")
			}
			actual, err := writer.ReadAssignment("bd-held")
			if err != nil {
				t.Fatal(err)
			}
			if actual.IndefinitelyDeferred != (deadline == "") || (actual.DeferUntil == nil) != (deadline == "") {
				t.Fatalf("missing canonical deferral: %+v", actual)
			}
		})
	}
}

func TestDoltliteAssignmentGuardWithoutRevision(t *testing.T) {
	for _, rawStatus := range []string{"blocked", "deferred"} {
		t.Run(rawStatus, func(t *testing.T) {
			store := newDoltliteStoreWithIssues(t, []testDoltliteIssue{{ID: "bd-held", Status: rawStatus, Assignee: "human", IssueType: "task"}})
			before, err := store.Get("bd-held")
			if err != nil {
				t.Fatal(err)
			}
			if before.Revision != 0 || before.Status != "open" {
				t.Fatalf("fixture must expose normalized pre-revision snapshot: %+v", before)
			}
			store.readyHash, store.orderRunHash, store.sessionHash = "populated", "populated", "populated"
			writes := 0
			store.BdStore = NewBdStore(store.BdStore.dir, func(_, _ string, args ...string) ([]byte, error) {
				if args[0] == "show" {
					return json.Marshal([]map[string]string{{"id": "bd-held", "status": rawStatus, "assignee": "human"}})
				}
				if !slices.Contains(args, "update") {
					return nil, fmt.Errorf("unexpected bd command: %v", args)
				}
				writes++
				for key, want := range map[string]string{"--if-status": rawStatus, "--if-assignee": "human", "--status": "open", "--assignee": ""} {
					i := slices.Index(args, key)
					if i < 0 || i+1 == len(args) || args[i+1] != want {
						t.Fatalf("%s=%q missing in %v", key, want, args)
					}
				}
				return nil, nil
			})
			writer, ok := AssignmentConditionalWriterFor(WorkStore{Store: store})
			if !ok {
				t.Fatal("typed DoltLite store lacks assignment capability")
			}
			before, err = writer.ReadAssignment(before.ID)
			if err != nil {
				t.Fatal(err)
			}
			open, empty := "open", ""
			updated, err := writer.UpdateIfAssignmentMatches(before.ID, before, UpdateOpts{Status: &open, Assignee: &empty, Metadata: map[string]string{"gc.work_outcome": ""}})
			if err != nil || !updated || writes != 1 {
				t.Fatalf("updated=%v error=%v writes=%d", updated, err, writes)
			}
			if store.readyHash != "" || store.orderRunHash != "" || store.sessionHash != "" {
				t.Fatal("DoltLite projections were not invalidated")
			}
		})
	}
}
