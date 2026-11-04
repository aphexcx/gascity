package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
	"github.com/gastownhall/gascity/internal/session"
)

type stopOwnershipBatchProvider struct {
	runtime.Provider
	env     map[string]string
	err     error
	listErr error
}

func TestStopLifecycleRefusalFailsCommand(t *testing.T) {
	for _, state := range []string{"invalid-state"} {
		t.Run(state, func(t *testing.T) {
			store := beads.NewMemStore()
			if _, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{
				"session_name": "runtime", "state": state,
			}}); err != nil {
				t.Fatal(err)
			}
			sp := runtime.NewFake()
			startStopTestSession(t, sp, "runtime", nil)
			var stdout, stderr lockedBuffer
			code := doStop([]string{"runtime"}, sp, nil, store, 0, events.Discard, &stdout, &stderr)
			if code != 1 || strings.Contains(stdout.String(), "City stopped.") || !strings.Contains(stderr.String(), "stopping runtime:") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestStopGracefulPoolPreservesCompletedStop(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{Type: "session", Labels: []string{"gc:session"}, Metadata: map[string]string{
		"session_name": "worker-runtime", "template": "worker", "state": "active", "sleep_reason": string(session.SleepReasonCityStop), "pool_slot": "1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	startStopTestSession(t, sp, "worker-runtime", nil)
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}}}
	var stdout, stderr lockedBuffer
	code := doStop([]string{"worker-runtime"}, sp, cfg, store, time.Second, events.Discard, &stdout, &stderr)
	after, err := store.Get(b.ID)
	t.Logf("code=%d state=%s stops=%d stdout=%q stderr=%q", code, after.Metadata["state"], sp.CountCalls("Stop", "worker-runtime"), stdout.String(), stderr.String())
	if code != 0 || err != nil || sp.IsRunning("worker-runtime") || sp.CountCalls("Stop", "worker-runtime") != 1 || after.Metadata["state"] != "asleep" {
		t.Fatal("graceful pool shutdown did not preserve its completed stop")
	}
}

type shutdownBlockedProvider struct {
	*runtime.Fake
	release         <-chan struct{}
	exitOnInterrupt bool
}

func TestStopTerminalRecordWithCityStopMarker(t *testing.T) {
	for _, state := range []string{"drained", "archived", "start-pending"} {
		t.Run(state, func(t *testing.T) {
			store := beads.NewMemStore()
			b, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{
				"session_name": "held-runtime", "state": state, "sleep_reason": string(session.SleepReasonCityStop),
			}})
			if err != nil {
				t.Fatal(err)
			}
			sp := runtime.NewFake()
			startStopTestSession(t, sp, "held-runtime", nil)
			var stdout, stderr lockedBuffer
			code := doStop([]string{"held-runtime"}, sp, nil, store, 0, events.Discard, &stdout, &stderr)
			after, err := store.Get(b.ID)
			want := state
			if state == "start-pending" {
				want = "failed-create"
			}
			if code != 0 || err != nil || sp.IsRunning("held-runtime") || after.Metadata["state"] != want {
				t.Fatalf("code=%d state=%s want=%s stdout=%q stderr=%q", code, after.Metadata["state"], want, stdout.String(), stderr.String())
			}
		})
	}
}

func (p shutdownBlockedProvider) Stop(name string) error {
	<-p.release
	return p.Fake.Stop(name)
}

func (p shutdownBlockedProvider) Interrupt(name string) error {
	if p.exitOnInterrupt {
		return p.Fake.Stop(name)
	}
	return p.Fake.Interrupt(name)
}

func TestStopLifecycleTimeoutFailsCommand(t *testing.T) {
	for _, grace := range []time.Duration{0, time.Second} {
		t.Run(grace.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				defer close(release)
				sp := shutdownBlockedProvider{Fake: runtime.NewFake(), release: release, exitOnInterrupt: grace > 0}
				startStopTestSession(t, sp.Fake, "runtime", nil)
				var stdout, stderr lockedBuffer
				code := doStop([]string{"runtime"}, sp, nil, nil, grace, events.Discard, &stdout, &stderr)
				if code != 1 || strings.Contains(stdout.String(), "City stopped.") || !strings.Contains(stderr.String(), "did not return within") {
					t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
			})
		})
	}
}

