package session

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
)

func createTestSession(t *testing.T, m *Manager, template string) string {
	t.Helper()
	sp := m.sp.(*runtime.Fake)
	_ = sp // ensure fake provider available

	b, err := m.store.Create(beads.Bead{
		Title: template,
		Type:  BeadType,
		Labels: []string{
			LabelSession,
			"template:" + template,
		},
		Metadata: map[string]string{
			"template":     template,
			"state":        string(StateActive),
			"session_name": "s-test-" + template,
		},
	})
	if err != nil {
		t.Fatalf("creating test bead: %v", err)
	}
	return b.ID
}

func getState(t *testing.T, m *Manager, id string) State {
	t.Helper()
	b, err := m.store.Get(id)
	if err != nil {
		t.Fatalf("getting bead: %v", err)
	}
	return State(b.Metadata["state"])
}

func TestConformance_CreatingState(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := NewManagerWithOptions(store, sp)

	// Create a bead in creating state.
	b, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"template":             "worker",
			"state":                string(StateCreating),
			"pending_create_claim": "true",
			"sleep_reason":         "idle-timeout",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Confirm creation transitions to active.
	if err := m.ConfirmCreation(b.ID); err != nil {
		t.Fatal(err)
	}
	if s := getState(t, m, b.ID); s != StateActive {
		t.Errorf("state = %q, want %q", s, StateActive)
	}
	// Check state_reason.
	got, _ := store.Get(b.ID)
	if got.Metadata["state_reason"] != "creation_complete" {
		t.Errorf("state_reason = %q, want creation_complete", got.Metadata["state_reason"])
	}
	if got.Metadata["pending_create_claim"] != "" {
		t.Errorf("pending_create_claim = %q, want cleared", got.Metadata["pending_create_claim"])
	}
	if got.Metadata["sleep_reason"] != "" {
		t.Errorf("sleep_reason = %q, want cleared", got.Metadata["sleep_reason"])
	}
}

func TestConformance_DrainState(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := NewManagerWithOptions(store, sp)

	id := createTestSession(t, m, "worker")

	// Begin drain.
	if err := m.BeginDrain(id, "config-drift"); err != nil {
		t.Fatal(err)
	}
	if s := getState(t, m, id); s != StateDraining {
		t.Errorf("state = %q, want %q", s, StateDraining)
	}
	b, _ := store.Get(id)
	if b.Metadata["state_reason"] != "config-drift" {
		t.Errorf("state_reason = %q, want config-drift", b.Metadata["state_reason"])
	}
	if b.Metadata["drain_at"] == "" {
		t.Error("drain_at should be set")
	}

	// Archive after drain.
	if err := m.Archive(id, "drain_complete"); err != nil {
		t.Fatal(err)
	}
	if s := getState(t, m, id); s != StateArchived {
		t.Errorf("state = %q, want %q", s, StateArchived)
	}
	b, _ = store.Get(id)
	if b.Metadata["archived_at"] == "" {
		t.Error("archived_at should be set")
	}
	if b.Metadata["pending_create_claim"] != "" {
		t.Errorf("pending_create_claim = %q, want cleared", b.Metadata["pending_create_claim"])
	}
	if b.Metadata["continuity_eligible"] != "false" {
		t.Errorf("continuity_eligible = %q, want false", b.Metadata["continuity_eligible"])
	}
}

