package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// Factory policy wrappers and controller caches must retain the provider's
// assignment capability and evict their old human-held snapshot after release.
func TestBeadPolicyAssignmentGuardThroughCache(t *testing.T) {
	for _, policyOutside := range []bool{false, true} {
		t.Run(fmt.Sprintf("policy outside cache %v", policyOutside), func(t *testing.T) {
			current := beads.Bead{ID: "bd-held", Status: "open", Assignee: "human"}
			writes := 0
			backing := beads.NewBdStore("/city", func(_, _ string, args ...string) ([]byte, error) {
				if args[0] == "show" {
					return json.Marshal([]beads.Bead{current})
				}
				if args[0] != "update" {
					return nil, fmt.Errorf("unexpected bd command: %v", args)
				}
				writes++
				for key, want := range map[string]string{"--if-status": "open", "--if-assignee": "human", "--status": "open", "--assignee": ""} {
					i := slices.Index(args, key)
					if i < 0 || i+1 == len(args) || args[i+1] != want {
						t.Fatalf("missing %s=%q in %v", key, want, args)
					}
				}
				current.Assignee = ""
				return nil, nil
			})
			var wrapped beads.Store
			if policyOutside {
				wrapped = wrapStoreWithBeadPolicies(beads.NewCachingStoreForTest(backing, nil), nil)
			} else {
				wrapped = beads.NewCachingStoreForTest(wrapStoreWithBeadPolicies(backing, nil), nil)
			}
			if _, err := wrapped.Get(current.ID); err != nil {
				t.Fatal(err)
			}
			writer, ok := beads.AssignmentConditionalWriterFor(wrapped)
			if !ok {
				t.Fatal("wrapped production store lost assignment capability")
			}
			before, err := writer.ReadAssignment(current.ID)
			if err != nil {
				t.Fatal(err)
			}
			open, empty := "open", ""
			updated, err := writer.UpdateIfAssignmentMatches(current.ID, before, beads.UpdateOpts{Status: &open, Assignee: &empty})
			if err != nil || !updated || writes != 1 {
				t.Fatalf("updated=%v error=%v writes=%d", updated, err, writes)
			}
			after, err := wrapped.Get(current.ID)
			if err != nil || after.Assignee != "" {
				t.Fatalf("stale cached owner after release: %+v, %v", after, err)
			}
		})
	}
}