func TestStopExitedPendingCreationSettlesRecord(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{
		"session_name": "pending-runtime", "state": "creating", "pending_create_claim": "true",
	}})
	if err != nil {
		t.Fatal(err)
	}
	sp := newExitedArtifactAfterInterruptProvider()
	if err := sp.Start(context.Background(), "pending-runtime", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr lockedBuffer
	code := doStop([]string{"pending-runtime"}, sp, nil, store, time.Nanosecond, events.Discard, &stdout, &stderr)
	after, err := store.Get(b.ID)
	if code != 0 || err != nil || after.Metadata["state"] != "failed-create" {
		t.Fatalf("code=%d state=%s err=%v stdout=%q stderr=%q", code, after.Metadata["state"], err, stdout.String(), stderr.String())
	}
}

func (p stopOwnershipBatchProvider) GetAllEnvironment(string) (map[string]string, error) {
	return p.env, p.err
}

func (p stopOwnershipBatchProvider) ListRunning(prefix string) ([]string, error) {
	if p.listErr != nil {
		return nil, p.listErr
	}
	return p.Provider.ListRunning(prefix)
}

func TestManualStopAcceptsConfirmedAbsenceDespiteListFailure(t *testing.T) {
	for _, mode := range []string{"single", "composite clear", "foreign backend", "unreadable backend", "mixed absence"} {
		t.Run(mode, func(t *testing.T) {
			store := beads.NewMemStore()
			b, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{"session_origin": "manual", "session_name": "manual-runtime", "state": "suspended"}})
			if err != nil {
				t.Fatal(err)
			}
			owner := runtime.NewFake()
			startStopTestSession(t, owner, "manual-runtime", nil)
			source := &stopOwnershipBatchProvider{Provider: owner, env: map[string]string{"GC_CITY_PATH": "/cities/test", "GC_SESSION_ID": b.ID}}
			other := &stopOwnershipBatchProvider{Provider: runtime.NewFake()}
			var all runtime.Provider = source
			if mode != "single" {
				all = hybrid.New(source, other, func(string) bool { return false })
			}
			targets, err := resolveManualStopSessionTargets(store, &config.City{}, "/cities/test", all)
			if err != nil || len(targets) != 1 {
				t.Fatalf("targets=%v error=%v", targets, err)
			}
			sp := providerForManualStopTargets(all, targets)
			source.err = runtime.ErrSessionNotFound
			source.listErr = errors.New("server unavailable for listing")
			switch mode {
			case "foreign backend":
				startStopTestSession(t, other.Provider.(*runtime.Fake), "manual-runtime", nil)
			case "unreadable backend":
				other.listErr = errors.New("other backend unavailable")
			case "mixed absence":
				source.err = errors.Join(runtime.ErrSessionNotFound, errors.New("ownership unavailable"))
			}
			wantErr := mode == "foreign backend" || mode == "unreadable backend" || mode == "mixed absence"
			if err := sp.Stop("manual-runtime"); (err != nil) != wantErr {
				t.Fatalf("confirmed absence error=%v, want error=%v", err, wantErr)
			}
			if owner.CountCalls("Stop", "manual-runtime") != 0 {
				t.Fatal("confirmed absence must not dispatch a stop by name")
			}
		})
	}
}

type stopHostOwnershipProvider struct {
	runtime.Provider
	err error
}

type stopBindingErrorProvider struct{ *runtime.Fake }

func (*stopBindingErrorProvider) BindForShutdown(string, string, string) (runtime.Provider, error) {
	return nil, errors.New("native identity changed")
}