func TestConformance_QuarantineState(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := NewManagerWithOptions(store, sp)

	id := createTestSession(t, m, "worker")
	if err := store.SetMetadata(id, "last_woke_at", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	until := time.Now().Add(5 * time.Minute)
	if err := m.Quarantine(id, until, 3); err != nil {
		t.Fatal(err)
	}
	if s := getState(t, m, id); s != StateQuarantined {
		t.Errorf("state = %q, want %q", s, StateQuarantined)
	}
	b, _ := store.Get(id)
	if b.Metadata["quarantine_cycle"] != "3" {
		t.Errorf("quarantine_cycle = %q, want 3", b.Metadata["quarantine_cycle"])
	}
	if b.Metadata["quarantined_until"] == "" {
		t.Error("quarantined_until should be set")
	}
	if b.Metadata["last_woke_at"] != "" {
		t.Errorf("last_woke_at = %q, want cleared", b.Metadata["last_woke_at"])
	}
}

func TestConformance_ArchivedReactivation(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := NewManagerWithOptions(store, sp)

	id := createTestSession(t, m, "worker")

	// Archive first.
	if err := m.Archive(id, "scale-down"); err != nil {
		t.Fatal(err)
	}
	if s := getState(t, m, id); s != StateArchived {
		t.Fatalf("state = %q, want %q", s, StateArchived)
	}

	if err := store.SetMetadata(id, "pending_create_claim", "true"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(id, "continuity_eligible", "false"); err != nil {
		t.Fatal(err)
	}

	// Reactivate.
	if err := m.Reactivate(id); err != nil {
		t.Fatal(err)
	}
	if s := getState(t, m, id); s != StateAsleep {
		t.Errorf("state = %q, want %q after reactivation", s, StateAsleep)
	}
	b, _ := store.Get(id)
	if b.Metadata["state_reason"] != "reactivated" {
		t.Errorf("state_reason = %q, want reactivated", b.Metadata["state_reason"])
	}
	if b.Metadata["pending_create_claim"] != "" {
		t.Errorf("pending_create_claim = %q, want cleared", b.Metadata["pending_create_claim"])
	}
	if b.Metadata["continuity_eligible"] != "false" {
		t.Errorf("continuity_eligible = %q, want preserved false", b.Metadata["continuity_eligible"])
	}
	if b.Metadata["archived_at"] != "" {
		t.Error("archived_at should be cleared on reactivation")
	}
}

func TestConformance_SuspendDrainingTearsDownRuntimeWithoutRewritingState(t *testing.T) {
	// ga-rxhu2: `gc stop` and `gc restart` issue suspend on every session bead
	// with no state pre-filter. Suspend from draining used to return
	// ErrIllegalTransition, so every restart SKIPPED the draining seats and they
	// survived as live panes still holding their pool slot names. Handle draining
	// the way failed-create is already handled (#2597, same function): tear the
	// runtime down best-effort and report success.
	//
	// Deliberately NOT a new state-machine edge. Adding
	// StateDraining -> StateSuspended to CmdSuspend would release nothing (an OPEN
	// bead owns its session_name whatever its state) and would let an operator
	// "suspend" a seat mid-drain, losing the drain reason. So the early return
	// must leave `state` alone for the drain machinery to finish or the reconciler
	// to reap.
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := NewManagerWithOptions(store, sp)

	id := createTestSession(t, m, "worker")
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	sessName := b.Metadata["session_name"]
	if err := sp.Start(context.Background(), sessName, runtime.Config{}); err != nil {
		t.Fatalf("seeding runtime: %v", err)
	}
	if err := m.BeginDrain(id, "shutdown"); err != nil {
		t.Fatalf("BeginDrain: %v", err)
	}

	if err := m.SuspendForShutdown(id); err != nil {
		t.Fatalf("SuspendForShutdown(draining) = %v, want nil (must not block gc stop and leave a live pane holding a pool name)", err)
	}
	if sp.CountCalls("Stop", sessName) == 0 {
		t.Errorf("SuspendForShutdown(draining) did not tear down the runtime session %q", sessName)
	}
	if sp.IsRunning(sessName) {
		t.Errorf("runtime session %q still running after SuspendForShutdown(draining)", sessName)
	}
	if got := getState(t, m, id); got != StateDraining {
		t.Errorf("state = %q, want %q — the early return must not rewrite state and lose the drain", got, StateDraining)
	}
	after, err := store.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	if after.Metadata["suspended_at"] != "" {
		t.Errorf("suspended_at = %q, want empty: this path performs no suspension", after.Metadata["suspended_at"])
	}
	if after.Metadata["drain_at"] == "" {
		t.Error("drain_at was cleared; the drain reason must survive a city stop")
	}
}

// The draining latitude belongs to the city-stop SWEEP, not to an operator
// naming one session. A targeted Suspend of a draining seat has none of the
// gates the reconciler's terminal escalation insists on for the same class of
// kill, so it must keep returning the illegal transition instead of quietly
// killing a live agent mid-drain and reporting 200.
func TestConformance_OperatorSuspendStillRejectsDraining(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := NewManagerWithOptions(store, sp)

	id := createTestSession(t, m, "worker")
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	sessName := b.Metadata["session_name"]
	if err := sp.Start(context.Background(), sessName, runtime.Config{}); err != nil {
		t.Fatalf("seeding runtime: %v", err)
	}
	if err := m.BeginDrain(id, "shutdown"); err != nil {
		t.Fatalf("BeginDrain: %v", err)
	}

	err = m.Suspend(id)
	if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("Suspend(draining) = %v, want ErrIllegalTransition — an operator suspend must not become an ungated mid-drain kill", err)
	}
	if sp.CountCalls("Stop", sessName) != 0 {
		t.Errorf("operator Suspend(draining) tore down runtime %q anyway", sessName)
	}
	if !sp.IsRunning(sessName) {
		t.Errorf("runtime session %q was killed by a rejected operator suspend", sessName)
	}
}

// A teardown that FAILED must not be reported as success. Callers read nil as
// "the seat stopped" — gc stop prints "Stopped agent", counts it and records
// session.stopped — so swallowing the error makes the sweep claim success over a
// pane still alive holding its pool slot name: the exact wedge class here.
func TestStopTurnSuspendedRuntime(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(map[bool]string{false: "runtime absent", true: "runtime alive"}[running], func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			m := NewManagerWithOptions(store, sp)
			id := createTestSession(t, m, "worker")
			b, err := store.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			name := b.Metadata["session_name"]
			if running {
				if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.SetMetadata(id, "state", string(StateSuspended)); err != nil {
				t.Fatal(err)
			}
			if err := m.StopTurn(id); err != nil {
				t.Fatal(err)
			}
			wantInterrupts := 0
			if running {
				wantInterrupts = 1
			}
			if got := sp.CountCalls("Interrupt", name); got != wantInterrupts {
				t.Fatalf("interrupts=%d, want %d", got, wantInterrupts)
			}
			if state := getState(t, m, id); state != StateSuspended {
				t.Fatalf("state=%q, want suspended", state)
			}
			if sp.IsRunning(name) != running || sp.CountCalls("Stop", name) != 0 || sp.CountCalls("Start", name) != wantInterrupts {
				t.Fatal("StopTurn must not start or tear down a suspended runtime")
			}
		})
	}
}

