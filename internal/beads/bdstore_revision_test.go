package beads

import (
	"encoding/json"
	"testing"
)

// TestBdIssueRevisionDecodesStringLegacyIntegerAndNull pins the bd 1.3.0 JSON
// contract at the show --json edge: bd emits the optimistic-concurrency token
// as a decimal string (beads #6053), older bd emitted a JSON integer, and a
// row with no token yet emits null. All three decode into the int64 token;
// the rolled gc 1aa0d83d9 declared the field int64 and every rig control
// dispatcher died on the first control bead it loaded (citadel, 2026-09-17).
func TestBdIssueRevisionDecodesStringLegacyIntegerAndNull(t *testing.T) {
	cases := []struct {
		name string
		json string
		want int64
	}{
		{"string token", `{"id":"gp-qp0s","title":"t","status":"open","revision":"303464683160428230"}`, 303464683160428230},
		{"negative string token", `{"id":"gp-7tz1","title":"t","status":"open","revision":"-9208529776834742732"}`, -9208529776834742732},
		{"legacy integer", `{"id":"gp-qp0s","title":"t","status":"open","revision":303464683160428230}`, 303464683160428230},
		{"null", `{"id":"gp-qp0s","title":"t","status":"open","revision":null}`, 0},
		{"absent", `{"id":"gp-qp0s","title":"t","status":"open"}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var issue bdIssue
			if err := json.Unmarshal([]byte(tc.json), &issue); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := int64(issue.Revision); got != tc.want {
				t.Fatalf("revision = %d, want %d", got, tc.want)
			}
			if got := issue.toBead().Revision; got != tc.want {
				t.Fatalf("toBead revision = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestBdRevisionRejectsNonNumericString(t *testing.T) {
	var r bdRevision
	if err := r.UnmarshalJSON([]byte(`"not-a-number"`)); err == nil {
		t.Fatal("expected a decode error for a non-numeric revision string")
	}
}