func TestManualStopRevalidatesWithAbsentBackend(t *testing.T) {
	for _, missing := range []string{"sibling", "owner"} {
		t.Run(missing, func(t *testing.T) {
			store := beads.NewMemStore()
			b, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{"session_origin": "manual", "session_name": "manual-runtime", "state": "active"}})
			if err != nil {
				t.Fatal(err)
			}
			live := runtime.NewFake()
			startStopTestSession(t, live, "manual-runtime", nil)
			owner := &stopOwnershipBatchProvider{Provider: live, env: map[string]string{"GC_CITY_PATH": "/cities/test", "GC_SESSION_ID": b.ID}}
			sibling := &stopOwnershipBatchProvider{Provider: runtime.NewFake()}
			all := auto.New(owner, sibling)
			targets, err := resolveManualStopSessionTargets(store, &config.City{}, "/cities/test", all)
			if err != nil || len(targets) != 1 {
				t.Fatalf("targets=%v error=%v", targets, err)
			}
			gone := sibling
			if missing == "owner" {
				gone = owner
			}
			gone.listErr = &runtime.PartialListError{Err: errors.New("no tmux server running"), ServerAbsent: true}
			gone.err = runtime.ErrSessionNotFound
			sp := providerForManualStopTargets(all, targets)
			if err := sp.Stop("manual-runtime"); err != nil {
				t.Fatalf("confirmed %s absence blocked shutdown: %v", missing, err)
			}
			wantStops := 1
			if missing == "owner" {
				wantStops = 0
			}
			if got := live.CountCalls("Stop", "manual-runtime"); got != wantStops {
				t.Fatalf("stops=%d, want %d", got, wantStops)
			}
		})
	}
}

func TestManualStopReportsNativeBindingFailure(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{"session_origin": "manual", "session_name": "manual-runtime", "state": "suspended"}})
	if err != nil {
		t.Fatal(err)
	}
	source := &stopBindingErrorProvider{Fake: runtime.NewFake()}
	startStopTestSession(t, source.Fake, "manual-runtime", map[string]string{"GC_CITY_PATH": "/cities/test", "GC_SESSION_ID": b.ID})
	var stdout, stderr lockedBuffer
	cfg := &config.City{}
	names, _, targets, err := resolveStopSessionNames(store, cfg, "/cities/test", "test", source, &stderr)
	if err == nil {
		t.Fatal("native identity failure must remain visible to resolution")
	}
	sp := providerForManualStopTargets(source, targets)
	code := doStopWithoutSuccess(names, sp, cfg, store, 0, events.Discard, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "manual-runtime") || strings.Contains(stdout.String(), "Stopped agent") || source.CountCalls("Stop", "manual-runtime") != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q; want named preservation after failed binding", code, stdout.String(), stderr.String())
	}
}

func (p stopHostOwnershipProvider) SessionOwnership(string) (string, string, error) {
	return "/cities/test", "session-1", p.err
}

type manualStopErrorProvider struct {
	*runtime.Fake
	err error
}

func (p manualStopErrorProvider) Stop(name string) error {
	_ = p.Fake.Stop(name)
	return p.err
}

func (p manualStopErrorProvider) Interrupt(name string) error {
	_ = p.Fake.Interrupt(name)
	return p.err
}

func (p manualStopErrorProvider) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) { //nolint:unparam // required runtime.LivenessObserverWithError signature
	return runtime.Liveness{}, p.err
}

