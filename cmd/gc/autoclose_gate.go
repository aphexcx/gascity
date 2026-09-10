package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/federation"
)

// autocloseGate is the cross-city fence for gc's AUTOMATIC bead writers: the
// convoy autoclose (the bead-close event path, the bd on_close hook path and
// the `gc convoy check` sweep), the molecule autoclose (step-terminal and
// source-bead triggers), the attached-wisp close, and the wisp GC's repair,
// abandoned-root and purge sweeps. Every one of them runs in EVERY city that
// holds a copy of a federated store, on the same rows, with nothing but
// wall-clock ordering between them: after a `gc dolt pull` brings one city's
// closes into another city's copy, the second city's event path sees "all
// children closed" and closes the convoy again — a second close of the same
// row with its own closed_at, updated_at and row_lock, which the next pull
// turns into a dolt conflict (hw-m0t6n, 2026-09-10; gp-c04p).
//
// The rule is federation.MayWriteAutomatically: exactly one city maintains a
// row — the one its single owner:<identity> label names. A row with no owner
// label, another city's owner, or two owners is not written by this city's
// automatic writers; a handoff label licenses a claim, never an automatic
// write. An unset [federation] identity means the city is not federated and
// the gate is off.
//
// The door is the STORE, not the call sites: fence wraps the store an
// automatic writer is handed so every write method that names a row — Close,
// CloseAll, CloseWithReason, Update, Reopen, Delete, SetMetadata,
// SetMetadataBatch, SetLocalString, DepAdd, DepRemove, and the Tx surface —
// reads the row's labels first and refuses the write with
// errAutomaticWriteFenced, logging the one greppable refusal line once per
// row. Reads pass through untouched (the cached and live handles are the
// underlying store's own), and wisp-tier (ephemeral) rows are never fenced:
// they are dolt-ignored and never leave the city. The production entry
// points fence from the loaded city config (autocloseGateFor): the
// controller's bead-close path, the bd hook entries, `gc convoy check` and
// the wisp GC tick. A store reachable only without a city.toml keeps the
// zero gate (no city.toml means no federation identity); a city.toml that
// exists but cannot be loaded vetoes the hook entries instead, because the
// identity cannot be proven either way.
type autocloseGate struct {
	// identity is this city's [federation] identity, trimmed; "" = off.
	identity string
}

// autocloseGateFor builds the gate for the automatic writers of the city cfg
// describes. A nil cfg yields the non-federated gate.
func autocloseGateFor(cfg *config.City) autocloseGate {
	if cfg == nil {
		return autocloseGate{}
	}
	return autocloseGate{identity: strings.TrimSpace(cfg.Federation.Identity)}
}

// errAutomaticWriteFenced is returned by every write the fence refused.
// Nothing was written and the refusal is already logged; an automatic
// writer treats it as a skipped row, never as a failure of the sweep.
var errAutomaticWriteFenced = errors.New("cross-city fence refused the automatic write")

// fence returns store wrapped in this city's fence, or store itself when the
// city is not federated. site prefixes the refusal lines written to log.
func (g autocloseGate) fence(store beads.Store, log io.Writer, site string) beads.Store {
	if g.identity == "" || store == nil {
		return store
	}
	if f, ok := store.(*fencedStore); ok && f.gate == g {
		return store
	}
	if log == nil {
		log = io.Discard
	}
	return &fencedStore{Store: store, gate: g, site: site, log: log, logged: map[string]bool{}}
}

// fencedStore is the fenced beads.Store: reads delegate to the wrapped store,
// writes go through allow first.
type fencedStore struct {
	beads.Store
	gate   autocloseGate
	site   string
	log    io.Writer
	logged map[string]bool
}

// allow reports whether this city's automatic writer may write the row id
// holds, reading the row through the wrapped store's live handle. A row
// that does not exist is left to the write itself (which reports not-found
// as it always did); a row that cannot be read is refused, fail-closed.
func (f *fencedStore) allow(id string) error {
	bead, err := beads.HandlesFor(f.Store).Live.Get(id)
	if err != nil {
		// bd resolves a missing id to a DIFFERENT bead by substring and
		// reports ErrIDCollision — which wraps ErrNotFound. That is not an
		// absent row the write may fall through to (the store's own mutators
		// would land it on the other, possibly foreign, bead), so it is
		// refused before the plain not-found pass-through.
		if errors.Is(err, beads.ErrIDCollision) {
			f.refuse(id, "id_collision="+strconvQuoteToken(err.Error())+" this_identity="+f.gate.identity+" rule=sole-owner")
			return fmt.Errorf("%w: %s resolved to a different bead: %w", errAutomaticWriteFenced, id, err)
		}
		if errors.Is(err, beads.ErrNotFound) {
			return nil
		}
		f.refuse(id, "read_error="+strconvQuoteToken(err.Error())+" this_identity="+f.gate.identity+" rule=sole-owner")
		return fmt.Errorf("%w: %s could not be read before the write: %w", errAutomaticWriteFenced, id, err)
	}
	if bead.Ephemeral {
		return nil
	}
	ok, reason := federation.MayWriteAutomatically(bead.Labels, f.gate.identity)
	if ok {
		return nil
	}
	f.refuse(id, reason)
	return fmt.Errorf("%w: %s", errAutomaticWriteFenced, federation.RefusalLine(id, reason))
}

