package extmsg

import (
	"sync"
	"time"
)

// InboundReceiptState is what gc can say about a receipt id after the fact.
type InboundReceiptState string

const (
	// InboundReceiptPending means the fan-out this receipt names is still
	// running: gc has neither delivered nor given up. A consumer keeps
	// waiting.
	InboundReceiptPending InboundReceiptState = "pending"
	// InboundReceiptConcluded means the fan-out finished and Delivery holds
	// its final outcome — the same [InboundDelivery] the inbound response would
	// have carried had it concluded inside the response budget.
	InboundReceiptConcluded InboundReceiptState = "concluded"
	// InboundReceiptUnknown means gc holds no record of this receipt id: it
	// was never issued by this process (a gc restart took the fan-out with
	// it), or it concluded and its record has since aged out of retention.
	// A RUNNING fan-out is never reported unknown — its record is neither
	// expired nor evicted while it is pending — so a consumer that polls
	// within the retention window and reads unknown has a definite answer:
	// nobody in this process is still trying.
	InboundReceiptUnknown InboundReceiptState = "unknown"
)

// InboundReceiptStatus is gc's after-the-fact answer for one receipt id.
//
// WHY THIS EXISTS. The inbound response reports "pending" when the terminal
// fan-out outruns its response budget (a busy session waits for an idle
// boundary before it is pasted, longer than any HTTP budget). The sends keep
// running, but until this store existed their eventual outcome was published
// into a channel nobody read any more. A consumer that held a dedup claim on
// that pending receipt — correctly, because re-posting into a busy session
// duplicates the message — was left with no way to learn whether the message
// ever landed, and a lost one left only a "held" log line behind (gp-3yg).
//
// The store keeps every fan-out's conclusion for a retention window so the
// consumer can ask afterwards. The state set is closed and each state is a
// definite statement; "unknown" is deliberately a state rather than an HTTP
// 404, so a consumer can tell "gc does not know this receipt" from "this gc
// has no such endpoint" and fail open only on the latter.
type InboundReceiptStatus struct {
	ReceiptID string              `json:"receipt_id"`
	State     InboundReceiptState `json:"state" enum:"pending,concluded,unknown"`
	// Delivery is present exactly when State is concluded.
	Delivery *InboundDelivery `json:"delivery,omitempty"`
	// BegunAt is when the fan-out started; zero when unknown.
	BegunAt time.Time `json:"begun_at,omitempty"`
	// ConcludedAt is when the fan-out finished; nil unless concluded.
	ConcludedAt *time.Time `json:"concluded_at,omitempty"`
}

// inboundReceiptRetention is how long a CONCLUDED receipt stays queryable. A
// consumer that received "pending" polls within minutes; an hour leaves room
// for one that was itself slow to get around to it without keeping every
// receipt a busy city ever issued.
const inboundReceiptRetention = time.Hour

// A PENDING record has no retention of its own: it lives exactly as long as
// the fan-out goroutine behind it. The fan-out is bounded only in principle
// (the runtime nudge does not take a context, so a wedged provider can hold
// it), but expiring the record under a running send would let a later poll
// read "unknown" — "nobody is still trying" — for a message that then lands,
// and a consumer acting on that would duplicate it (codex r1 P2 #1). The
// consumer's own poll deadline bounds how long IT waits, and it records that
// as "state unknown", not as a loss. Memory is bounded regardless: every
// pending record is backed by a live goroutine that already costs more.
//
// inboundReceiptCapacity bounds the CONCLUDED population — pending records
// are neither counted against it nor ever evicted by it (codex r2 P2 #1).
// Well above the number of inbounds a city sees inside one retention
// window; when a conclusion pushes the count past it, the oldest concluded
// records are shed.
const inboundReceiptCapacity = 4096

type inboundReceiptEntry struct {
	// city is the city the fan-out ran for. A lookup through another
	// city's path answers unknown: receipt ids are enumerable
	// (ir-<pid>-<seq>) and a concluded delivery names session ids and
	// error text, which one city must not read off another (codex r4 P2).
	city        string
	begunAt     time.Time
	concludedAt time.Time
	delivery    *InboundDelivery
}

func (e *inboundReceiptEntry) concluded() bool { return e.delivery != nil }

// InboundReceiptStore remembers the outcome of every inbound fan-out for a
// retention window, keyed by receipt id, so a consumer that was answered
// "pending" can ask later whether the send landed. Safe for concurrent use.
type InboundReceiptStore struct {
	mu       sync.Mutex
	entries  map[string]*inboundReceiptEntry
	now      func() time.Time
	capacity int
	// concluded counts the entries that carry a delivery, so the capacity
	// check does not walk the map.
	concluded int
}

