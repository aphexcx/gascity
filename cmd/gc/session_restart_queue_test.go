package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

func TestRestartGapQueueDeliversToReplacementOnce(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	cityPath := t.TempDir()
	store, sp := beads.NewMemStore(), runtime.NewFake()
	m := session.NewManagerWithOptions(store, sp, session.WithCityPath(cityPath), session.WithRestartCoordinator(func() bool { return true }, func() error { return nil }))
	info, err := m.CreateSession(t.Context(), session.CreateOptions{
		BeadOnly: true, Template: "runner", Command: "launch", Provider: "codex", WorkDir: cityPath,
		ExtraMeta: map[string]string{session.NamedSessionMetadataKey: "true", session.NamedSessionIdentityMetadata: "runner"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataBatch(info.ID, session.RestartRequestPatch("", time.Now())); err != nil {
		t.Fatal(err)
	}
	delivery, err := m.Send(t.Context(), info.ID, "after replacement", "caller-launch", runtime.Config{})
	if err != nil || !delivery.Queued {
		t.Fatalf("delivery=%+v err=%v", delivery, err)
	}
	if sp.CountCalls("Start", info.SessionName) != 0 {
		t.Fatal("message started runtime")
	}
	if err := m.StartRuntimeOnly(t.Context(), info.ID, "controller-launch", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	idleSince := time.Now().Add(-time.Minute)
	sp.Activity = map[string]time.Time{info.SessionName: idleSince}
	target := nudgeTarget{
		cityPath: cityPath, agent: config.Agent{Name: "runner"}, sessionID: info.ID, sessionName: info.SessionName,
		continuationEpoch: b.Metadata["continuation_epoch"], resolved: &config.ResolvedProvider{Name: "codex"},
	}
	for attempt := 0; attempt < 2; attempt++ {
		delivered, err := tryDeliverQueuedNudgesByPoller(target, store, store, sp, time.Second, worker.LiveObservation{Running: true, LastActivity: &idleSince})
		if err != nil {
			t.Fatal(err)
		}
		if delivered != (attempt == 0) {
			t.Fatalf("attempt %d delivered=%v", attempt, delivered)
		}
	}
	if got := sp.CountCalls("Nudge", info.SessionName); got != 1 {
		t.Fatalf("delivered %d times", got)
	}
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending)+len(state.InFlight)+len(state.Dead) != 0 {
		t.Fatal("queue retained or dead-lettered the delivered message")
	}
}

func TestSupervisedSubmitFallbackQueuesWithoutCitySocket(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	cityPath := t.TempDir()
	store, sp := beads.NewMemStore(), runtime.NewFake()
	m := session.NewManagerWithOptions(store, sp, session.WithCityPath(cityPath))
	info, err := m.CreateSession(t.Context(), session.CreateOptions{
		BeadOnly: true, Template: "runner", Command: "launch", Provider: "codex", WorkDir: cityPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	previousManaged, previousPoke := nudgeCityUsesManagedReconciler, nudgePokeController
	probes, pokes := 0, 0
	nudgeCityUsesManagedReconciler = func(path string) bool { probes++; return path == cityPath }
	nudgePokeController = func(string) error { pokes++; return nil }
	t.Cleanup(func() { nudgeCityUsesManagedReconciler, nudgePokeController = previousManaged, previousPoke })
	// The local submit fallback uses this factory and Message entry even when
	// the supervisor has no per-city socket or HTTP API.
	handle, err := workerHandleForSessionWithConfig(cityPath, store, sp, nil, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if probes != 0 {
		t.Fatal("handle construction probed controller reachability")
	}
	result, err := handle.Message(t.Context(), worker.MessageRequest{Text: "after restart"})
	if err != nil || !result.Queued {
		t.Fatalf("submit=%+v err=%v", result, err)
	}
	if probes != 1 || pokes != 1 || sp.CountCalls("Start", info.SessionName) != 0 {
		t.Fatalf("probes=%d pokes=%d starts=%d", probes, pokes, sp.CountCalls("Start", info.SessionName))
	}
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil || len(state.Pending) != 1 {
		t.Fatalf("queue=%+v err=%v", state, err)
	}
}