func (f *fencedStore) refuse(id, reason string) {
	if f.logged[id] {
		return
	}
	f.logged[id] = true
	fmt.Fprintf(f.log, "%s: %s\n", f.site, federation.RefusalLine(id, reason)) //nolint:errcheck // best-effort diagnostics
}

func strconvQuoteToken(s string) string {
	return fmt.Sprintf("%q", s)
}

func (f *fencedStore) Update(id string, opts beads.UpdateOpts) error {
	if err := f.allow(id); err != nil {
		return err
	}
	return f.Store.Update(id, opts)
}

func (f *fencedStore) Close(id string) error {
	if err := f.allow(id); err != nil {
		return err
	}
	return f.Store.Close(id)
}

// CloseWithReason keeps the explicit-reason close path of a store that has
// one (closeConvoyWithReason / closeMoleculeWithReason look for it) and
// falls back to Close, exactly as those callers would on the bare store.
func (f *fencedStore) CloseWithReason(id, reason string) error {
	if err := f.allow(id); err != nil {
		return err
	}
	if closer, ok := f.Store.(explicitReasonCloser); ok {
		return closer.CloseWithReason(id, reason)
	}
	return f.Store.Close(id)
}

func (f *fencedStore) Reopen(id string) error {
	if err := f.allow(id); err != nil {
		return err
	}
	return f.Store.Reopen(id)
}

// CloseAll closes the ids this city may write and skips the rest: a subtree
// that mixes rows of two cities closes this city's rows only. The count is
// what was actually closed.
func (f *fencedStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	kept := make([]string, 0, len(ids))
	for _, id := range ids {
		if err := f.allow(id); err != nil {
			continue
		}
		kept = append(kept, id)
	}
	if len(kept) == 0 {
		return 0, nil
	}
	return f.Store.CloseAll(kept, metadata)
}

func (f *fencedStore) SetMetadata(id, key, value string) error {
	if err := f.allow(id); err != nil {
		return err
	}
	return f.Store.SetMetadata(id, key, value)
}

func (f *fencedStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if err := f.allow(id); err != nil {
		return err
	}
	return f.Store.SetMetadataBatch(id, kvs)
}

func (f *fencedStore) SetLocalString(id, key, value string) error {
	if err := f.allow(id); err != nil {
		return err
	}
	return f.Store.SetLocalString(id, key, value)
}

func (f *fencedStore) Delete(id string) error {
	if err := f.allow(id); err != nil {
		return err
	}
	return f.Store.Delete(id)
}

func (f *fencedStore) DepAdd(issueID, dependsOnID, depType string) error {
	if err := f.allow(issueID); err != nil {
		return err
	}
	return f.Store.DepAdd(issueID, dependsOnID, depType)
}

func (f *fencedStore) DepRemove(issueID, dependsOnID string) error {
	if err := f.allow(issueID); err != nil {
		return err
	}
	return f.Store.DepRemove(issueID, dependsOnID)
}