func TestManualStopProviderPreservesOwnersAndErrors(t *testing.T) {
	wantErr := errors.New("backend unavailable")
	base := runtime.NewFake()
	first := manualStopErrorProvider{Fake: runtime.NewFake(), err: errors.Join(wantErr, runtime.ErrSessionNotFound)}
	second := runtime.NewFake()
	startStopTestSession(t, first.Fake, "manual", nil)
	startStopTestSession(t, second, "manual", nil)
	startStopTestSession(t, base, "configured", nil)
	sp := providerForManualStopTargets(base, []runtime.BackendListResult{
		{Label: "first", Provider: first, Names: []string{"manual"}},
		{Label: "second", Provider: second, Names: []string{"manual"}},
	})
	obs, err := runtime.ObserveLivenessWithError(sp, "manual", nil)
	if !obs.Running || !errors.Is(err, wantErr) || !sp.IsRunning("manual") {
		t.Fatalf("observation=%+v err=%v; want live copy and retained backend error", obs, err)
	}
	if err := sp.Interrupt("manual"); !errors.Is(err, wantErr) {
		t.Fatalf("Interrupt error=%v, want backend error", err)
	}
	if err := sp.Stop("manual"); !errors.Is(err, wantErr) {
		t.Fatalf("Stop error=%v, want backend error", err)
	}
	for _, owner := range []*runtime.Fake{first.Fake, second} {
		if owner.CountCalls("Interrupt", "manual") != 1 || owner.CountCalls("Stop", "manual") != 1 {
			t.Fatal("each verified copy must be interrupted and stopped even when another fails")
		}
	}
	if base.CountCalls("Stop", "manual") != 0 || sp.IsRunning("manual") {
		t.Fatal("manual shutdown must not fall back to an unverified backend")
	}
	if err := sp.Interrupt("configured"); err != nil {
		t.Fatal(err)
	}
	if err := sp.Stop("configured"); err != nil {
		t.Fatal(err)
	}
	if base.CountCalls("Stop", "configured") != 1 || base.CountCalls("Interrupt", "configured") != 1 {
		t.Fatal("configured targets must retain their original routing")
	}
}

func TestManualStopWaitsRetainErrorsAndTimeout(t *testing.T) {
	sp := runtime.NewFake()
	wantErr := errors.New("confirmation unavailable")
	sp.WaitForIdleErrors["manual"] = wantErr
	sp.InterruptBoundaryErrors["manual"] = wantErr
	unsupported := struct{ runtime.Provider }{runtime.NewFake()}
	provider := providerForManualStopTargets(runtime.NewFake(), []runtime.BackendListResult{
		{Label: "unsupported", Provider: unsupported, Names: []string{"manual"}},
		{Label: "owner", Provider: sp, Names: []string{"manual"}},
	})
	idle := provider.(runtime.IdleWaitProvider)
	boundary := provider.(runtime.InterruptBoundaryWaitProvider)
	if err := idle.WaitForIdle(context.Background(), "manual", time.Second); !errors.Is(err, wantErr) || errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("idle error=%v, want retained confirmation failure", err)
	}
	if err := boundary.WaitForInterruptBoundary(context.Background(), "manual", time.Now(), time.Second); !errors.Is(err, wantErr) || errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("boundary error=%v, want retained confirmation failure", err)
	}
	sp.WaitForIdleGates["manual"] = make(chan struct{})
	if err := idle.WaitForIdle(context.Background(), "manual", time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("idle error=%v, want shared timeout", err)
	}
	noWait := providerForManualStopTargets(unsupported, []runtime.BackendListResult{{Provider: unsupported, Names: []string{"manual"}}})
	if err := noWait.(runtime.IdleWaitProvider).WaitForIdle(context.Background(), "manual", time.Second); !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("idle error=%v, want unsupported", err)
	}
}

func TestManualStopUsesProviderHostOwnership(t *testing.T) {
	for _, fail := range []bool{false, true} {
		sp := runtime.NewFake()
		startStopTestSession(t, sp, "test-session", map[string]string{"GC_CITY_PATH": "/workspace"})
		provider := stopHostOwnershipProvider{Provider: sp}
		if fail {
			provider.err = errors.New("ownership unavailable")
		}
		owners, err := manualStopSessionBackends(session.Info{ID: "session-1", SessionName: "test-session"}, "/cities/test", []runtime.BackendListResult{{Provider: provider, Names: []string{"test-session"}}})
		if (len(owners) == 1) == fail || (err != nil) != fail {
			t.Fatalf("owners=%v err=%v fail=%v", owners, err, fail)
		}
	}
}