func TestConformance_SuspendForShutdownRetriesSuspendedRuntime(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "stops runtime", true: "retains teardown failure"}[fail], func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			m := NewManagerWithOptions(store, sp)
			id := createTestSession(t, m, "worker")
			b, err := store.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			name := b.Metadata["session_name"]
			if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			if err := store.SetMetadata(id, "state", string(StateSuspended)); err != nil {
				t.Fatal(err)
			}
			if err := m.Suspend(id); err != nil {
				t.Fatal(err)
			}
			if sp.CountCalls("Stop", name) != 0 {
				t.Fatal("ordinary Suspend must remain idempotent")
			}
			wantErr := errors.New("runtime refused shutdown")
			if fail {
				sp.StopErrors[name] = wantErr
			}
			err = m.SuspendForShutdown(id)
			if errors.Is(err, wantErr) != fail || (!fail && err != nil) {
				t.Fatalf("shutdown error=%v, failure=%v", err, fail)
			}
			if sp.CountCalls("Stop", name) != 1 || sp.IsRunning(name) != fail {
				t.Fatalf("stops=%d running=%v failure=%v", sp.CountCalls("Stop", name), sp.IsRunning(name), fail)
			}
			after, err := store.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			if after.Metadata["state"] != string(StateSuspended) {
				t.Fatalf("state=%q, want suspended", after.Metadata["state"])
			}
		})
	}
}

