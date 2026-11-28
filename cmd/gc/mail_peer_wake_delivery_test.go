package main

import (
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// A listed first wake creates the named recipient with the receiving
// controller's own provider, never with one the process builds from its
// ambient environment.
func TestPeerCityStartsRecipientOnControllerProvider(t *testing.T) {
	c := newPeerWakeCity(t, beads.NewMemStore(), []string{"alpha"},
		config.Agent{Name: "reader", Provider: "codex", StartCommand: "echo", MailWakeStart: true, MailWakeFrom: []string{"city:alpha"}},
	)
	c.cfg.NamedSessions = []config.NamedSession{{Template: "reader"}}
	startSessionResetTestController(t, c.dir)
	t.Setenv("GC_CITY", c.dir)
	t.Setenv("GC_SESSION", "absent-provider")
	var ambient atomic.Int64
	build := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		ambient.Add(1)
		return nil, errors.New("ambient provider construction refused")
	}
	t.Cleanup(func() { buildSessionProviderByName = build })

	answer := c.keyed("alpha", "reader", "alpha/mayor")
	if answer.status != http.StatusCreated || !answer.notified {
		t.Errorf("first listed wake of a never-started recipient: %+v", answer)
	}
	if n := ambient.Load(); n != 0 {
		t.Errorf("built %d ambient provider(s); the controller's own provider was supplied", n)
	}
	records, err := c.store.List(beads.ListQuery{Type: session.BeadType, TierMode: beads.TierBoth})
	if err != nil || len(records) != 1 {
		t.Fatalf("session records: %v, %v", records, err)
	}
	if meta := records[0].Metadata; meta["configured_named_identity"] != "reader" || meta["pending_create_claim"] != "true" {
		t.Errorf("created recipient: %v", meta)
	}
	if history := c.history(records[0].ID); len(history) != 1 || history[0] != "city:alpha" {
		t.Errorf("wake history %q, want one wake under city:alpha", history)
	}
}

// A queued reminder goes to the nudge store the receiving controller routes
// to, not to the one the process's one-shot command cache remembers.
func TestPeerCityQueuedReminderUsesControllerNudgeStore(t *testing.T) {
	work := beads.NewMemStore()
	c := newPeerWakeCity(t, work, []string{"alpha"},
		config.Agent{Name: "reader", Provider: "codex", StartCommand: "echo", MailWakeFrom: []string{"city:alpha"}},
	)
	reader := c.start("reader", "")
	// The controller serves split storage. The process still holds the
	// unsplit routes it resolved for this city before the restart.
	relocated := beads.NewMemStore()
	c.state.storageRoutes = &storageRoutes{stores: map[coordclass.Class]beads.Store{coordclass.ClassNudges: relocated}, binding: "infra"}
	seedCLIStorageRoutes(t, c.dir, nil)
	c.sp.NudgeErrors = map[string]error{reader.SessionName: errors.New("live reminder unavailable")}

	answer := c.keyed("alpha", reader.ID, "alpha/mayor")
	if answer.status != http.StatusCreated || !answer.notified {
		t.Fatalf("listed mail to a busy recipient: %+v", answer)
	}
	if onController, onWork := c.queued(relocated), c.queued(work); onController != 1 || onWork != 0 {
		t.Errorf("queued reminder beads: %d on the controller's nudge store, %d on the work store; want 1 and 0", onController, onWork)
	}
}

// mailWakeWriteBarrier holds each wake-history write until two requests have
// both read the history, so the two writes race for one revision.
type mailWakeWriteBarrier struct {
	*beads.MemStore
	mu      sync.Mutex
	waiting int
	release chan struct{}
}

func (s *mailWakeWriteBarrier) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	if _, ok := opts.Metadata["mail_wake_log"]; ok {
		s.mu.Lock()
		s.waiting++
		if s.waiting == 2 {
			close(s.release)
		}
		s.mu.Unlock()
		select {
		case <-s.release:
		case <-time.After(5 * time.Second):
			// Requests that never overlap have nothing to race for.
		}
	}
	return s.MemStore.UpdateIfMatch(id, expectedRevision, opts)
}

// Two listed mails that arrive together and both fit the budget both get
// their turn: the one that loses the history write tries again, and the two
// reminders are never typed over each other.
func TestPeerCityConcurrentListedSendsBothWake(t *testing.T) {
	store := &mailWakeWriteBarrier{MemStore: beads.NewMemStore(), release: make(chan struct{})}
	c := newPeerWakeCity(t, store, []string{"alpha"},
		config.Agent{Name: "reader", Provider: "claude", StartCommand: "echo", MailWakeFrom: []string{"city:alpha"}},
	)
	reader := c.start("reader", "")
	answers := make([]peerMailAnswer, 2)
	var wg sync.WaitGroup
	for i, from := range []string{"alpha/mayor", "alpha/worker"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i] = c.keyed("alpha", reader.ID, from)
		}()
	}
	wg.Wait()
	for i, answer := range answers {
		if answer.status != http.StatusCreated || !answer.notified {
			t.Errorf("send %d of two inside the budget: %+v", i+1, answer)
		}
	}
	if history := c.history(reader.ID); len(history) != 2 {
		t.Errorf("wake history %q, want two wakes", history)
	}
	typed, queued := len(c.typed(reader)), c.queued(store)
	if typed+queued != 2 || typed > 1 {
		t.Errorf("%d reminder(s) typed and %d queued; want two reminders and at most one typed while the other is in flight", typed, queued)
	}
}

// A listed reminder that overlaps a live delivery to the same recipient is
// queued with its own mail reference instead of being typed alongside it.
func TestPeerCityOverlappingReminderIsQueued(t *testing.T) {
	c := newPeerWakeCity(t, beads.NewMemStore(), []string{"alpha"},
		config.Agent{Name: "reader", Provider: "claude", StartCommand: "echo", MailWakeFrom: []string{"city:alpha"}},
	)
	reader := c.start("reader", "")
	if _, occupied := c.state.liveMailNotifies.LoadOrStore(reader.ID, struct{}{}); occupied {
		t.Fatal("live delivery slot leaked")
	}
	answer := c.keyed("alpha", reader.ID, "alpha/mayor")
	if _, held := c.state.liveMailNotifies.Load(reader.ID); !held {
		t.Error("the listed send released a delivery slot it did not hold")
	}
	c.state.liveMailNotifies.Delete(reader.ID)
	if answer.status != http.StatusCreated || !answer.notified {
		t.Fatalf("overlapping listed mail: %+v", answer)
	}
	if typed := c.typed(reader); len(typed) != 0 {
		t.Errorf("typed %q while another delivery held the recipient", typed)
	}
	items, err := c.store.List(beads.ListQuery{Label: nudgeBeadLabel, TierMode: beads.TierBoth})
	if err != nil || len(items) != 1 {
		t.Fatalf("queued reminders: %v, %v", items, err)
	}
	pending, _, _, err := listQueuedNudges(c.dir, reader.ID, time.Now())
	if err != nil || len(pending) != 1 || pending[0].Reference == nil || pending[0].Reference.ID != answer.id {
		t.Errorf("queued reminder for %q: %+v, %v", answer.id, pending, err)
	}
}
