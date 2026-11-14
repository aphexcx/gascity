package beads

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"

	beadslib "github.com/steveyegge/beads"
)

func TestNativeListQueueCancellationKeepsActiveReadOwnership(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{}, 3)
		release := make(chan struct{})
		var calls atomic.Int32
		storage := &nativeDoltStorageSpy{searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			calls.Add(1)
			entered <- struct{}{}
			<-release
			return nil, nil
		}}
		store := newNativeDoltStoreForTest(storage)
		holderContext, cancelHolder := context.WithCancel(context.Background())
		defer cancelHolder()
		holderDone := make(chan error, 1)
		go func() {
			_, err := store.ListWithQueueContext(holderContext, ListQuery{Label: "holder"})
			holderDone <- err
		}()
		<-entered
		waitContext, cancelWait := context.WithCancel(context.Background())
		waitDone := make(chan error, 1)
		reader := HandlesFor(WithListQueueContext(waitContext, NewCachingStore(store, nil))).Live
		go func() { _, err := reader.List(ListQuery{Label: "abandoned"}); waitDone <- err }()
		synctest.Wait()
		cancelWait()
		if err := <-waitDone; !errors.Is(err, context.Canceled) {
			t.Errorf("queued read error=%v", err)
		}
		cancelHolder()
		nextDone := make(chan error, 1)
		go func() { _, err := store.List(ListQuery{Label: "next"}); nextDone <- err }()
		synctest.Wait()
		if calls.Load() != 1 {
			t.Errorf("caller cancellation released an active read's gate: calls=%d", calls.Load())
		}
		close(release)
		if err := <-holderDone; err != nil {
			t.Error(err)
		}
		if err := <-nextDone; err != nil {
			t.Error(err)
		}
		if calls.Load() != 2 {
			t.Errorf("abandoned queued read reached storage: calls=%d", calls.Load())
		}
	})
}