// defaultInboundReceipts is the process-wide store every Server shares.
//
// Receipt ids are process-scoped (NextInboundReceiptID embeds the pid), and
// so is the fan-out goroutine a pending record stands for — it belongs to
// the process, not to the Server that started it. The supervisor replaces a
// city's Server whenever the city's State changes (and drops it while the
// city is briefly not running); a store hung off the Server would then
// answer "unknown" for a fan-out still running under the old one, and a
// consumer acting on that would duplicate the message when it landed
// (codex r2 P2 #2). One store per process is the scope at which "unknown"
// is a true statement.
var (
	defaultInboundReceipts     *InboundReceiptStore
	defaultInboundReceiptsOnce sync.Once
)

// DefaultInboundReceipts returns the process-wide store.
func DefaultInboundReceipts() *InboundReceiptStore {
	defaultInboundReceiptsOnce.Do(func() {
		defaultInboundReceipts = NewInboundReceiptStore()
	})
	return defaultInboundReceipts
}

// NewInboundReceiptStore returns an empty store with production retention
// and capacity. Production code uses [DefaultInboundReceipts]; a fresh
// store is for tests that need isolation.
func NewInboundReceiptStore() *InboundReceiptStore {
	return &InboundReceiptStore{
		entries:  make(map[string]*inboundReceiptEntry),
		now:      time.Now,
		capacity: inboundReceiptCapacity,
	}
}

// Begin records that the fan-out for receiptID has started on behalf of
// city. Idempotent: a second Begin for a known id keeps the original
// record.
func (s *InboundReceiptStore) Begin(city, receiptID string) {
	if s == nil || receiptID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	if _, ok := s.entries[receiptID]; ok {
		return
	}
	// No capacity check: a pending record is bounded by the fan-out
	// goroutine behind it, never by this store.
	s.entries[receiptID] = &inboundReceiptEntry{city: city, begunAt: s.now()}
}

// Conclude records the fan-out's final outcome. A conclusion for an id that
// was never begun is recorded anyway — a late result must never be dropped —
// and a second conclusion for the same id is ignored, so the first (and only
// real) outcome is what a consumer reads.
func (s *InboundReceiptStore) Conclude(city, receiptID string, delivery InboundDelivery) {
	if s == nil || receiptID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	now := s.now()
	e, ok := s.entries[receiptID]
	if !ok {
		e = &inboundReceiptEntry{city: city, begunAt: now}
		s.entries[receiptID] = e
	}
	if e.concluded() {
		return
	}
	d := cloneInboundDelivery(delivery)
	e.delivery = &d
	e.concludedAt = now
	s.concluded++
	s.makeRoomLocked()
}

// cloneInboundDelivery copies a delivery including its Members slice, so
// neither the caller's later mutations nor a Lookup result's can reach the
// stored record (codex r1 P2 #2).
func cloneInboundDelivery(d InboundDelivery) InboundDelivery {
	out := d
	if d.Members != nil {
		out.Members = append([]InboundDeliveryMember(nil), d.Members...)
	}
	return out
}

// Lookup reports what the store knows about receiptID for city. Never an
// error: an id the store does not hold — or holds for a different city —
// is the unknown state, which is itself an answer.
func (s *InboundReceiptStore) Lookup(city, receiptID string) InboundReceiptStatus {
	out := InboundReceiptStatus{ReceiptID: receiptID, State: InboundReceiptUnknown}
	if s == nil || receiptID == "" {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	e, ok := s.entries[receiptID]
	if !ok || e.city != city {
		return out
	}
	out.BegunAt = e.begunAt
	if !e.concluded() {
		out.State = InboundReceiptPending
		return out
	}
	out.State = InboundReceiptConcluded
	d := cloneInboundDelivery(*e.delivery)
	out.Delivery = &d
	at := e.concludedAt
	out.ConcludedAt = &at
	return out
}

// sweepLocked drops CONCLUDED records past their retention. Pending records
// are never swept (see inboundReceiptCapacity). Called on every operation
// rather than from a timer so the store needs no goroutine and no Close.
func (s *InboundReceiptStore) sweepLocked() {
	now := s.now()
	for id, e := range s.entries {
		if e.concluded() && now.Sub(e.concludedAt) > inboundReceiptRetention {
			s.dropLocked(id, e)
		}
	}
}

// makeRoomLocked evicts the oldest CONCLUDED records until the concluded
// population is back within capacity. A pending record is never a victim:
// its fan-out is still running, and evicting it would report a live send
// as unknown.
func (s *InboundReceiptStore) makeRoomLocked() {
	for s.capacity > 0 && s.concluded > s.capacity {
		victim := ""
		var victimAt time.Time
		var victimEntry *inboundReceiptEntry
		for id, e := range s.entries {
			if !e.concluded() {
				continue
			}
			if victim == "" || e.concludedAt.Before(victimAt) {
				victim, victimAt, victimEntry = id, e.concludedAt, e
			}
		}
		if victim == "" {
			return
		}
		s.dropLocked(victim, victimEntry)
	}
}

func (s *InboundReceiptStore) dropLocked(id string, e *inboundReceiptEntry) {
	if e.concluded() {
		s.concluded--
	}
	delete(s.entries, id)
}
