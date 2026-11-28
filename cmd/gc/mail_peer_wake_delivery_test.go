package main

import (
	"errors"
	"fmt"
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

// mailWakeHistoryHook is a session store that lets a test act just before a
// write of the wake history, on a store with or without conditional writes,
// and just before a read of a record.
type mailWakeHistoryHook struct {
	*beads.MemStore
	beforeWrite func(id string)
	beforeGet   func(id string)
}

func (s *mailWakeHistoryHook) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	if _, ok := opts.Metadata["mail_wake_log"]; ok && !s.DisableConditionalWrites && s.beforeWrite != nil {
		s.beforeWrite(id)
	}
	return s.MemStore.UpdateIfMatch(id, expectedRevision, opts)
}

func (s *mailWakeHistoryHook) SetMetadata(id, key, value string) error {
	if key == "mail_wake_log" && s.beforeWrite != nil {
		s.beforeWrite(id)
	}
	return s.MemStore.SetMetadata(id, key, value)
}

func (s *mailWakeHistoryHook) Get(id string) (beads.Bead, error) {
	if s.beforeGet != nil {
		s.beforeGet(id)
	}
	return s.MemStore.Get(id)
}

func newMailWakeHistoryHook(conditionalWrites bool) *mailWakeHistoryHook {
	mem := beads.NewMemStore()
	mem.DisableConditionalWrites = !conditionalWrites
	return &mailWakeHistoryHook{MemStore: mem}
}

// A session record that another writer moved between the reservation's read
// and its write does not cost the mail its turn: the reservation reads again
// and retries.
func TestPeerCityWakeSurvivesMovedSessionRecord(t *testing.T) {
	store := newMailWakeHistoryHook(true)
	c := newPeerWakeCity(t, store, []string{"alpha"},
		config.Agent{Name: "reader", Provider: "claude", StartCommand: "echo", MailWakeFrom: []string{"city:alpha"}},
	)
	reader := c.start("reader", "")
	writes := 0
	store.beforeWrite = func(id string) {
		if writes++; writes == 1 {
			if err := store.MemStore.SetMetadata(id, "unrelated", "write"); err != nil {
				t.Error(err)
			}
		}
	}
	answer := c.keyed("alpha", reader.ID, "alpha/mayor")
	if answer.status != http.StatusCreated || !answer.notified {
		t.Errorf("listed mail whose first reservation lost a race: %+v", answer)
	}
	if history, typed := c.history(reader.ID), c.typed(reader); len(history) != 1 || len(typed) != 1 || writes != 2 {
		t.Errorf("history %q, %d reminder(s) typed, %d write attempt(s); want one wake after one retry", history, len(typed), writes)
	}
}

// A retry counts the budget against what the winning writer recorded, so a
// lost race never buys a fourth wake.
func TestPeerCityRetryRechecksBudget(t *testing.T) {
	store := newMailWakeHistoryHook(true)
	c := newPeerWakeCity(t, store, []string{"alpha"},
		config.Agent{Name: "reader", Provider: "claude", StartCommand: "echo", MailWakeFrom: []string{"city:alpha"}},
	)
	reader := c.start("reader", "")
	for range 2 {
		if answer := c.keyed("alpha", reader.ID, "alpha/mayor"); !answer.notified {
			t.Fatalf("wake inside the budget: %+v", answer)
		}
	}
	// Another process takes the last wake of the window between this
	// request's read of the history and its write.
	writes := 0
	store.beforeWrite = func(id string) {
		if writes++; writes > 1 {
			return
		}
		front := sessionFrontDoor(store.MemStore)
		seen, err := front.MailWakeHistory(id)
		if err == nil {
			err = front.RecordMailWake(id, seen, session.MailWakeEntry{Sender: "city:alpha", At: time.Now().UTC()})
		}
		if err != nil {
			t.Error(err)
		}
	}
	answer := c.keyed("alpha", reader.ID, "alpha/worker")
	if answer.status != http.StatusCreated || answer.notified {
		t.Errorf("a fourth wake after losing the race for the third: %+v", answer)
	}
	if history, typed := c.history(reader.ID), c.typed(reader); len(history) != 3 || len(typed) != 2 || writes != 1 {
		t.Errorf("history %q, %d reminder(s) typed, %d write attempt(s); want three wakes recorded, two typed, one attempt", history, len(typed), writes)
	}
}

// A reservation that keeps losing gives up after a fixed number of attempts
// and declines, as a failed history write always has.
func TestPeerCityRetryGivesUp(t *testing.T) {
	store := newMailWakeHistoryHook(true)
	c := newPeerWakeCity(t, store, []string{"alpha"},
		config.Agent{Name: "reader", Provider: "claude", StartCommand: "echo", MailWakeFrom: []string{"city:alpha"}},
	)
	reader := c.start("reader", "")
	writes := 0
	store.beforeWrite = func(id string) {
		writes++
		if err := store.MemStore.SetMetadata(id, "unrelated", fmt.Sprint(writes)); err != nil {
			t.Error(err)
		}
	}
	answer := c.keyed("alpha", reader.ID, "alpha/mayor")
	if answer.status != http.StatusCreated || answer.notified {
		t.Errorf("listed mail whose reservation never lands: %+v", answer)
	}
	if history, typed := c.history(reader.ID), c.typed(reader); len(history) != 0 || len(typed) != 0 || writes != mailWakeRecordAttempts {
		t.Errorf("history %q, %d reminder(s) typed, %d write attempt(s); want none, none and %d", history, len(typed), writes, mailWakeRecordAttempts)
	}
}