func TestSuspendRetainsMixedBackendStopFailure(t *testing.T) {
	for _, mode := range []string{"operator", "shutdown", "suspended shutdown", "draining shutdown", "nested shutdown"} {
		t.Run(mode, func(t *testing.T) {
			store := beads.NewMemStore()
			terminal := runtime.NewFake()
			m := NewManagerWithOptions(store, terminal)
			id := createTestSession(t, m, "worker")
			b, err := store.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			name := b.Metadata["session_name"]
			if err := terminal.Start(context.Background(), name, runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			state := StateActive
			switch mode {
			case "suspended shutdown":
				state = StateSuspended
			case "draining shutdown":
				state = StateDraining
			}
			if err := store.SetMetadata(id, "state", string(state)); err != nil {
				t.Fatal(err)
			}
			wantErr := errors.New("live backend refused stop")
			terminal.StopErrors[name] = wantErr
			if mode == "nested shutdown" {
				terminal.StopErrors[name] = errors.Join(wantErr, runtime.ErrSessionNotFound)
			}
			// auto.Stop joins the failed live backend with its absent fallback.
			m.sp = auto.New(terminal, runtime.NewFake())
			if mode == "operator" {
				err = m.Suspend(id)
			} else {
				err = m.SuspendForShutdown(id)
			}
			if !errors.Is(err, wantErr) {
				t.Errorf("suspend error=%v, want live backend failure", err)
			}
			if got := getState(t, m, id); got != state || !terminal.IsRunning(name) {
				t.Errorf("state=%s runtime running=%v, want unchanged %s and running", got, terminal.IsRunning(name), state)
			}
		})
	}
}

func TestSuspendAcceptsOnlyAbsentStopCauses(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "operator", true: "shutdown"}[shutdown], func(t *testing.T) {
			sp := runtime.NewFake()
			m := NewManagerWithOptions(beads.NewMemStore(), sp)
			id := createTestSession(t, m, "worker")
			sp.StopErrors["s-test-worker"] = errors.Join(runtime.ErrSessionNotFound, runtime.ErrSessionNotFound)
			var err error
			if shutdown {
				err = m.SuspendForShutdown(id)
			} else {
				err = m.Suspend(id)
			}
			if err != nil || getState(t, m, id) != StateSuspended {
				t.Fatalf("pure absence did not suspend: %v", err)
			}
		})
	}
}

func TestConformance_SuspendForShutdownPropagatesDrainingTeardownFailure(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := NewManagerWithOptions(store, sp)

	id := createTestSession(t, m, "worker")
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	sessName := b.Metadata["session_name"]
	if err := sp.Start(context.Background(), sessName, runtime.Config{}); err != nil {
		t.Fatalf("seeding runtime: %v", err)
	}
	sp.StopErrors = map[string]error{sessName: errors.New("provider refused the stop")}
	if err := m.BeginDrain(id, "shutdown"); err != nil {
		t.Fatalf("BeginDrain: %v", err)
	}

	err = m.SuspendForShutdown(id)
	if err == nil {
		t.Fatal("SuspendForShutdown(draining) = nil despite a failed teardown; gc stop would report a live pane as stopped")
	}
	if !strings.Contains(err.Error(), "stopping runtime session") {
		t.Errorf("err = %v, want it to name the runtime teardown failure", err)
	}
}

// The scope control for the pin above. The draining escape hatch must be
// draining-ONLY, never a blanket "any state suspends" that would hide a real
// illegal-transition bug elsewhere.
func TestConformance_SuspendStillRejectsOtherIllegalStates(t *testing.T) {
	for _, from := range []State{StateStartPending, StateCreating, StateDrained} {
		t.Run(string(from), func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			m := NewManagerWithOptions(store, sp)

			id := createTestSession(t, m, "worker")
			if err := store.SetMetadata(id, "state", string(from)); err != nil {
				t.Fatalf("set state %q: %v", from, err)
			}

			err := m.Suspend(id)
			if err == nil {
				t.Fatalf("Suspend from %q should return ErrIllegalTransition", from)
			}
			if !errors.Is(err, ErrIllegalTransition) {
				t.Errorf("err = %v, want wrapping ErrIllegalTransition", err)
			}
			var ite *IllegalTransitionError
			if !errors.As(err, &ite) {
				t.Fatalf("err should unwrap to *IllegalTransitionError; got %T", err)
			}
			if ite.From != from {
				t.Errorf("ite.From = %q, want %q", ite.From, from)
			}
			if ite.Command != CmdSuspend {
				t.Errorf("ite.Command = %q, want %q", ite.Command, CmdSuspend)
			}
		})
	}
}

// The other scope control: an ACTIVE seat's suspend is completely unchanged —
// runtime stopped, state rewritten to suspended, suspended_at stamped.
func TestConformance_SuspendActiveSessionUnchanged(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := NewManagerWithOptions(store, sp)

	id := createTestSession(t, m, "worker")
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	sessName := b.Metadata["session_name"]
	if err := sp.Start(context.Background(), sessName, runtime.Config{}); err != nil {
		t.Fatalf("seeding runtime: %v", err)
	}

	if err := m.Suspend(id); err != nil {
		t.Fatalf("Suspend(active) = %v, want nil", err)
	}
	if got := getState(t, m, id); got != StateSuspended {
		t.Errorf("state = %q, want %q", got, StateSuspended)
	}
	if sp.IsRunning(sessName) {
		t.Errorf("runtime session %q still running after Suspend(active)", sessName)
	}
	after, err := store.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	if after.Metadata["suspended_at"] == "" {
		t.Error("suspended_at not stamped on an ordinary active suspend")
	}
}

