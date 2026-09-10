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

// Tx authorizes OUTSIDE the transaction, then runs it. A store's Tx can hold
// a lock for the whole callback (the native dolt store holds its read lock;
// a reconnecting read inside would wait on the same lock forever), so the
// rows are read before the transaction opens: the callback runs once
// against a recorder that only collects the ids it writes (beads.Tx has no
// reads, so a callback's writes are fixed by what it captured and it runs
// the same way twice), every id is authorized through the wrapped store's
// live handle, and only then does the real transaction run the callback
// again — with no fence reads inside it. A callback error on the recording
// pass is returned as is; a refused id returns the refusal and the
// transaction never opens. Create is not fenced on either pass.
func (f *fencedStore) Tx(commitMsg string, fn func(tx beads.Tx) error) error {
	rec := &recordingTx{}
	if err := fn(rec); err != nil {
		return err
	}
	for _, id := range rec.ids {
		if err := f.allow(id); err != nil {
			return err
		}
	}
	return f.Store.Tx(commitMsg, fn)
}

// recordingTx is the fence's first pass over a transaction callback: it
// records the row ids the callback writes and writes nothing.
type recordingTx struct {
	ids []string
}

func (r *recordingTx) Create(b beads.Bead) (beads.Bead, error) { return b, nil }
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