// Listed mails that arrive together are counted one at a time, whether or
// not the session store can fence a write: those that fit the budget all get
// a turn, the rest are declined, and no two reminders are typed over each
// other.
func TestPeerCityConcurrentListedSendsHoldBudget(t *testing.T) {
	for _, tc := range []struct {
		name              string
		conditionalWrites bool
		prior, want       int
	}{
		{"fenced writes, room for both", true, 0, 2},
		{"fenced writes, room for one", true, 2, 1},
		{"unfenced writes, room for both", false, 0, 2},
		{"unfenced writes, room for one", false, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMailWakeHistoryHook(tc.conditionalWrites)
			c := newPeerWakeCity(t, store, []string{"alpha"},
				config.Agent{Name: "reader", Provider: "claude", StartCommand: "echo", MailWakeFrom: []string{"city:alpha"}},
			)
			reader := c.start("reader", "")
			for range tc.prior {
				if answer := c.keyed("alpha", reader.ID, "alpha/mayor"); !answer.notified {
					t.Fatalf("wake inside the budget: %+v", answer)
				}
			}
			// Hold each history write until both requests have read the
			// history, or until it is plain that they are taking turns.
			var mu sync.Mutex
			waiting, both := 0, make(chan struct{})
			store.beforeWrite = func(string) {
				mu.Lock()
				if waiting++; waiting == 2 {
					close(both)
				}
				mu.Unlock()
				select {
				case <-both:
				case <-time.After(500 * time.Millisecond):
				}
			}
			requests := []*http.Request{
				peerMailRequest(t, c.keys["alpha"], "alpha", reader.ID, "alpha/mayor"),
				peerMailRequest(t, c.keys["alpha"], "alpha", reader.ID, "alpha/worker"),
			}
			answers, errs := make([]peerMailAnswer, len(requests)), make([]error, len(requests))
			var wg sync.WaitGroup
			for i, r := range requests {
				wg.Add(1)
				go func() {
					defer wg.Done()
					answers[i], errs[i] = servePeerMail(c.hardened, r)
				}()
			}
			wg.Wait()
			notified := 0
			for i, answer := range answers {
				if errs[i] != nil || answer.status != http.StatusCreated {
					t.Fatalf("send %d: %+v, %v", i+1, answer, errs[i])
				}
				if answer.notified {
					notified++
				}
			}
			if notified != tc.want {
				t.Errorf("%d of two concurrent sends got a turn, want %d", notified, tc.want)
			}
			if history := c.history(reader.ID); len(history) != tc.prior+tc.want {
				t.Errorf("wake history %q, want %d wake(s)", history, tc.prior+tc.want)
			}
			typed, queued := len(c.typed(reader)), c.queued(store)
			if typed+queued != tc.prior+tc.want || typed > tc.prior+1 {
				t.Errorf("%d reminder(s) typed and %d queued after %d earlier; want %d more, at most one of them typed while the other is in flight", typed, queued, tc.prior, tc.want)
			}
		})
	}
}

// A key the recipient does not list is declined before it can hold the
// recipient's live-delivery slot, so it cannot push a listed reminder into
// the queue.
func TestPeerCityUnlistedKeyDoesNotHoldDeliverySlot(t *testing.T) {
	store := newMailWakeHistoryHook(true)
	c := newPeerWakeCity(t, store, []string{"alpha", "beta"},
		config.Agent{Name: "reader", Provider: "claude", StartCommand: "echo", MailWakeFrom: []string{"city:alpha"}},
	)
	reader := c.start("reader", "")
	held := false
	store.beforeGet = func(string) {
		if _, ok := c.state.liveMailNotifies.Load(reader.ID); ok {
			held = true
		}
	}
	c.assertDeclined("unlisted key beta", c.keyed("beta", reader.ID, "beta/mayor"), reader)
	if held {
		t.Error("an unlisted key held the recipient's live-delivery slot")
	}
	if answer := c.keyed("alpha", reader.ID, "alpha/mayor"); !answer.notified || !held {
		t.Errorf("the listed key: %+v, held the slot while it worked: %t", answer, held)
	}
}

// Without an open provider the controller declines a listed turn request, as
// it does an unlisted one. It never builds a provider from the process
// environment.
func TestPeerCityListedWakeNeedsControllerProvider(t *testing.T) {
	c := newPeerWakeCity(t, beads.NewMemStore(), []string{"alpha"},
		config.Agent{Name: "reader", Provider: "claude", StartCommand: "echo", MailWakeFrom: []string{"city:alpha"}},
	)
	reader := c.start("reader", "")
	c.state.sp = nil
	var ambient atomic.Int64
	build := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		ambient.Add(1)
		return c.sp, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = build })
	c.assertDeclined("listed key with no controller provider", c.keyed("alpha", reader.ID, "alpha/mayor"), reader)
	if n := ambient.Load(); n != 0 {
		t.Errorf("built %d ambient provider(s)", n)
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