func TestManualStopSessionOwnership(t *testing.T) {
	info := session.Info{ID: "session-1", SessionName: "custom-runtime"}
	for _, tc := range []struct {
		name    string
		city    string
		id      string
		err     error
		want    bool
		wantErr bool
	}{
		{name: "owned", city: "/cities/test", id: info.ID, want: true},
		{name: "normalized city", city: "/cities/test/.", id: info.ID, want: true},
		{name: "foreign city", city: "/cities/foreign", id: info.ID},
		{name: "reused by same city", city: "/cities/test", id: "session-2"},
		{name: "missing city", id: info.ID},
		{name: "missing session id", city: "/cities/test"},
		{name: "listed but now absent", err: runtime.ErrSessionNotFound},
		{name: "absence joined with unreadable ownership", err: errors.Join(runtime.ErrSessionNotFound, errors.New("ownership unavailable")), wantErr: true},
		{name: "unreadable batch", city: "/cities/test", id: info.ID, err: errors.New("ownership unavailable"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := stopOwnershipBatchProvider{Provider: runtime.NewFake(), env: map[string]string{"GC_CITY_PATH": tc.city, "GC_SESSION_ID": tc.id}, err: tc.err}
			owners, err := manualStopSessionBackends(info, "/cities/test", []runtime.BackendListResult{{Provider: provider, Names: []string{info.SessionName}}})
			owned := len(owners) > 0
			if owned != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("owned=%v err=%v, want owned=%v error=%v", owned, err, tc.want, tc.wantErr)
			}
		})
	}

	t.Run("conflicting backend", func(t *testing.T) {
		owned := stopOwnershipBatchProvider{Provider: runtime.NewFake(), env: map[string]string{"GC_CITY_PATH": "/cities/test", "GC_SESSION_ID": info.ID}}
		foreign := stopOwnershipBatchProvider{Provider: runtime.NewFake(), env: map[string]string{"GC_CITY_PATH": "/cities/foreign", "GC_SESSION_ID": "session-2"}}
		got, err := manualStopSessionBackends(info, "/cities/test", []runtime.BackendListResult{
			{Provider: owned, Names: []string{info.SessionName}},
			{Provider: foreign, Names: []string{info.SessionName}},
		})
		if len(got) != 0 || err == nil {
			t.Fatalf("owned=%v err=%v; a conflicting runtime must veto the target", got, err)
		}
	})
}

func TestResolveManualStopSessionsRetainsListingFailure(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{"session_origin": "manual", "session_name": "custom-runtime"}}); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("runtime listing unavailable")
	sp := scopedStopOrphanProvider{Provider: runtime.NewFake(), listErr: wantErr}
	names, err := resolveManualStopSessionTargets(store, &config.City{}, "/cities/test", sp)
	if len(names) != 0 || !errors.Is(err, wantErr) {
		t.Fatalf("names=%v err=%v, want no targets and listing error", names, err)
	}
}