// The state machine itself must NOT gain a draining edge: the fix lives in
// Manager.Suspend's pre-checks, and a table edge would release no pool name
// while silently converting a drain into a suspension.
func TestConformance_SuspendTableStillRejectsDraining(t *testing.T) {
	if _, err := Transition(StateDraining, CmdSuspend); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("Transition(draining, suspend) err = %v, want ErrIllegalTransition — "+
			"the escape hatch belongs in Manager.Suspend, not the transition table", err)
	}
}

func TestConformance_SuspendFailedCreateTearsDownRuntime(t *testing.T) {
	// #2597: `gc stop` issues suspend on every session bead, including
	// failed-create ones (it does not pre-filter by state). failed-create is a
	// create-rollback terminal state with no live turn to suspend, but it may
	// have leaked a runtime process. Under a backing-store outage the reconciler
	// cannot reap these (its close path requires a reachable store), so suspend
	// is the only thing that can tear the leaked process down. Suspend must
	// therefore succeed and stop the runtime rather than reject the command
	// with an illegal-transition error that blocks `gc stop` city-wide.
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := NewManagerWithOptions(store, sp)

	id := createTestSession(t, m, "dog")
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	sessName := b.Metadata["session_name"]

	// Seed a leaked runtime process and the failed-create landing state.
	if err := sp.Start(context.Background(), sessName, runtime.Config{}); err != nil {
		t.Fatalf("seeding runtime: %v", err)
	}
	if err := store.SetMetadata(id, "state", string(StateFailedCreate)); err != nil {
		t.Fatalf("set failed-create state: %v", err)
	}

	// Suspend(failed-create) must succeed so `gc stop` is not blocked
	// city-wide. The pre-fix regression returned a wrapped ErrIllegalTransition;
	// either symptom (any non-nil) trips this assertion and pinpoints the
	// regression by quoting the returned error.
	if err := m.Suspend(id); err != nil {
		t.Fatalf("Suspend(failed-create) = %v, want nil (must not block gc stop)", err)
	}
	if sp.CountCalls("Stop", sessName) == 0 {
		t.Errorf("Suspend(failed-create) did not tear down the leaked runtime session %q", sessName)
	}
	if sp.IsRunning(sessName) {
		t.Errorf("runtime session %q still running after Suspend(failed-create)", sessName)
	}
}

func TestSuspendForShutdownPendingCreation(t *testing.T) {
	for _, state := range []State{StateCreating, StateStartPending, StateFailedCreate} {
		for _, stopFails := range []bool{false, true} {
			t.Run(string(state)+"/stopFails="+strconv.FormatBool(stopFails), func(t *testing.T) {
				store := beads.NewMemStore()
				sp := runtime.NewFake()
				m := NewManagerWithOptions(store, sp)
				b, err := store.Create(beads.Bead{Type: BeadType, Metadata: map[string]string{
					"state": string(state), "session_name": "pending-runtime", "pending_create_claim": "true", "pending_create_started_at": "2026-11-01T00:00:00Z",
				}})
				if err != nil {
					t.Fatal(err)
				}
				if err := sp.Start(context.Background(), "pending-runtime", runtime.Config{}); err != nil {
					t.Fatal(err)
				}
				failure := errors.New("stop denied")
				if stopFails {
					sp.StopErrors = map[string]error{"pending-runtime": errors.Join(runtime.ErrSessionNotFound, failure)}
				}
				err = m.SuspendForShutdown(b.ID)
				if stopFails {
					if !errors.Is(err, failure) || getState(t, m, b.ID) != state {
						t.Fatalf("failed teardown: err=%v state=%s", err, getState(t, m, b.ID))
					}
					return
				}
				after, readErr := store.Get(b.ID)
				if err != nil || readErr != nil || sp.IsRunning("pending-runtime") || getState(t, m, b.ID) != StateFailedCreate {
					t.Fatalf("shutdown: err=%v read=%v alive=%v state=%s", err, readErr, sp.IsRunning("pending-runtime"), getState(t, m, b.ID))
				}
				if state != StateFailedCreate && (after.Metadata["pending_create_claim"] != "" || after.Metadata["pending_create_started_at"] != "") {
					t.Fatalf("pending creation markers survived: %v", after.Metadata)
				}
			})
		}
	}
}