// Tx authorizes OUTSIDE the transaction, then runs it with an allowlist
// inside. A store's Tx can hold a lock for the whole callback (the native
// dolt store holds its read lock; a reconnecting read inside would wait on
// the same lock forever), so the rows are read before the transaction
// opens: the callback runs once against a recorder that only collects the
// ids it writes, every id is authorized through the wrapped store's live
// handle, and only then does the real transaction run the callback again
// behind a Tx that permits exactly those ids and refuses any other without
// a read. A callback whose control flow depends on write results (the
// recorder answers every write with success) can therefore name a row on
// the real pass it did not name on the recording pass — and that write is
// refused, never read. A callback error on the recording pass is returned
// as is; a refused id returns the refusal and the transaction never opens.
// Create is not fenced on either pass.
func (f *fencedStore) Tx(commitMsg string, fn func(tx beads.Tx) error) error {
	rec := &recordingTx{}
	if err := fn(rec); err != nil {
		return err
	}
	allowed := make(map[string]bool, len(rec.ids))
	for _, id := range rec.ids {
		if err := f.allow(id); err != nil {
			return err
		}
		allowed[id] = true
	}
	// The parents of the rows the callback creates are read here, outside
	// the transaction, so a create inside it can stamp the child's lane
	// without a read under the lock.
	parents := make(map[string][]string, len(rec.parents))
	for _, parentID := range rec.parents {
		if labels, known := f.parentLabels(parentID); known {
			parents[parentID] = labels
		}
	}
	return f.Store.Tx(commitMsg, func(tx beads.Tx) error {
		return fn(&fencedTx{Tx: tx, allowed: allowed, parents: parents, fence: f})
	})
}

// fencedTx is the real pass of a fenced transaction: only the ids the
// recording pass named and the fence authorized may be written; anything
// else is refused in memory, without a read.
type fencedTx struct {
	beads.Tx
	allowed map[string]bool
	parents map[string][]string // labels of every parent the recording pass named, read outside the lock
	fence   *fencedStore
}

func (t *fencedTx) permit(id string) error {
	if t.allowed[id] {
		return nil
	}
	t.fence.refuse(id, "this_identity="+t.fence.gate.identity+" rule=sole-owner tx=unrecorded-write")
	return fmt.Errorf("%w: %s was not named on the transaction's recording pass", errAutomaticWriteFenced, id)
}

func (t *fencedTx) Update(id string, opts beads.UpdateOpts) error {
	if err := t.permit(id); err != nil {
		return err
	}
	return t.Tx.Update(id, opts)
}

func (t *fencedTx) SetMetadataBatch(id string, kvs map[string]string) error {
	if err := t.permit(id); err != nil {
		return err
	}
	return t.Tx.SetMetadataBatch(id, kvs)
}

func (t *fencedTx) Close(id string) error {
	if err := t.permit(id); err != nil {
		return err
	}
	return t.Tx.Close(id)
}

// automaticWriteFilter is what a fenced store exposes so a BOUNDED sweep can
// leave out the rows the fence would refuse before spending its budget on
// them — otherwise a handful of another city's rows at the front of the
// candidate list would be selected, refused and re-selected every pass,
// starving this city's own rows behind them. It is not a second door: the
// write itself is still fenced; this only asks the same question earlier.
type automaticWriteFilter interface {
	automaticWriteAllowed(id string) bool
}

func (f *fencedStore) automaticWriteAllowed(id string) bool {
	return f.allow(id) == nil
}

// mayWriteAutomatically reports whether store would let an automatic writer
// write id: true for any store that is not fenced.
func mayWriteAutomatically(store beads.Store, id string) bool {
	if filter, ok := store.(automaticWriteFilter); ok {
		return filter.automaticWriteAllowed(id)
	}
	return true
}

// recordingTx is the fence's first pass over a transaction callback: it
// records the row ids the callback writes and writes nothing.
type recordingTx struct {
	ids     []string
	parents []string // ParentID of every create, so the real pass can stamp the child's lane
}

func (r *recordingTx) Create(b beads.Bead) (beads.Bead, error) {
	if b.ParentID != "" {
		r.parents = append(r.parents, b.ParentID)
	}
	return b, nil
}

func (r *recordingTx) Update(id string, _ beads.UpdateOpts) error {
	r.ids = append(r.ids, id)
	return nil
}

func (r *recordingTx) SetMetadataBatch(id string, _ map[string]string) error {
	r.ids = append(r.ids, id)
	return nil
}

func (r *recordingTx) Close(id string) error {
	r.ids = append(r.ids, id)
	return nil
}

// AtomicTx reports the wrapped store's atomicity, so callers that pick the
// transactional write pair (closeRootWithMarker) keep their guarantee.
func (f *fencedStore) AtomicTx() bool {
	return beads.StoreSupportsAtomicTx(f.Store)
}

// Handles keeps the wrapped store's own cached and live readers (a caching
// store's tiers are its own) and routes the writer through the fence.
func (f *fencedStore) Handles() beads.StoreHandles {
	h := beads.HandlesFor(f.Store)
	return beads.StoreHandles{Cached: h.Cached, Live: h.Live, Writer: f}
}

