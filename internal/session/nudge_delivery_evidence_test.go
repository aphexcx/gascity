package session

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// landedThenErroredProvider is a vouching runtime whose send fails AFTER the
// paste landed and says so — evidence and error together, on both the
// default and the immediate route, as runtime.NudgeVouchingProvider and
// runtime.ImmediateNudgeVouchingProvider permit.
type landedThenErroredProvider struct {
	*runtime.Fake
}

func (p *landedThenErroredProvider) NudgeDelivered(name string, content []runtime.ContentBlock) (runtime.NudgeDelivery, error) {
	if err := p.Nudge(name, content); err != nil {
		return runtime.NudgeDelivery{}, err
	}
	return runtime.NudgeDelivery{Delivered: true, Bytes: len(runtime.FlattenText(content)), Submit: runtime.NudgeSubmitUnconfirmed},
		errors.New("submit sequence refused after the paste")
}

func (p *landedThenErroredProvider) NudgeNowDelivered(name string, content []runtime.ContentBlock) (runtime.NudgeDelivery, error) {
	if err := p.NudgeNow(name, content); err != nil {
		return runtime.NudgeDelivery{}, err
	}
	return runtime.NudgeDelivery{Delivered: true, Bytes: len(runtime.FlattenText(content)), Submit: runtime.NudgeSubmitUnconfirmed},
		errors.New("hidden-attach client closed after the paste")
}

// TestManagerSendKeepsEvidenceAlongsideError pins the manager's half of the
// pass-through contract (gp-2io, gate rounds 2 and 3): none of the send
// routes may zero a runtime's landed evidence because the send also errored.
// The default and immediate routes return both to the worker handle above;
// the wait-idle routes keep their documented contract (a send error means
// "not live, fall back to the queue": nil error) but still carry the
// evidence, so a caller holds on a landed payload instead of queueing a
// duplicate. A manager that returned an empty delivery on any of these would
// let the inbound receipt classify a whole live copy as failed/0 and license
// a clean re-post.
func TestManagerSendKeepsEvidenceAlongsideError(t *testing.T) {
	store := beads.NewMemStore()
	sp := &landedThenErroredProvider{Fake: runtime.NewFake()}
	mgr := NewManagerWithOptions(store, sp)

	dir := t.TempDir()
	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Template: "worker", Title: "Worker", Command: "claude", WorkDir: dir, Provider: "claude",
		Hints: runtime.Config{WorkDir: dir},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The wait-idle routes probe for an idle boundary first; the Fake reports
	// that probe unsupported unless armed.
	sp.WaitForIdleErrors[info.SessionName] = nil

	const message = "landed then failed"
	hints := runtime.Config{WorkDir: dir}
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		send     func() (runtime.NudgeDelivery, error)
		wantErr  bool // the wait-idle routes swallow send errors by contract
		wantSize int  // the wait-idle routes wrap the message in a reminder
	}{
		{"Send", func() (runtime.NudgeDelivery, error) { return mgr.Send(ctx, info.ID, message, "", hints) }, true, len(message)},
		{"SendLiveOnly", func() (runtime.NudgeDelivery, error) { return mgr.SendLiveOnly(ctx, info.ID, message) }, true, len(message)},
		{"SendImmediate", func() (runtime.NudgeDelivery, error) { return mgr.SendImmediate(ctx, info.ID, message, "", hints) }, true, len(message)},
		{"SendImmediateLiveOnly", func() (runtime.NudgeDelivery, error) { return mgr.SendImmediateLiveOnly(ctx, info.ID, message) }, true, len(message)},
		{"TryWaitIdleNudge", func() (runtime.NudgeDelivery, error) {
			return mgr.TryWaitIdleNudge(ctx, info.ID, "mail", message, "", hints)
		}, false, len(formatWaitIdleReminder("mail", message))},
		{"TryWaitIdleNudgeLiveOnly", func() (runtime.NudgeDelivery, error) {
			return mgr.TryWaitIdleNudgeLiveOnly(ctx, info.ID, "mail", message)
		}, false, len(formatWaitIdleReminder("mail", message))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delivery, err := tc.send()
			if tc.wantErr && err == nil {
				t.Fatalf("%s error = nil, want the runtime's error passed through", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("%s error = %v, want nil: a wait-idle send error downgrades to the queue by contract", tc.name, err)
			}
			if !delivery.Landed() || delivery.Bytes != tc.wantSize || delivery.Submit != runtime.NudgeSubmitUnconfirmed {
				t.Fatalf("%s delivery = %+v, want the runtime's evidence intact (Bytes=%d, unconfirmed) whatever the error", tc.name, delivery, tc.wantSize)
			}
		})
	}
}

// unconfirmedEverywhereProvider vouches every route's payload as landed
// whole with an unconfirmed submit — the incident evidence, on both the
// default and the immediate route.
type unconfirmedEverywhereProvider struct {
	*runtime.Fake
}

func (p *unconfirmedEverywhereProvider) evidence(content []runtime.ContentBlock) runtime.NudgeDelivery {
	return runtime.NudgeDelivery{Delivered: true, Bytes: len(runtime.FlattenText(content)), Submit: runtime.NudgeSubmitUnconfirmed}
}

func (p *unconfirmedEverywhereProvider) NudgeDelivered(name string, content []runtime.ContentBlock) (runtime.NudgeDelivery, error) {
	if err := p.Nudge(name, content); err != nil {
		return runtime.NudgeDelivery{}, err
	}
	return p.evidence(content), nil
}

func (p *unconfirmedEverywhereProvider) NudgeNowDelivered(name string, content []runtime.ContentBlock) (runtime.NudgeDelivery, error) {
	if err := p.NudgeNow(name, content); err != nil {
		return runtime.NudgeDelivery{}, err
	}
	return p.evidence(content), nil
}

// TestManagerSubmitReportsUnconfirmedSubmitAsError pins the semantic submit
// boundary (gp-2io, gate round 5): Manager.Submit reports success or
// failure, not evidence, and before the evidence path existed an unconfirmed
// submit reached it as ErrNudgeSubmitUnconfirmed. It still must — a nil
// error here emits session.submit.succeeded for a turn that may be sitting
// drafted in the pane.
func TestManagerSubmitReportsUnconfirmedSubmitAsError(t *testing.T) {
	for name, provider := range map[string]string{
		"default delivery (claude)":  "claude",
		"immediate delivery (codex)": "codex",
	} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			sp := &unconfirmedEverywhereProvider{Fake: runtime.NewFake()}
			mgr := NewManagerWithOptions(store, sp)

			dir := t.TempDir()
			info, err := mgr.CreateSession(context.Background(), CreateOptions{
				Template: "worker", Title: "Worker", Command: provider, WorkDir: dir, Provider: provider,
				Hints: runtime.Config{WorkDir: dir},
			})
			if err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
				t.Fatalf("Start: %v", err)
			}

			outcome, err := mgr.Submit(context.Background(), info.ID, "ship it", "", runtime.Config{WorkDir: dir}, SubmitIntentDefault)
			if !errors.Is(err, runtime.ErrNudgeSubmitUnconfirmed) {
				t.Fatalf("Submit error = %v, want ErrNudgeSubmitUnconfirmed: success here claims a turn the agent was never seen taking", err)
			}
			if outcome.Queued {
				t.Fatalf("outcome = %+v, want not queued: the payload was pasted live", outcome)
			}
		})
	}
}
