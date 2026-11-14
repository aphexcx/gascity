package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestOrdinaryStartAndAttachSkipControllerProbe(t *testing.T) {
	for _, operation := range []string{"start", "attach"} {
		for _, state := range []string{"live", "asleep"} {
			t.Run(operation+"/"+state, func(t *testing.T) {
				store, sp := beads.NewMemStore(), runtime.NewFake()
				m := NewManagerWithOptions(store, sp)
				info, err := m.CreateSession(t.Context(), CreateOptions{
					Template: "runner", Command: "launch", WorkDir: t.TempDir(),
					ExtraMeta: map[string]string{NamedSessionMetadataKey: "true"},
				})
				if err != nil {
					t.Fatal(err)
				}
				if state == "asleep" {
					if err := sp.Stop(info.SessionName); err != nil {
						t.Fatal(err)
					}
					if err := store.SetMetadataBatch(info.ID, SleepPatch(time.Now(), "idle")); err != nil {
						t.Fatal(err)
					}
				}
				probes := 0
				WithRestartCoordinator(func() bool { probes++; return true }, nil)(m)
				if pending, err := m.ControllerRestartPending(info.ID); err != nil || pending {
					t.Fatalf("ordinary entry hint: pending=%v err=%v", pending, err)
				}
				if operation == "start" {
					err = m.Start(t.Context(), info.ID, "launch", runtime.Config{})
				} else {
					err = m.Attach(t.Context(), info.ID, "launch", runtime.Config{})
				}
				if err != nil {
					t.Fatal(err)
				}
				if probes != 0 || !sp.IsRunning(info.SessionName) {
					t.Fatalf("ordinary entry probes=%d running=%v", probes, sp.IsRunning(info.SessionName))
				}
			})
		}
	}
}

