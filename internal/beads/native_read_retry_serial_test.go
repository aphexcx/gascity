package beads

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	beadslib "github.com/steveyegge/beads"
)

func TestNativeReadRetrySerializesSlowCalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		active, maximum := 0, 0
		release := make(chan struct{})
		storage := &nativeDoltStorageSpy{searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			mu.Lock()
			active++
			maximum = max(maximum, active)
			mu.Unlock()
			<-release
			mu.Lock()
			active--
			mu.Unlock()
			return nil, nil
		}}
		store := newNativeDoltStoreForTest(storage)
		done := make(chan error, 2)
		for range 2 {
			go func() { _, err := store.List(ListQuery{Label: "sample"}); done <- err }()
		}
		synctest.Wait()
		close(release)
		for range 2 {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
		if maximum != 1 {
			t.Fatalf("%d reads of the same kind in flight, want 1", maximum)
		}
	})
}

func TestNativeReadRetryBackoffGrows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var starts []time.Time
		storage := &nativeDoltStorageSpy{searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			starts = append(starts, time.Now())
			if len(starts) < 5 {
				return nil, errors.New("invalid connection")
			}
			return nil, nil
		}}
		store := newNativeDoltStoreForTest(storage)
		store.reopen = func(context.Context) (beadslib.Storage, error) { return storage, nil }
		if _, err := store.List(ListQuery{Label: "sample"}); err != nil {
			t.Fatal(err)
		}
		for i := 2; i < len(starts); i++ {
			previous := starts[i-1].Sub(starts[i-2])
			current := starts[i].Sub(starts[i-1])
			if current <= previous {
				t.Fatalf("retry delay did not grow: %s then %s", previous, current)
			}
		}
	})
}

func TestNativeReadRetryKindsRemainIndependent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		storage := &nativeDoltStorageSpy{
			searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
				entered <- struct{}{}
				<-release
				return nil, nil
			},
			countIssues: func(context.Context, string, beadslib.IssueFilter) (int64, error) {
				return 1, nil
			},
		}
		store := newNativeDoltStoreForTest(storage)
		done := make(chan error, 1)
		go func() { _, err := store.List(ListQuery{Label: "sample"}); done <- err }()
		<-entered
		if _, err := store.Count(context.Background(), ListQuery{AllowScan: true}); err != nil {
			t.Error(err)
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestNativeReadRetryQueueHonorsCallerCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		storage := &nativeDoltStorageSpy{countIssues: func(context.Context, string, beadslib.IssueFilter) (int64, error) {
			entered <- struct{}{}
			<-release
			return 0, nil
		}}
		store := newNativeDoltStoreForTest(storage)
		query := ListQuery{AllowScan: true, IncludeClosed: true}
		done := make(chan error, 1)
		go func() { _, err := store.Count(context.Background(), query); done <- err }()
		<-entered
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := store.Count(ctx, query); !errors.Is(err, context.Canceled) {
			t.Errorf("canceled queued count: %v", err)
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if len(entered) != 0 {
			t.Fatal("canceled read reached storage")
		}
	})
}

func TestNativeReadRetryQueueSharesDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{}, 2)
		storage := &nativeDoltStorageSpy{searchIssues: func(ctx context.Context, _ string, _ beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			entered <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		store := newNativeDoltStoreForTest(storage)
		store.readRetryBudgetOverride = time.Second
		done := make(chan error, 2)
		go func() { _, err := store.List(ListQuery{Label: "sample"}); done <- err }()
		<-entered
		go func() { _, err := store.List(ListQuery{Label: "sample"}); done <- err }()
		for range 2 {
			if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error=%v", err)
			}
		}
		if len(entered) != 0 {
			t.Fatal("queued read started after spending its deadline waiting")
		}
	})
}
