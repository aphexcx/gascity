package beads_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestBdStoreAssignmentGuardReadRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		readErr        error
		wantErr        bool
	}{
		{name: "active claim", response: `[{"id":"bd-42","status":"in_progress","assignee":"human"}]`},
		{name: "closed", response: `[{"id":"bd-42","status":"closed","assignee":"human"}]`},
		{name: "different owner", response: `[{"id":"bd-42","status":"open","assignee":"another"}]`},
		{name: "new indefinite hold", response: `[{"id":"bd-42","status":"deferred","assignee":"human"}]`},
		{name: "new scheduled hold", response: `[{"id":"bd-42","status":"deferred","assignee":"human","defer_until":"2099-01-01T00:00:00Z"}]`},
		{name: "read failure", readErr: errors.New("ledger unavailable"), wantErr: true},
		{name: "id collision", response: `[{"id":"bd-420","status":"open","assignee":"human"}]`, wantErr: true},
		{name: "absent", response: `[]`, wantErr: true},
		{name: "missing status", response: `[{"id":"bd-42","assignee":"human"}]`, wantErr: true},
		{name: "bad JSON", response: `broken`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewBdStore("/city", func(_, _ string, args ...string) ([]byte, error) {
				if args[0] != "show" {
					t.Fatalf("mutation after invalid read: %v", args)
				}
				return []byte(tc.response), tc.readErr
			})
			updated, err := store.UpdateIfAssignmentMatches("bd-42", beads.Bead{ID: "bd-42", Status: "open", Assignee: "human"}, beads.UpdateOpts{Status: strPtr("open"), Assignee: strPtr("")})
			if updated || (err != nil) != tc.wantErr {
				t.Fatalf("updated=%v error=%v", updated, err)
			}
			if tc.readErr != nil && !errors.Is(err, tc.readErr) {
				t.Fatalf("lost read error: %v", err)
			}
		})
	}
}

func TestBdStoreAssignmentGuardWriteFailures(t *testing.T) {
	for _, tc := range []struct {
		name, detail         string
		code                 int
		wantErr, unsupported bool
	}{
		{name: "intervening state", code: 13},
		{name: "old bd assignee", detail: "unknown flag: --if-assignee", code: 1, wantErr: true, unsupported: true},
		{name: "old bd status", detail: "unknown flag: --if-status", code: 1, wantErr: true, unsupported: true},
		{name: "backend error", detail: "permission denied", code: 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			runner := &releaseVerbRunner{show: func(string) ([]byte, error) { return []byte(`[{"id":"bd-42","status":"open","assignee":""}]`), nil }}
			runner.reply = func(args []string) ([]byte, error) {
				writes++
				joined := strings.Join(args, "\x00")
				if !strings.Contains(joined, "--if-assignee\x00") || !strings.Contains(joined, "--if-status\x00open") {
					t.Fatalf("unguarded write: %v", args)
				}
				return []byte(tc.detail), exitErrorWithDetail(t, tc.code, tc.detail)
			}
			store := beads.NewBdStore("/city", runner.run)
			updated, err := store.UpdateIfAssignmentMatches("bd-42", beads.Bead{Status: "open"}, beads.UpdateOpts{Status: strPtr("open"), Assignee: strPtr("")})
			if updated || (err != nil) != tc.wantErr || writes != 1 {
				t.Fatalf("updated=%v error=%v writes=%d", updated, err, writes)
			}
			if errors.Is(err, beads.ErrConditionalWriteUnsupported) != tc.unsupported {
				t.Fatalf("unsupported classification: %v", err)
			}
		})
	}
}

func TestBdStoreAssignmentGuardRejectsUnguardedEdits(t *testing.T) {
	for _, opts := range []beads.UpdateOpts{{ParentID: strPtr("bd-parent")}, {Labels: []string{"add"}}, {RemoveLabels: []string{"remove"}}} {
		store := beads.NewBdStore("/city", func(_, _ string, args ...string) ([]byte, error) {
			t.Fatalf("runner called for unsupported edits: %v", args)
			return nil, nil
		})
		updated, err := store.UpdateIfAssignmentMatches("bd-42", beads.Bead{Status: "open"}, opts)
		if updated || !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
			t.Fatalf("updated=%v error=%v", updated, err)
		}
	}
}