func TestMessagesQueueDuringControllerRestart(t *testing.T) {
	for _, pending := range []string{"restart", "reset", "kill", "config-drift", "dormant", "crashed", "running-restart", "held"} {
		for _, surface := range []string{"send", "immediate", "wait-idle", "default", "follow_up", "interrupt_now"} {
			t.Run(pending+"/"+surface, func(t *testing.T) {
				store, sp := beads.NewMemStore(), runtime.NewFake()
				cityPath := t.TempDir()
				pokes := 0
				m := NewManagerWithOptions(store, sp, WithCityPath(cityPath), WithRestartCoordinator(func() bool { return true }, func() error { pokes++; return nil }))
				info, err := m.CreateSession(t.Context(), CreateOptions{
					BeadOnly: true, Template: "runner", Command: "launch", WorkDir: cityPath,
					ExtraMeta: map[string]string{NamedSessionMetadataKey: "true", NamedSessionIdentityMetadata: "runner", "provider_kind": "codex"},
				})
				if err != nil {
					t.Fatal(err)
				}
				pollerCalls := 0
				originalPoller := startSessionSubmitPoller
				startSessionSubmitPoller = func(city, key, name string) error {
					pollerCalls++
					if city != cityPath || key != info.ID || name != info.SessionName {
						t.Errorf("poller identity = (%q, %q, %q), want (%q, %q, %q)", city, key, name, cityPath, info.ID, info.SessionName)
					}
					return nil
				}
				t.Cleanup(func() { startSessionSubmitPoller = originalPoller })
				patch := MetadataPatch{"restart_requested": "true", "state": "active"}
				switch pending {
				case "reset":
					patch = RestartRequestPatch("", time.Now())
				case "kill":
					patch = KillPendingPatch(time.Now())
				case "config-drift":
					patch = ConfigDriftResetPatch(StateAsleep, "", time.Now())
				case "dormant", "held":
					patch = MetadataPatch{"state": "asleep", "sleep_reason": "idle", NamedSessionMetadataKey: "", NamedSessionIdentityMetadata: ""}
					if pending == "held" {
						patch["held_until"] = time.Now().Add(time.Hour).Format(time.RFC3339)
					}
				case "crashed":
					patch = MetadataPatch{"state": "active"}
				case "running-restart":
					if err := sp.Start(t.Context(), info.SessionName, runtime.Config{Command: "outgoing"}); err != nil {
						t.Fatal(err)
					}
				}
				if err := store.SetMetadataBatch(info.ID, patch); err != nil {
					t.Fatal(err)
				}
				starts := sp.CountCalls("Start", info.SessionName)
				var queued bool
				switch surface {
				case "send", "immediate", "wait-idle":
					var delivery runtime.NudgeDelivery
					switch surface {
					case "send":
						delivery, err = m.Send(t.Context(), info.ID, "hello", "launch", runtime.Config{})
					case "immediate":
						delivery, err = m.SendImmediate(t.Context(), info.ID, "hello", "launch", runtime.Config{})
					case "wait-idle":
						delivery, err = m.TryWaitIdleNudge(t.Context(), info.ID, "user", "hello", "launch", runtime.Config{})
					}
					queued = delivery.Queued
				default:
					var outcome SubmitOutcome
					outcome, err = m.Submit(t.Context(), info.ID, "hello", "launch", runtime.Config{}, SubmitIntent(surface))
					queued = outcome.Queued
				}
				wantQueued, wantActivations := true, 1
				if pending == "kill" {
					wantQueued, wantActivations = false, 0
					if !errors.Is(err, ErrSessionKillPending) {
						t.Fatalf("message error = %v, want ErrSessionKillPending", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if queued != wantQueued || pokes != wantActivations || sp.CountCalls("Start", info.SessionName) != starts {
					t.Fatalf("queued=%v pokes=%d starts=%d", queued, pokes, sp.CountCalls("Start", info.SessionName))
				}
				if pollerCalls != wantActivations {
					t.Errorf("deferred delivery poller activations = %d, want %d", pollerCalls, wantActivations)
				}
				state, err := nudgequeue.LoadState(cityPath)
				if err != nil {
					t.Fatal(err)
				}
				if len(state.Pending) != wantActivations || (wantQueued && state.Pending[0].Message != "hello") {
					t.Fatalf("queue = %+v", state.Pending)
				}
				b, err := store.Get(info.ID)
				if err != nil {
					t.Fatal(err)
				}
				if pending == "kill" && b.Metadata["wake_request"] != "" {
					t.Error("kill-fenced message recorded a wake")
				}
				for key, value := range patch {
					if pending == "held" && key == "held_until" {
						continue
					}
					if b.Metadata[key] != value {
						t.Errorf("pending marker %s changed", key)
					}
				}
				if pending == "dormant" || pending == "held" || pending == "config-drift" || pending == "crashed" {
					if b.Metadata["wake_request"] != "explicit" || b.Metadata["held_until"] != "" {
						t.Error("dormant message did not record an explicit wake and clear the hold")
					}
				}
			})
		}
	}
}

func TestControllerMessageRejectsLiveKillFence(t *testing.T) {
	city := t.TempDir()
	store, sp := beads.NewMemStore(), runtime.NewFake()
	pokes := 0
	m := NewManagerWithOptions(store, sp, WithCityPath(city), WithRestartCoordinator(func() bool { return true }, func() error { pokes++; return nil }))
	info, err := m.CreateSession(t.Context(), CreateOptions{BeadOnly: true, Template: "runner", Command: "launch", WorkDir: city})
	if err != nil {
		t.Fatal(err)
	}
	if err := sp.Start(t.Context(), info.SessionName, runtime.Config{Command: "outgoing"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataBatch(info.ID, KillPendingPatch(time.Now())); err != nil {
		t.Fatal(err)
	}
	_, err = m.Send(t.Context(), info.ID, "fenced message", "caller-launch", runtime.Config{})
	if !errors.Is(err, ErrSessionKillPending) {
		t.Errorf("Send error = %v, want ErrSessionKillPending", err)
	}
	state, loadErr := nudgequeue.LoadState(city)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(state.Pending)+len(state.InFlight)+len(state.Dead) != 0 || pokes != 0 {
		t.Errorf("pending=%d inflight=%d dead=%d pokes=%d", len(state.Pending), len(state.InFlight), len(state.Dead), pokes)
	}
	if sp.CountCalls("Start", info.SessionName) != 1 || sp.CountCalls("Nudge", info.SessionName) != 0 {
		t.Error("fenced Send started or nudged a runtime")
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Metadata["wake_request"] != "" {
		t.Errorf("wake_request=%q", b.Metadata["wake_request"])
	}
}

func TestStartAndAttachWaitForControllerReplacement(t *testing.T) {
	for _, attach := range []bool{false, true} {
		name := "start"
		if attach {
			name = "attach"
		}
		t.Run(name, func(t *testing.T) {
			store, sp := beads.NewMemStore(), runtime.NewFake()
			m := NewManagerWithOptions(store, sp)
			info, err := m.CreateSession(t.Context(), CreateOptions{BeadOnly: true, Template: "runner", Command: "launch", WorkDir: t.TempDir(), ExtraMeta: map[string]string{NamedSessionMetadataKey: "true", NamedSessionIdentityMetadata: "runner"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetMetadataBatch(info.ID, RestartRequestPatch("", time.Now())); err != nil {
				t.Fatal(err)
			}
			pokes := 0
			WithRestartCoordinator(func() bool { return true }, func() error {
				pokes++
				// Runtime-only is the controller boundary and must acquire the
				// same lock released by the waiting user operation.
				return m.StartRuntimeOnly(context.Background(), info.ID, "controller-launch", runtime.Config{})
			})(m)
			if attach {
				err = m.Attach(t.Context(), info.ID, "caller-launch", runtime.Config{})
			} else {
				err = m.Start(t.Context(), info.ID, "caller-launch", runtime.Config{})
			}
			if err != nil {
				t.Fatal(err)
			}
			if pokes != 1 || sp.CountCalls("Start", info.SessionName) != 1 {
				t.Fatalf("pokes=%d starts=%d", pokes, sp.CountCalls("Start", info.SessionName))
			}
			if got := sp.LastStartConfig(info.SessionName).Command; got != "controller-launch" {
				t.Fatalf("command=%q", got)
			}
		})
	}
}

func TestMessageDeferralRequiresRunningController(t *testing.T) {
	for _, named := range []bool{false, true} {
		store, sp := beads.NewMemStore(), runtime.NewFake()
		m := NewManagerWithOptions(store, sp, WithCityPath(t.TempDir()), WithRestartCoordinator(func() bool { return !named }, nil))
		meta := map[string]string{}
		if named {
			meta[NamedSessionMetadataKey] = "true"
			meta[NamedSessionIdentityMetadata] = "runner"
		}
		info, err := m.CreateSession(t.Context(), CreateOptions{BeadOnly: true, Template: "runner", Command: "launch", WorkDir: t.TempDir(), ExtraMeta: meta})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SetMetadataBatch(info.ID, RestartRequestPatch("", time.Now())); err != nil {
			t.Fatal(err)
		}
		delivery, err := m.Send(t.Context(), info.ID, "hello", "launch", runtime.Config{})
		if err != nil {
			t.Fatal(err)
		}
		wantStarts := 0
		if named {
			wantStarts = 1
		}
		if delivery.Queued != !named || sp.CountCalls("Start", info.SessionName) != wantStarts {
			t.Fatalf("named=%v: queued=%v starts=%d", named, delivery.Queued, sp.CountCalls("Start", info.SessionName))
		}
	}
}

func TestQueuedReplayWaitsForReplacement(t *testing.T) {
	store, sp := beads.NewMemStore(), runtime.NewFake()
	m := NewManagerWithOptions(store, sp, WithRestartCoordinator(func() bool { return true }, nil))
	info, err := m.CreateSession(t.Context(), CreateOptions{Template: "runner", Command: "launch", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{NamedSessionMetadataKey: "true", NamedSessionIdentityMetadata: "runner"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataBatch(info.ID, RestartRequestPatch("", time.Now())); err != nil {
		t.Fatal(err)
	}
	for _, waitIdle := range []bool{false, true} {
		var delivery runtime.NudgeDelivery
		if waitIdle {
			delivery, err = m.TryWaitIdleNudgeLiveOnly(t.Context(), info.ID, "queue", "hello")
		} else {
			delivery, err = m.SendLiveOnly(t.Context(), info.ID, "hello")
		}
		if err != nil {
			t.Fatal(err)
		}
		if delivery.Landed() || delivery.Queued {
			t.Fatalf("outgoing runtime accepted replay: %+v", delivery)
		}
	}
	if got := sp.CountCalls("Nudge", info.SessionName) + sp.CountCalls("NudgeNow", info.SessionName); got != 0 {
		t.Fatalf("outgoing runtime received %d messages", got)
	}
}

func TestControllerReplacementWaitHasBound(t *testing.T) {
	for _, attach := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			store, sp := beads.NewMemStore(), runtime.NewFake()
			m := NewManagerWithOptions(store, sp, WithRestartCoordinator(func() bool { return true }, nil))
			info, err := m.CreateSession(t.Context(), CreateOptions{BeadOnly: true, Template: "runner", Command: "launch", WorkDir: t.TempDir(), ExtraMeta: map[string]string{NamedSessionMetadataKey: "true", NamedSessionIdentityMetadata: "runner"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetMetadataBatch(info.ID, RestartRequestPatch("", time.Now())); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			if attach {
				err = m.Attach(t.Context(), info.ID, "launch", runtime.Config{})
			} else {
				err = m.Start(t.Context(), info.ID, "launch", runtime.Config{})
			}
			if err == nil || !strings.Contains(err.Error(), "pending restart") {
				t.Errorf("wait returned %v, want pending restart error", err)
			}
			if time.Since(start) != 2*time.Minute || sp.CountCalls("Start", info.SessionName) != 0 {
				t.Errorf("elapsed=%s starts=%d", time.Since(start), sp.CountCalls("Start", info.SessionName))
			}
		})
	}
}
