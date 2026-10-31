package doctor

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/nudgequeue"
)

func TestNudgeDeadLettersCheck(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)
	check := NewNudgeDeadLettersCheck()
	check.now = func() time.Time { return now }
	ctx := &CheckContext{CityPath: dir}
	if r := check.Run(ctx); r.Status != StatusOK {
		t.Fatalf("missing queue: %+v", r)
	}
	if _, err := os.Stat(nudgequeue.StatePath(dir)); !os.IsNotExist(err) {
		t.Fatalf("read-only check created a queue: %v", err)
	}
	if err := nudgequeue.WithState(dir, func(s *nudgequeue.State) error {
		s.Pending = []nudgequeue.Item{{ID: "live"}}
		s.Dead = []nudgequeue.Item{
			{ID: "n-old", Agent: "worker-a", SessionID: "s-1", LastError: "draft_ownership_lost", DeadAt: now.Add(-2 * time.Hour)},
			{ID: "n-new", Agent: "worker-b", LastError: "expired", DeadAt: now.Add(-time.Minute)},
			{ID: "n-superseded", LastError: "superseded", DeadAt: now.Add(-time.Hour)},
			{ID: "n-replayed", ReplayID: "n-successor", DeadAt: now.Add(-time.Hour)},
			{ID: "n-dismissed", DismissedAt: now, DeadAt: now.Add(-time.Hour)},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(nudgequeue.StatePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	r := check.Run(ctx)
	if r.Status != StatusWarning || !strings.Contains(r.Message, "2 dead-letter nudges") || !strings.Contains(r.Message, "2h0m0s") || len(r.Details) != 2 {
		t.Fatalf("doctor row: %+v", r)
	}
	for _, value := range []string{"n-old", "worker-a", "s-1", "draft_ownership_lost", "n-new", "worker-b", "expired"} {
		if !strings.Contains(strings.Join(r.Details, "\n"), value) {
			t.Fatalf("doctor omitted %q: %+v", value, r)
		}
	}
	for _, detail := range r.Details {
		if !strings.Contains(detail, "age=") || !strings.Contains(detail, "cap=7d") || !strings.Contains(detail, "expires=") {
			t.Errorf("missing per-item recovery window: %s", detail)
		}
	}
	after, err := os.ReadFile(nudgequeue.StatePath(dir))
	if err != nil || string(after) != string(before) {
		t.Fatalf("doctor mutated queue: %v", err)
	}
	if err := nudgequeue.WithState(dir, func(s *nudgequeue.State) error {
		for i := range s.Dead {
			if s.Dead[i].UnresolvedDeadLetter() {
				s.Dead[i].DismissedAt = now
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if r := check.Run(ctx); r.Status != StatusOK || len(r.Details) != 0 {
		t.Fatalf("resolved entries reported unresolved: %+v", r)
	}
	if err := os.WriteFile(nudgequeue.StatePath(dir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := check.Run(ctx); r.Status != StatusError {
		t.Fatalf("corrupt queue reported green: %+v", r)
	}
}
