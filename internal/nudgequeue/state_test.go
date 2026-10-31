package nudgequeue

import (
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
)

func TestClaimDueBatchKeepsRetryPayloadStable(t *testing.T) {
	now := time.Unix(100, 0)
	old := now.Add(-time.Second)
	future := now.Add(time.Minute)
	match := func(item Item) bool { return item.Agent == "target" }
	for _, tt := range []struct {
		name  string
		state State
		want  []string
	}{
		{"retry excludes arrivals", State{Pending: []Item{
			{ID: "new", Agent: "target"},
			{ID: "b", Agent: "target", LastAttemptAt: old},
			{ID: "a", Agent: "target", LastAttemptAt: old},
		}}, []string{"a", "b"}},
		{"retry backoff blocks arrivals", State{Pending: []Item{
			{ID: "new", Agent: "target"},
			{ID: "a", Agent: "target", LastAttemptAt: old, DeliverAfter: future},
		}}, nil},
		{"inflight blocks arrivals", State{Pending: []Item{{ID: "new", Agent: "target"}}, InFlight: []Item{{ID: "a", Agent: "target"}}}, nil},
		{"other target does not block", State{Pending: []Item{{ID: "new", Agent: "target"}}, InFlight: []Item{{ID: "other", Agent: "other"}}}, []string{"new"}},
		{"fresh order ignores due time", State{Pending: []Item{
			{ID: "later", Agent: "target", CreatedAt: now},
			{ID: "earlier", Agent: "target", CreatedAt: old, DeliverAfter: now},
			{ID: "future", Agent: "target", DeliverAfter: future},
		}}, []string{"earlier", "later"}},
		{"oldest retry first", State{Pending: []Item{
			{ID: "later", Agent: "target", LastAttemptAt: now},
			{ID: "earlier", Agent: "target", LastAttemptAt: old},
		}}, []string{"earlier"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := len(tt.state.Pending) + len(tt.state.InFlight)
			claimed := ClaimDueBatch(&tt.state, now, time.Minute, match)
			var ids []string
			for _, item := range claimed {
				ids = append(ids, item.ID)
				if !item.ClaimedAt.Equal(now) || !item.LeaseUntil.Equal(future) {
					t.Fatalf("claim did not acquire lease: %+v", item)
				}
			}
			if !slices.Equal(ids, tt.want) || len(tt.state.Pending)+len(tt.state.InFlight) != before {
				t.Fatalf("claimed=%v want=%v; state=%+v", ids, tt.want, tt.state)
			}
		})
	}
}

// TestWithState_TimesOutInsteadOfBlockingForever guards ga-2kzci3 FR1/FR2:
// WithState itself -- not just the withStateBounded helper it wraps -- must
// give a caller that cannot acquire the queue lock within its default budget
// a clear, actionable error, never an unbounded hang. The bound was
// specified to live inside WithState, since every production caller goes
// through it directly, not sit beside it as dead code only a test exercises.
func TestWithState_TimesOutInsteadOfBlockingForever(t *testing.T) {
	cityPath := t.TempDir()

	if err := WithState(cityPath, func(state *State) error {
		state.Pending = append(state.Pending, Item{ID: "seed"})
		return nil
	}); err != nil {
		t.Fatalf("seed queue state: %v", err)
	}

	lockFile, err := os.OpenFile(LockPath(cityPath), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	defer lockFile.Close() //nolint:errcheck
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("hold queue lock: %v", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck

	done := make(chan error, 1)
	go func() {
		done <- WithState(cityPath, func(_ *State) error {
			t.Error("WithState invoked fn after failing to acquire the lock")
			return nil
		})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("WithState returned nil error while queue lock was held; want a timeout error")
		}
		lower := strings.ToLower(err.Error())
		mentionsLock := strings.Contains(lower, "lock")
		mentionsTimeout := strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out")
		if !mentionsLock || !mentionsTimeout {
			t.Fatalf("WithState error = %q, want a descriptive lock-timeout error (mentioning lock + timeout)", err)
		}
	case <-time.After(defaultLockWaitTimeout + 2*time.Second):
		t.Fatal("WithState blocked far past its default wait timeout while the queue lock was held (unbounded wait regression)")
	}
}

// TestWithStateBounded_TimesOutInsteadOfBlockingForever guards ga-2kzci3 FR1/FR2:
// a caller that cannot acquire the queue lock within its budget must get a
// clear, actionable error, never an unbounded hang.
func TestWithStateBounded_TimesOutInsteadOfBlockingForever(t *testing.T) {
	cityPath := t.TempDir()

	if err := WithState(cityPath, func(state *State) error {
		state.Pending = append(state.Pending, Item{ID: "seed"})
		return nil
	}); err != nil {
		t.Fatalf("seed queue state: %v", err)
	}

	lockFile, err := os.OpenFile(LockPath(cityPath), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	defer lockFile.Close() //nolint:errcheck
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("hold queue lock: %v", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck

	const waitTimeout = 150 * time.Millisecond
	done := make(chan error, 1)
	var called bool
	var mu sync.Mutex
	go func() {
		done <- withStateBounded(cityPath, waitTimeout, clock.Real{}, func(_ *State) error {
			mu.Lock()
			called = true
			mu.Unlock()
			return nil
		})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("withStateBounded returned nil error while queue lock was held; want a timeout error")
		}
		lower := strings.ToLower(err.Error())
		mentionsLock := strings.Contains(lower, "lock")
		mentionsTimeout := strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out")
		if !mentionsLock || !mentionsTimeout {
			t.Fatalf("withStateBounded error = %q, want a descriptive lock-timeout error (mentioning lock + timeout)", err)
		}
		mu.Lock()
		gotCalled := called
		mu.Unlock()
		if gotCalled {
			t.Fatal("withStateBounded invoked fn after failing to acquire the lock")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("withStateBounded blocked far past its wait timeout while the queue lock was held (unbounded wait regression)")
	}
}