func TestManualStopRevalidatesOwnershipBeforeMutation(t *testing.T) {
	for _, tc := range []struct{ operation, change string }{
		{"Stop", "foreign"},
		{"Interrupt", "foreign"},
		{"SendKeys", "foreign"},
		{"Stop", "missing"},
		{"Stop", "unreadable"},
		{"Stop", "different session"},
	} {
		t.Run(tc.operation+"/"+tc.change, func(t *testing.T) {
			operation := tc.operation
			store := beads.NewMemStore()
			b, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{
				"session_origin": "manual", "session_name": "manual-runtime", "state": "suspended",
			}})
			if err != nil {
				t.Fatal(err)
			}
			owner := runtime.NewFake()
			startStopTestSession(t, owner, "manual-runtime", map[string]string{"GC_CITY_PATH": "/cities/test", "GC_SESSION_ID": b.ID})
			source := &stopOwnershipBatchProvider{Provider: owner, env: map[string]string{"GC_CITY_PATH": "/cities/test", "GC_SESSION_ID": b.ID}}
			targets, err := resolveManualStopSessionTargets(store, &config.City{}, "/cities/test", source)
			if err != nil || len(targets) != 1 {
				t.Fatalf("targets=%v error=%v", targets, err)
			}
			sp := providerForManualStopTargets(source, targets)
			switch tc.change {
			case "foreign":
				source.env["GC_CITY_PATH"] = "/cities/foreign"
			case "missing":
				delete(source.env, "GC_SESSION_ID")
			case "unreadable":
				source.err = errors.New("metadata not found")
			case "different session":
				source.env["GC_SESSION_ID"] = "replacement"
			}
			switch operation {
			case "Stop":
				err = sp.Stop("manual-runtime")
			case "Interrupt":
				err = sp.Interrupt("manual-runtime")
			case "SendKeys":
				err = sp.SendKeys("manual-runtime", "Escape")
			}
			if err == nil || !strings.Contains(err.Error(), "manual-runtime") || owner.CountCalls(operation, "manual-runtime") != 0 || !owner.IsRunning("manual-runtime") {
				t.Fatalf("foreign replacement must survive %s: error=%v calls=%d running=%v", operation, err, owner.CountCalls(operation, "manual-runtime"), owner.IsRunning("manual-runtime"))
			}
		})
	}
}

func TestManualStopReportsOwnershipChangeDuringGrace(t *testing.T) {
	for _, mode := range []string{"new foreign backend", "two owned backends", "new pending backend"} {
		t.Run(mode, func(t *testing.T) {
			ambiguousAtDiscovery := mode == "two owned backends"
			store := beads.NewMemStore()
			b, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{
				"session_origin": "manual", "session_name": "manual-runtime", "state": "suspended",
			}})
			if err != nil {
				t.Fatal(err)
			}
			owner, other := runtime.NewFake(), runtime.NewFake()
			startStopTestSession(t, owner, "manual-runtime", map[string]string{"GC_CITY_PATH": "/cities/test", "GC_SESSION_ID": b.ID})
			if ambiguousAtDiscovery {
				startStopTestSession(t, other, "manual-runtime", map[string]string{"GC_CITY_PATH": "/cities/test", "GC_SESSION_ID": b.ID})
			}
			var otherProvider runtime.Provider = other
			if mode == "new pending backend" {
				otherProvider = &stopUnlistedRuntimeProvider{Fake: other, hidden: true}
			}
			source := hybrid.New(owner, otherProvider, func(string) bool { return false })
			cfg := &config.City{}
			targets, err := resolveManualStopSessionTargets(store, cfg, "/cities/test", source)
			if err != nil {
				t.Fatal(err)
			}
			sp := providerForManualStopTargets(source, targets)
			if err := sp.Interrupt("manual-runtime"); (err != nil) != ambiguousAtDiscovery {
				t.Fatalf("interrupt error=%v, ambiguous=%v", err, ambiguousAtDiscovery)
			}
			// A newly visible backend must veto the discovery-time ownership, too.
			if !ambiguousAtDiscovery {
				startStopTestSession(t, other, "manual-runtime", map[string]string{"GC_CITY_PATH": "/cities/foreign", "GC_SESSION_ID": "foreign"})
			}
			var stdout, stderr lockedBuffer
			rec := events.NewFake()
			code := doStopWithoutSuccess([]string{"manual-runtime"}, sp, cfg, store, 0, rec, &stdout, &stderr)
			if code == 0 || !strings.Contains(stderr.String(), "manual-runtime") || strings.Contains(stdout.String(), "Stopped agent") {
				t.Errorf("code=%d stdout=%q stderr=%q; want named preservation and failure", code, stdout.String(), stderr.String())
			}
			if !owner.IsRunning("manual-runtime") || !other.IsRunning("manual-runtime") {
				t.Error("ambiguous runtime copies must both survive")
			}
			for _, event := range rec.Events {
				if event.Type == events.SessionStopped {
					t.Error("preserved runtime emitted SessionStopped")
				}
			}
		})
	}
}