// fenceDemandPrepStores hands the demand-preparation passes their stores.
// Legacy-bound route canonicalization, slot-suffix collapse and
// control-dispatcher route repair rewrite gc.routed_to on open, unassigned
// work with no agent behind it — automatic writers that every city's
// controller runs on the same shared rows — so on a federated city they
// write only this city's rows. The result is index-aligned with stores (and
// so with the beads they came with); one fence per distinct store, so a
// refused row is logged once. A non-federated city gets stores back as is.
func fenceDemandPrepStores(cfg *config.City, stores []beads.Store, stderr io.Writer) []beads.Store {
	gate := autocloseGateFor(cfg)
	if gate.identity == "" {
		return stores
	}
	fenced := make([]beads.Store, len(stores))
	byStore := make(map[beads.Store]beads.Store, 4)
	for i, store := range stores {
		if store == nil {
			continue
		}
		f, ok := byStore[store]
		if !ok {
			f = gate.fence(store, stderr, "demand prep")
			byStore[store] = f
		}
		fenced[i] = f
	}
	return fenced
}

// fenceOrderTrackingSweepStore fences a store the order-tracking sweeps carry
// inside their scope wrapper. The wrapper embeds a bare beads.Store and does
// not forward Handles(), so a fence around the WRAPPER would read the row
// through the wrapper's plain Get — a CachingStore's memoized row — and
// authorize from a stale owner label. The fence goes around the inner store,
// where the live handle is, and the wrapper (its label and dedup key) goes
// back outside it. An unwrapped store is fenced directly.
func fenceOrderTrackingSweepStore(store beads.Store, fence func(beads.Store) beads.Store) beads.Store {
	if scoped, ok := store.(orderTrackingSweepScopedStore); ok {
		scoped.Store = fence(scoped.Store)
		return scoped
	}
	return fence(store)
}

// bareStore returns the store behind a fence, or store itself. For identity
// comparisons that must see the database, not the wrapper around it.
func bareStore(store beads.Store) beads.Store {
	if f, ok := store.(*fencedStore); ok {
		return f.Store
	}
	return store
}

// stampOwner labels a permanent row this city's automatic writer creates by
// the store's own one rule, federation.OwnerLabelForChild: an owner already
// on the row wins; a child stays in its PARENT's lane (a create under
// another city's bead never lands a second owner or the wrong lane); only a
// child of an unowned parent, or a row with no parent, is the creating
// city's — so its own follow-up writes (a cooked molecule's steps and deps,
// a poured wisp's metadata) pass the fence and the other city's are refused.
// parentLabels are the parent's labels as read through the live handle;
// parentKnown=false means the parent could not be read: no proof of its
// lane, so nothing is stamped (a bd backend applies the same rule itself).
// An ephemeral row is never fenced and never labeled.
func (g autocloseGate) stampOwner(b beads.Bead, parentLabels []string, parentKnown bool) beads.Bead {
	if b.Ephemeral || !parentKnown {
		return b
	}
	b.Labels = federation.OwnerLabelForChild(append(make([]string, 0, len(b.Labels)+1), b.Labels...), parentLabels, federation.OwnerLabelPrefix+g.identity)
	return b
}

// parentLabels reads a parent's labels through the wrapped store's live
// handle. A row with no parent is known (nil labels); a parent that cannot
// be read is not.
func (f *fencedStore) parentLabels(parentID string) (labels []string, known bool) {
	if parentID == "" {
		return nil, true
	}
	parent, err := beads.HandlesFor(f.Store).Live.Get(parentID)
	if err != nil {
		return nil, false
	}
	return parent.Labels, true
}

// Create stamps the owner label a permanent row should carry (see
// stampOwner), reading the parent's lane first.
func (f *fencedStore) Create(b beads.Bead) (beads.Bead, error) {
	labels, known := f.parentLabels(b.ParentID)
	return f.Store.Create(f.gate.stampOwner(b, labels, known))
}

// Create inside a fenced transaction stamps the owner label the same way —
// from the parent labels read BEFORE the transaction opened (the recording
// pass names every parent; nothing is read inside the lock; a parent the
// recording pass did not name is unknown, so nothing is stamped) — and
// permits the new row for the rest of the transaction: a row this city just
// created is its own, and the recording pass could not have named its id.
func (t *fencedTx) Create(b beads.Bead) (beads.Bead, error) {
	labels, known := t.parents[b.ParentID]
	if b.ParentID == "" {
		labels, known = nil, true
	}
	created, err := t.Tx.Create(t.fence.gate.stampOwner(b, labels, known))
	if err == nil && created.ID != "" {
		t.allowed[created.ID] = true
	}
	return created, err
}
