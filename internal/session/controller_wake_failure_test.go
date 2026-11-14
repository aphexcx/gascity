package session

import (
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
)

var errMessageWakePersistence = errors.New("injected message wake persistence failure")

type messageWakeFailureStore struct {
	beads.Store
	city, step string
	failed     bool
}

func (s *messageWakeFailureStore) fail(step string) bool {
	if s.failed || step != s.step {
		return false
	}
	queue, err := nudgequeue.LoadState(s.city)
	if err != nil {
		return false
	}
	for _, item := range queue.Pending {
		if item.Message == "new message" {
			s.failed = true
			return true
		}
	}
	return false
}

func (s *messageWakeFailureStore) Get(id string) (beads.Bead, error) {
	if s.fail("read") {
		return beads.Bead{}, errMessageWakePersistence
	}
	if s.fail("conflict") {
		return beads.Bead{}, &WakeConflictError{SessionID: id, State: "closed"}
	}
	return s.Store.Get(id)
}

func (s *messageWakeFailureStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if strings.HasPrefix(q.Label, "session:") && s.fail("wait-read") {
		return nil, errMessageWakePersistence
	}
	return s.Store.List(q)
}

func (s *messageWakeFailureStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if s.fail("wait-cancel") {
		return 0, errMessageWakePersistence
	}
	return s.Store.CloseAll(ids, metadata)
}

func (s *messageWakeFailureStore) SetMetadataBatch(id string, metadata map[string]string) error {
	if metadata["wake_request"] != "" && s.fail("wake-write") {
		return errMessageWakePersistence
	}
	return s.Store.SetMetadataBatch(id, metadata)
}

func TestControllerMessageRollsBackOnWakePersistenceFailure(t *testing.T) {
	for _, step := range []string{"read", "wait-read", "wait-cancel", "wake-write"} {
		t.Run(step, func(t *testing.T) {
			city := t.TempDir()
			mem, sp := beads.NewMemStore(), runtime.NewFake()
			store := &messageWakeFailureStore{Store: mem, city: city, step: step}
			m := NewManagerWithOptions(store, sp, WithCityPath(city), WithRestartCoordinator(func() bool { return true }, nil))
			original := startSessionSubmitPoller
			pollers := 0
			startSessionSubmitPoller = func(string, string, string) error { pollers++; return nil }
			t.Cleanup(func() { startSessionSubmitPoller = original })
			info, err := m.CreateSession(t.Context(), CreateOptions{BeadOnly: true, Template: "runner", Command: "launch", WorkDir: city})
			if err != nil {
				t.Fatal(err)
			}
			if err := mem.SetMetadataBatch(info.ID, SleepPatch(time.Now(), "idle")); err != nil {
				t.Fatal(err)
			}
			if _, err := mem.Create(waitBeadFixture("", "open", info.ID, map[string]string{"state": waitStatePending})); err != nil {
				t.Fatal(err)
			}
			if err := nudgequeue.WithState(city, func(s *nudgequeue.State) error {
				s.Pending = append(s.Pending, nudgequeue.Item{ID: "other-item", Message: "keep me"})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			delivery, err := m.Send(t.Context(), info.ID, "new message", "launch", runtime.Config{})
			if !errors.Is(err, errMessageWakePersistence) || delivery.Queued || !store.failed {
				t.Errorf("err=%v queued=%v injected=%v", err, delivery.Queued, store.failed)
			}
			queue, err := nudgequeue.LoadState(city)
			if err != nil {
				t.Fatal(err)
			}
			if len(queue.Pending) != 1 || queue.Pending[0].ID != "other-item" || len(queue.InFlight)+len(queue.Dead) != 0 {
				t.Errorf("failed message retained or unrelated message removed: %+v", queue)
			}
			if pollers != 0 || sp.CountCalls("Start", info.SessionName) != 0 {
				t.Errorf("failed enqueue activated pollers=%d starts=%d", pollers, sp.CountCalls("Start", info.SessionName))
			}
		})
	}
}

func TestControllerMessageLifecycleConflictStaysQueued(t *testing.T) {
	city := t.TempDir()
	mem, sp := beads.NewMemStore(), runtime.NewFake()
	store := &messageWakeFailureStore{Store: mem, city: city, step: "conflict"}
	pokes := 0
	m := NewManagerWithOptions(store, sp, WithCityPath(city), WithRestartCoordinator(func() bool { return true }, func() error { pokes++; return nil }))
	original := startSessionSubmitPoller
	startSessionSubmitPoller = func(string, string, string) error { return nil }
	t.Cleanup(func() { startSessionSubmitPoller = original })
	info, err := m.CreateSession(t.Context(), CreateOptions{BeadOnly: true, Template: "runner", Command: "launch", WorkDir: city})
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.SetMetadataBatch(info.ID, SleepPatch(time.Now(), "idle")); err != nil {
		t.Fatal(err)
	}
	delivery, err := m.Send(t.Context(), info.ID, "new message", "launch", runtime.Config{})
	if err != nil || !delivery.Queued || !store.failed || pokes != 1 {
		t.Fatalf("err=%v queued=%v injected=%v pokes=%d", err, delivery.Queued, store.failed, pokes)
	}
	queue, err := nudgequeue.LoadState(city)
	if err != nil || len(queue.Pending) != 1 {
		t.Fatalf("queue=%+v err=%v", queue, err)
	}
}

func TestControllerReplacementWaitLimitsPokes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var pokes []time.Time
		m := NewManagerWithOptions(beads.NewMemStore(), runtime.NewFake(), WithRestartCoordinator(func() bool { return true }, func() error {
			pokes = append(pokes, time.Now())
			return nil
		}))
		start := time.Now()
		err := m.awaitControllerRestart(t.Context(), func() error { return ErrControllerRestartPending })
		if err == nil || time.Since(start) != 2*time.Minute {
			t.Fatalf("err=%v elapsed=%s", err, time.Since(start))
		}
		// Include both endpoints: the timeout and the final poll can become
		// runnable together at t=120s, after the initial poke at t=0.
		if len(pokes) == 0 || !pokes[0].Equal(start) || len(pokes) > 121 {
			t.Fatalf("pokes=%d, want entry poke and at most one per second", len(pokes))
		}
		for i := 1; i < len(pokes); i++ {
			if gap := pokes[i].Sub(pokes[i-1]); gap < time.Second {
				t.Fatalf("poke gap=%s, want at least one second", gap)
			}
		}
	})
}