type stopFalseLivenessProvider struct{ *runtime.Fake }

func (*stopFalseLivenessProvider) IsRunning(string) bool { return false }

type stopUnlistedRuntimeProvider struct {
	*runtime.Fake
	hidden bool
}

func (p *stopUnlistedRuntimeProvider) SessionOwnership(name string) (string, string, error) {
	if !p.Fake.IsRunning(name) {
		return "", "", runtime.ErrSessionNotFound
	}
	city, err := p.GetMeta(name, "GC_CITY_PATH")
	if err != nil {
		return "", "", err
	}
	id, err := p.GetMeta(name, "GC_SESSION_ID")
	return city, id, err
}

func (p *stopUnlistedRuntimeProvider) IsRunning(name string) bool {
	return !p.hidden && p.Fake.IsRunning(name)
}

func (p *stopUnlistedRuntimeProvider) ListRunning(prefix string) ([]string, error) {
	if p.hidden {
		return nil, nil
	}
	return p.Fake.ListRunning(prefix)
}

func TestManualStopDoesNotInferAbsenceFromFailedLiveness(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{"session_origin": "manual", "session_name": "manual-runtime", "state": "suspended"}})
	if err != nil {
		t.Fatal(err)
	}
	source := &stopUnlistedRuntimeProvider{Fake: runtime.NewFake()}
	startStopTestSession(t, source.Fake, "manual-runtime", map[string]string{"GC_CITY_PATH": "/cities/test", "GC_SESSION_ID": b.ID})
	targets, err := resolveManualStopSessionTargets(store, &config.City{}, "/cities/test", source)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets=%v error=%v", targets, err)
	}
	sp := providerForManualStopTargets(source, targets)
	source.hidden = true
	wantErr := errors.New("deletion failed")
	source.StopErrors["manual-runtime"] = wantErr
	if err := sp.Stop("manual-runtime"); !errors.Is(err, wantErr) {
		t.Fatalf("Stop error=%v, want deletion failure for owned but unlisted artifact", err)
	}
}

func TestStopSuspendedRuntimeRetainsFailedDeletion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		absent bool
	}{
		{"failed", errors.New("deletion refused"), false},
		{"absent", runtime.ErrSessionNotFound, true},
		{"mixed", errors.Join(errors.New("deletion refused"), runtime.ErrSessionNotFound), false},
		{"all absent", errors.Join(runtime.ErrSessionNotFound, runtime.ErrSessionNotFound), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			b, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{
				"session_origin": "manual", "session_name": "manual-runtime", "state": "suspended",
			}})
			if err != nil {
				t.Fatal(err)
			}
			sp := &stopFalseLivenessProvider{runtime.NewFake()}
			startStopTestSession(t, sp.Fake, "manual-runtime", nil)
			sp.StopErrors["manual-runtime"] = tc.err
			var stdout, stderr lockedBuffer
			rec := events.NewFake()
			doStopWithoutSuccess([]string{"manual-runtime"}, sp, &config.City{}, store, 0, rec, &stdout, &stderr)
			absent := tc.absent
			if strings.Contains(stdout.String(), "Stopped agent") != absent || strings.Contains(stderr.String(), "deletion refused") == absent {
				t.Errorf("stdout=%q stderr=%q, absent=%v", stdout.String(), stderr.String(), absent)
			}
			stopped := 0
			for _, event := range rec.Events {
				if event.Type == events.SessionStopped {
					stopped++
				}
			}
			if (stopped == 1) != absent {
				t.Errorf("SessionStopped events=%d, absent=%v", stopped, absent)
			}
			after, err := store.Get(b.ID)
			if err != nil || after.Metadata["state"] != "suspended" {
				t.Fatalf("state changed: %+v, error=%v", after, err)
			}
		})
	}
}