func TestSuspendForShutdownTerminalRuntime(t *testing.T) {
	for _, state := range []State{StateDrained, StateArchived, StateClosed} {
		for _, stopFails := range []bool{false, true} {
			t.Run(string(state)+"/stopFails="+strconv.FormatBool(stopFails), func(t *testing.T) {
				store := beads.NewMemStore()
				sp := runtime.NewFake()
				m := NewManagerWithOptions(store, sp)
				b, err := store.Create(beads.Bead{Type: BeadType, Metadata: map[string]string{"state": string(state), "session_name": "terminal-runtime"}})
				if err != nil {
					t.Fatal(err)
				}
				if state == StateClosed {
					if err := store.Close(b.ID); err != nil {
						t.Fatal(err)
					}
				}
				before, err := store.Get(b.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := sp.Start(context.Background(), "terminal-runtime", runtime.Config{}); err != nil {
					t.Fatal(err)
				}
				failure := errors.New("deletion denied")
				if stopFails {
					sp.StopErrors = map[string]error{"terminal-runtime": failure}
				}
				err = m.SuspendForShutdown(b.ID)
				if stopFails {
					if !errors.Is(err, failure) || !sp.IsRunning("terminal-runtime") {
						t.Fatalf("failed teardown: %v", err)
					}
				} else if err != nil || sp.IsRunning("terminal-runtime") {
					t.Fatalf("terminal runtime survived: %v", err)
				}
				after, readErr := store.Get(b.ID)
				if readErr != nil || after.Status != before.Status || after.Metadata["state"] != before.Metadata["state"] {
					t.Fatalf("terminal record changed: before=%v after=%v err=%v", before, after, readErr)
				}
			})
		}
	}
}

func TestConformance_QuarantineReactivation(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	m := NewManagerWithOptions(store, sp)

	id := createTestSession(t, m, "crasher")

	// Quarantine the session.
	until := time.Now().Add(5 * time.Minute)
	if err := m.Quarantine(id, until, 3); err != nil {
		t.Fatal(err)
	}

	// Reactivate.
	if err := m.Reactivate(id); err != nil {
		t.Fatal(err)
	}
	if s := getState(t, m, id); s != StateAsleep {
		t.Errorf("state = %q, want %q after quarantine reactivation", s, StateAsleep)
	}
	b, _ := store.Get(id)

	// quarantine_cycle should be preserved (for eviction tracking).
	if b.Metadata["quarantine_cycle"] != "3" {
		t.Errorf("quarantine_cycle = %q, want 3 (should be preserved)", b.Metadata["quarantine_cycle"])
	}
	// crash_count should be reset.
	if b.Metadata["crash_count"] != "0" {
		t.Errorf("crash_count = %q, want 0", b.Metadata["crash_count"])
	}
	// quarantined_until should be cleared.
	if b.Metadata["quarantined_until"] != "" {
		t.Error("quarantined_until should be cleared on reactivation")
	}
	// Quarantined non-terminal sessions remain continuity eligible by default.
	if b.Metadata["continuity_eligible"] != "true" {
		t.Errorf("continuity_eligible = %q, want true", b.Metadata["continuity_eligible"])
	}
}

func TestCanonicalLifecycleState(t *testing.T) {
	cases := []struct {
		name string
		in   State
		want State
	}{
		{"empty legacy state normalizes to active", StateNone, StateActive},
		{"awake alias normalizes to active", StateAwake, StateActive},
		{"active is unchanged", StateActive, StateActive},
		{"asleep is unchanged", StateAsleep, StateAsleep},
		{"suspended is unchanged", StateSuspended, StateSuspended},
		{"failed-create is unchanged", StateFailedCreate, StateFailedCreate},
		{"drained is not remapped here", State("drained"), State("drained")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonicalLifecycleState(tc.in); got != tc.want {
				t.Errorf("canonicalLifecycleState(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
