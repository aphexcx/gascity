package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// writeFederatedSweepCity is a file-store city with a federation identity for
// the two CLI-run sweeps (`gc order sweep-tracking`, `gc order
// sweep-nudge-mail`); the core pack runs both as subprocesses on a schedule.
func writeFederatedSweepCity(t *testing.T, identity string) (string, beads.Store) {
	t.Helper()
	cityPath := writeConvoyTestCityWithFederation(t, identity)
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_RIG_ROOT", "")
	t.Chdir(cityPath)
	if err := ensureScopedFileStoreLayout(cityPath); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityPath); err != nil {
		t.Fatal(err)
	}
	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatal(err)
	}
	return cityPath, store
}

// `gc order sweep-tracking` (the core pack runs it every minute) closes stale
// tracking rows with no agent behind it: only the row's own city does.
func TestOrderSweepTrackingCommandHonorsFederationIdentity(t *testing.T) {
	for _, tc := range twoCityCases {
		t.Run("city="+tc.city, func(t *testing.T) {
			cityPath, store := writeFederatedSweepCity(t, tc.city)
			tracking, err := store.Create(beads.Bead{Title: "order:cleanup", Labels: []string{"order-run:cleanup", labelOrderTracking, "owner:citadel"}})
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := cmdOrderSweepTrackingWithOptions(time.Nanosecond, false, false, false, false, nil, &stdout, &stderr); code != 0 {
				t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
			}
			reopened, err := openStoreAtForCity(cityPath, cityPath)
			if err != nil {
				t.Fatal(err)
			}
			got, err := reopened.Get(tracking.ID)
			if err != nil {
				t.Fatal(err)
			}
			if closed := got.Status == "closed"; closed != tc.wantClosed {
				t.Fatalf("tracking status = %q, want closed=%v; stdout=%q stderr=%q", got.Status, tc.wantClosed, stdout.String(), stderr.String())
			}
		})
	}
}

// `gc order sweep-nudge-mail` (every five minutes) closes read mail and
// consumed nudges with no agent behind it: only the row's own city does.
func TestOrderSweepNudgeMailCommandHonorsFederationIdentity(t *testing.T) {
	for _, tc := range twoCityCases {
		t.Run("city="+tc.city, func(t *testing.T) {
			cityPath, store := writeFederatedSweepCity(t, tc.city)
			mail, err := store.Create(beads.Bead{Title: "old mail", Type: "message", Labels: []string{"read", "owner:citadel"}})
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			if code := cmdOrderSweepNudgeMail(time.Nanosecond, time.Nanosecond, false, false, &stdout, &stderr); code != 0 {
				t.Fatalf("exit = %d; stderr=%q", code, stderr.String())
			}
			reopened, err := openStoreAtForCity(cityPath, cityPath)
			if err != nil {
				t.Fatal(err)
			}
			got, err := reopened.Get(mail.ID)
			if err != nil {
				t.Fatal(err)
			}
			if closed := got.Status == "closed"; closed != tc.wantClosed {
				t.Fatalf("mail status = %q, want closed=%v; stdout=%q stderr=%q", got.Status, tc.wantClosed, stdout.String(), stderr.String())
			}
		})
	}
}

// The detached-orphan lane re-stamps gc.routed_to on a handoff orphan from a
// shared session bead — on both its delta and backstop passes. Only the row's
// own city writes; a refused row is a skip, never a lane error.
func TestDetachedOrphanLaneTwoCitiesOnlyOwnerWrites(t *testing.T) {
	for _, pass := range []string{"delta", "backstop"} {
		for _, tc := range twoCityCases {
			t.Run(pass+"/city="+tc.city, func(t *testing.T) {
				work := detachedOrphanWorkBead("D-1")
				work.Labels = []string{"owner:citadel"}
				store := &countingRouteStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{detachedOrphanSessionBead(), work}, nil)}
				var stderr bytes.Buffer
				cr := &CityRuntime{
					cityName:            "city",
					cfg:                 &config.City{Federation: config.FederationConfig{Identity: tc.city}},
					standaloneCityStore: store,
					logPrefix:           "gc",
					stderr:              &stderr,
				}
				var report detachedOrphanReport
				if pass == "delta" {
					cr.detachedOrphanLaneOf().pending["D-1"] = struct{}{}
					report = cr.sweepDetachedHandoffOrphansDelta()
				} else {
					report = cr.runDetachedOrphanBackstop(backstopReasonCadence)
				}
				if report.err != nil {
					t.Fatalf("lane err = %v (a refused row is a skip, never an error)", report.err)
				}
				want := 0
				if tc.wantClosed {
					want = 1
				}
				got, _ := store.Get("D-1")
				routed := got.Metadata[beadmeta.RoutedToMetadataKey] == detachedOrphanTestPool
				if report.restored != want || routed != tc.wantClosed {
					t.Fatalf("restored = %d routed = %v, want %d/%v; stderr=%q", report.restored, routed, want, tc.wantClosed, stderr.String())
				}
			})
		}
	}
}

// Demand preparation rewrites gc.routed_to on open, unassigned work with no
// agent behind it (legacy bound routes, slot-suffixed routes, misplaced
// control-dispatcher routes). The stores those passes write through come
// from fenceDemandPrepStores: fenced on a federated city, untouched otherwise.
func TestDemandPrepRouteRewritesTwoCitiesOnlyOwnerWrites(t *testing.T) {
	const legacy = "rig-A/gc.planner"
	const canonical = "rig-A/planner"
	for _, tc := range twoCityCases {
		t.Run("city="+tc.city, func(t *testing.T) {
			cfg := legacyBoundRecoveryConfig()
			cfg.Federation.Identity = tc.city
			wb := workBead("wb-1", legacy, "", "open", 5)
			wb.Labels = []string{"owner:citadel"}
			mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
			var log bytes.Buffer
			stores := fenceDemandPrepStores(cfg, []beads.Store{mem}, &log)
			if len(stores) != 1 {
				t.Fatalf("stores = %d, want 1", len(stores))
			}
			if _, isFenced := stores[0].(*fencedStore); isFenced != (tc.city != "") {
				t.Fatalf("fenced = %v, want %v", isFenced, tc.city != "")
			}
			canonicalizeLegacyBoundUnassignedRoutedWork(cfg, []beads.Bead{wb}, stores, &log)
			got, err := mem.Get("wb-1")
			if err != nil {
				t.Fatal(err)
			}
			if rewritten := got.Metadata[beadmeta.RoutedToMetadataKey] == canonical; rewritten != tc.wantClosed {
				t.Fatalf("gc.routed_to = %q, rewritten=%v want %v; log=%q", got.Metadata[beadmeta.RoutedToMetadataKey], rewritten, tc.wantClosed, log.String())
			}
		})
	}
}

// staleLabelStore serves a cached row whose labels are stale while its Live
// handle reads the MemStore — the production CachingStore shape after a pull
// changed a row's owner label.
type staleLabelStore struct {
	*beads.MemStore
	cached beads.Bead
}

func (s staleLabelStore) Get(_ string) (beads.Bead, error) { return s.cached, nil }

func (s staleLabelStore) Handles() beads.StoreHandles {
	return beads.StoreHandles{Cached: s, Live: s.MemStore, Writer: s.MemStore}
}

// The order-tracking sweeps carry each store inside a scope wrapper that
// does not forward Handles(). The fence must still read the LIVE row through
// it, and the wrapper's label and key must survive the fence.
func TestOrderTrackingSweepFenceReadsTheLiveRowThroughTheScopeWrapper(t *testing.T) {
	mem := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "t-1", Title: "order:x", Status: "open", Labels: []string{labelOrderTracking, "owner:jadegate"}}}, nil)
	stale := staleLabelStore{MemStore: mem, cached: beads.Bead{ID: "t-1", Title: "order:x", Status: "open", Labels: []string{labelOrderTracking, "owner:citadel"}}}
	scoped := orderTrackingSweepScopedStore{Store: stale, label: "city", key: "city-key"}
	var log bytes.Buffer
	fenced := fenceOrderTrackingSweepStore(scoped, func(s beads.Store) beads.Store {
		return (autocloseGate{identity: "citadel"}).fence(s, &log, "sweep")
	})
	if err := fenced.Close("t-1"); err == nil {
		t.Fatalf("a row whose LIVE owner is jadegate was closed from its stale cached label; log=%q", log.String())
	}
	if got, _ := mem.Get("t-1"); got.Status != "open" {
		t.Fatalf("row written: %q", got.Status)
	}
	if mayWriteAutomatically(fenced, "t-1") {
		t.Fatalf("the bounded sweeps' pre-filter must see the fence through the scope wrapper")
	}
	if orderTrackingSweepStoreKey(fenced) != "city-key" || orderTrackingSweepStoreLabel(fenced, 0) != "city" {
		t.Fatalf("scope key/label lost behind the fence: key=%q label=%q", orderTrackingSweepStoreKey(fenced), orderTrackingSweepStoreLabel(fenced, 0))
	}
	if _, ok := fenceOrderTrackingSweepStore(mem, func(s beads.Store) beads.Store { return s }).(*beads.MemStore); !ok {
		t.Fatalf("an unscoped store must pass through the fence function unchanged")
	}
}

// Convergence reconciliation pours a root's first wisp BEFORE its first root
// write; Create is not fenced, so the root must be authorized first. Another
// city's root gets no child from this city.
func TestConvergenceReconcileNeverPoursForAnotherCitysRoot(t *testing.T) {
	for _, tc := range twoCityCases {
		t.Run("city="+tc.city, func(t *testing.T) {
			cr, store := setupConvergenceRuntime(t)
			cr.cfg.Federation.Identity = tc.city
			cr.convScopes[""] = cr.newConvergenceScope("", cr.fenceMaintenance(store, "convergence"), cr.cityPath, []string{sharedTestFormulaDir})
			root, err := store.Create(beads.Bead{
				Title:  "loop",
				Type:   "convergence",
				Status: "in_progress",
				Labels: []string{"owner:citadel"},
				Metadata: map[string]string{
					convergence.FieldFormula: "test-formula",
					convergence.FieldTarget:  "test-agent",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			cr.convergenceStartupReconcile(context.Background())
			children, err := store.List(beads.ListQuery{ParentID: root.ID, IncludeClosed: true, TierMode: beads.TierBoth})
			if err != nil {
				t.Fatal(err)
			}
			if poured := len(children) > 0; poured != tc.wantClosed {
				t.Fatalf("children = %d (poured=%v), want poured=%v; stderr=%q", len(children), poured, tc.wantClosed, cr.stderr.(*bytes.Buffer).String())
			}
			got, _ := store.Get(root.ID)
			if active := got.Metadata[convergence.FieldState] == convergence.StateActive; active != tc.wantClosed {
				t.Fatalf("root state = %q, want active=%v", got.Metadata[convergence.FieldState], tc.wantClosed)
			}
		})
	}
}

// A bounded nudge/mail sweep must not let a foreign backlog starve this
// city's own stale rows: the candidate query is not capped before ownership
// is known; only the close budget is.
func TestNudgeMailSweepClosesTheLocalRowBehindAForeignBacklog(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	old := now.Add(-3 * time.Hour)
	var seed []beads.Bead
	for i := range 20 {
		n := nudgeSeed("nudge-f"+string(rune('a'+i)), "nudge-f"+string(rune('a'+i)), old)
		n.Labels = append(n.Labels, "owner:jadegate")
		m := mailSeed("mail-f"+string(rune('a'+i)), old)
		m.Labels = append(m.Labels, "owner:jadegate")
		seed = append(seed, n, m)
	}
	mine := nudgeSeed("nudge-mine", "nudge-mine", old.Add(time.Minute))
	mine.Labels = append(mine.Labels, "owner:citadel")
	mail := mailSeed("mail-mine", old.Add(time.Minute))
	mail.Labels = append(mail.Labels, "owner:citadel")
	seed = append(seed, mine, mail)
	inner := beads.NewMemStoreFrom(100, seed, nil)
	fenced := (autocloseGate{identity: "citadel"}).fence(inner, io.Discard, "watchdog")

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: fenced}, beads.MailStore{Store: fenced}, &nudgequeue.State{}, now, 10*time.Minute, 30*time.Minute, 5)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.NudgeClosed != 1 || result.MailClosed != 1 {
		t.Fatalf("closed nudge=%d mail=%d, want 1 each (the local rows behind 20 foreign ones)", result.NudgeClosed, result.MailClosed)
	}
	for _, id := range []string{"nudge-mine", "mail-mine"} {
		if got, _ := inner.Get(id); got.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, got.Status)
		}
	}
}

// A permanent row an automatic writer creates through the fence is this
// city's to maintain: Create stamps owner:<identity> unless the row already
// names an owner or is ephemeral (never fenced, never labeled).
func TestAutocloseFenceCreateStampsThisCitysOwnerLabel(t *testing.T) {
	fenced, inner, _ := fencedMem(t, "citadel")
	cases := []struct {
		name string
		in   beads.Bead
		want []string
	}{
		{"unlabeled permanent row", beads.Bead{Title: "poured"}, []string{"owner:citadel"}},
		{"row with other labels", beads.Bead{Title: "poured", Labels: []string{"x"}}, []string{"x", "owner:citadel"}},
		{"row that names an owner", beads.Bead{Title: "poured", Labels: []string{"owner:jadegate"}}, []string{"owner:jadegate"}},
		{"ephemeral row", beads.Bead{Title: "wisp", Ephemeral: true}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			created, err := fenced.Create(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			got, err := inner.Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got.Labels, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("labels = %q, want %q", got.Labels, tc.want)
			}
			if strings.Join(created.Labels, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("returned labels = %q, want %q", created.Labels, tc.want)
			}
		})
	}
}

// A child created through the fence keeps its parent's owner — the store's
// own rule (federation.OwnerLabelForChild): a create under another city's
// bead never lands a second owner or the wrong lane. Holds for a store
// create and for a create inside a fenced transaction.
func TestAutocloseFenceCreateKeepsTheParentsOwner(t *testing.T) {
	fenced, inner, _ := fencedMem(t, "citadel")
	theirs, _ := inner.Create(beads.Bead{Title: "their root", Labels: []string{"owner:jadegate"}})
	mine, _ := inner.Create(beads.Bead{Title: "my root", Labels: []string{"owner:citadel"}})
	unowned, _ := inner.Create(beads.Bead{Title: "unowned root"})
	cases := []struct {
		name   string
		parent string
		want   string
	}{
		{"their parent", theirs.ID, "owner:jadegate"},
		{"my parent", mine.ID, "owner:citadel"},
		{"unowned parent", unowned.ID, "owner:citadel"},
		{"no parent", "", "owner:citadel"},
	}
	for _, via := range []string{"store", "tx"} {
		for _, tc := range cases {
			t.Run(via+"/"+tc.name, func(t *testing.T) {
				b := beads.Bead{Title: "child", ParentID: tc.parent}
				var created beads.Bead
				var err error
				if via == "store" {
					created, err = fenced.Create(b)
				} else {
					err = fenced.Tx("create", func(tx beads.Tx) error {
						var txErr error
						created, txErr = tx.Create(b)
						return txErr
					})
				}
				if err != nil {
					t.Fatal(err)
				}
				got, err := inner.Get(created.ID)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Join(got.Labels, ",") != tc.want {
					t.Fatalf("labels = %q, want %q", got.Labels, tc.want)
				}
			})
		}
	}
}

// A lifecycle command (approve/iterate/stop) pours the next iteration BEFORE
// it writes the root; on another city's root it must refuse first, leaving no
// child behind.
func TestConvergenceLifecycleNeverPoursForAnotherCitysRoot(t *testing.T) {
	cr, store := setupConvergenceRuntime(t)
	cr.cfg.Federation.Identity = "jadegate"
	scope := cr.newConvergenceScope("", cr.fenceMaintenance(store, "convergence"), cr.cityPath, []string{sharedTestFormulaDir})
	cr.convScopes[""] = scope
	root, err := store.Create(beads.Bead{
		Title:  "loop",
		Type:   "convergence",
		Status: "in_progress",
		Labels: []string{"owner:citadel"},
		Metadata: map[string]string{
			convergence.FieldFormula:       "test-formula",
			convergence.FieldTarget:        "test-agent",
			convergence.FieldState:         convergence.StateWaitingManual,
			convergence.FieldMaxIterations: "5",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	reply := cr.handleConvergenceLifecycle(context.Background(), scope, "iterate", root.ID, "tester")
	if reply.Error == "" {
		t.Fatalf("iterate on another city's root succeeded")
	}
	children, err := store.List(beads.ListQuery{ParentID: root.ID, IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 0 {
		t.Fatalf("children = %d, want 0 (a refused root must pour nothing); error=%q", len(children), reply.Error)
	}
}

// Closed-history retention deletes old tracking rows past the retained
// floor: only the row's own city does, and the other city's refusals are
// skips — never a sweep error that fails the scheduled command — and never
// counted toward the bulk-delete confirm gate.
func TestClosedOrderTrackingRetentionSkipsForeignRowsWithoutError(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	seed := func() []beads.Bead {
		var out []beads.Bead
		for i := range 12 {
			out = append(out, beads.Bead{
				ID: "track-" + string(rune('a'+i)), Title: "order:x", Status: "closed",
				Labels:    []string{"order-run:x", labelOrderTracking, "owner:citadel"},
				CreatedAt: old.Add(time.Duration(i) * time.Minute), UpdatedAt: old.Add(time.Duration(i) * time.Minute),
			})
		}
		return out
	}
	policy := orderTrackingRetentionPolicy{deleteAfterClose: time.Hour, retainLast: 10}
	for _, tc := range []struct {
		city string
		want int
	}{{"citadel", 2}, {"jadegate", 0}} {
		t.Run("city="+tc.city, func(t *testing.T) {
			gate := autocloseGate{identity: tc.city}
			var log bytes.Buffer
			full := gate.fence(beads.NewMemStoreFrom(100, seed(), nil), &log, "retention")
			bounded := gate.fence(beads.NewMemStoreFrom(100, seed(), nil), &log, "retention")
			counted := gate.fence(beads.NewMemStoreFrom(100, seed(), nil), &log, "retention")
			n, err := sweepClosedOrderTrackingRetention(full, now, policy, nil)
			if err != nil || n != tc.want {
				t.Fatalf("retention = (%d, %v), want (%d, nil); log=%q", n, err, tc.want, log.String())
			}
			nb, err := sweepClosedOrderTrackingRetentionBounded(bounded, now, policy, nil, 5)
			if err != nil || nb != tc.want {
				t.Fatalf("bounded retention = (%d, %v), want (%d, nil); log=%q", nb, err, tc.want, log.String())
			}
			c, err := countClosedOrderTrackingRetentionEligible([]beads.Store{counted}, now, policy, nil)
			if err != nil || c != tc.want {
				t.Fatalf("eligible count = (%d, %v), want (%d, nil)", c, err, tc.want)
			}
		})
	}
}

// Assigned-work canonicalization rewrites the assignee and route of work
// pre-assigned to a legacy bound identity with no live session behind it —
// the same automatic writer as the unassigned pass, over the same fenced
// stores.
func TestAssignedWorkCanonicalizationTwoCitiesOnlyOwnerWrites(t *testing.T) {
	const legacy = "rig-A/gc.planner"
	const canonical = "rig-A/planner"
	for _, tc := range twoCityCases {
		t.Run("city="+tc.city, func(t *testing.T) {
			cfg := legacyBoundRecoveryConfig()
			cfg.Federation.Identity = tc.city
			wb := workBead("wb-1", legacy, legacy, "open", 5)
			wb.Labels = []string{"owner:citadel"}
			mem := beads.NewMemStoreFrom(0, []beads.Bead{wb}, nil)
			var log bytes.Buffer
			canonicalizeLegacyBoundAssignedWork(cfg, []beads.Bead{wb}, fenceDemandPrepStores(cfg, []beads.Store{mem}, &log), newSessionBeadSnapshot(nil), &log)
			got, _ := mem.Get("wb-1")
			if rewritten := got.Assignee == canonical; rewritten != tc.wantClosed {
				t.Fatalf("assignee = %q, rewritten=%v want %v; log=%q", got.Assignee, rewritten, tc.wantClosed, log.String())
			}
		})
	}
}

// bd resolves a missing id to a DIFFERENT bead by substring and reports it
// as ErrIDCollision, which wraps ErrNotFound. That is not an absent row the
// write may fall through to: the write would land on the other, possibly
// foreign, bead. The fence refuses it.
func TestAutocloseFenceRefusesAnIDCollision(t *testing.T) {
	inner := newGCStore([]beads.Bead{{ID: "x", Status: "open", Type: "task", Labels: []string{"owner:citadel"}}})
	inner.getErrors["x"] = fmt.Errorf("getting bead %q (resolved to %q): %w", "x", "x-other", beads.ErrIDCollision)
	var log bytes.Buffer
	store := (autocloseGate{identity: "citadel"}).fence(inner, &log, "site")
	if err := store.Close("x"); !errors.Is(err, errAutomaticWriteFenced) {
		t.Fatalf("Close = %v, want errAutomaticWriteFenced", err)
	}
	delete(inner.getErrors, "x")
	if got, _ := inner.Get("x"); got.Status != "open" {
		t.Fatalf("row written through an id collision: %q", got.Status)
	}
	if !strings.Contains(log.String(), "cross-city-fence refused bead=x") || !strings.Contains(log.String(), "id_collision") {
		t.Fatalf("log = %q, want an id_collision refusal", log.String())
	}
}

// A row created inside a fenced transaction is permitted for the rest of
// it only when this city may maintain it: a row that names another city's
// owner, or inherits one from its parent, is refused like any other row.
func TestAutocloseFenceTxDoesNotPermitAForeignRowItCreated(t *testing.T) {
	fenced, inner, _ := fencedMem(t, "citadel")
	theirs, _ := inner.Create(beads.Bead{Title: "their root", Labels: []string{"owner:jadegate"}})
	cases := []struct {
		name string
		bead beads.Bead
	}{
		{"names another owner", beads.Bead{Title: "child", Labels: []string{"owner:jadegate"}}},
		{"inherits another owner", beads.Bead{Title: "child", ParentID: theirs.ID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var created beads.Bead
			err := fenced.Tx("create then close", func(tx beads.Tx) error {
				c, err := tx.Create(tc.bead)
				if err != nil {
					return err
				}
				created = c
				return tx.Close(c.ID)
			})
			if !errors.Is(err, errAutomaticWriteFenced) {
				t.Fatalf("Tx err = %v, want errAutomaticWriteFenced", err)
			}
			if created.ID != "" {
				if got, getErr := inner.Get(created.ID); getErr == nil && got.Status == "closed" {
					t.Fatalf("foreign row %s closed by the transaction that created it", created.ID)
				}
			}
		})
	}
	// Control: a row this city may maintain is permitted.
	err := fenced.Tx("create then close", func(tx beads.Tx) error {
		c, err := tx.Create(beads.Bead{Title: "mine"})
		if err != nil {
			return err
		}
		return tx.Close(c.ID)
	})
	if err != nil {
		t.Fatalf("own row: %v", err)
	}
}

// The dry-run twin of the nudge/mail sweep counts what the sweep would
// close: behind the fence that is this city's rows only, found behind a
// foreign backlog just as the sweep finds them.
func TestNudgeMailDryRunCountsMatchTheSweepBehindAForeignBacklog(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	old := now.Add(-3 * time.Hour)
	var seed []beads.Bead
	for i := range 20 {
		n := nudgeSeed("nudge-f"+string(rune('a'+i)), "nudge-f"+string(rune('a'+i)), old)
		n.Labels = append(n.Labels, "owner:jadegate")
		m := mailSeed("mail-f"+string(rune('a'+i)), old)
		m.Labels = append(m.Labels, "owner:jadegate")
		seed = append(seed, n, m)
	}
	mine := nudgeSeed("nudge-mine", "nudge-mine", old.Add(time.Minute))
	mine.Labels = append(mine.Labels, "owner:citadel")
	mail := mailSeed("mail-mine", old.Add(time.Minute))
	mail.Labels = append(mail.Labels, "owner:citadel")
	seed = append(seed, mine, mail)
	fenced := (autocloseGate{identity: "citadel"}).fence(beads.NewMemStoreFrom(100, seed, nil), io.Discard, "watchdog")

	counts, err := countStaleNudgeMail(beads.NudgesStore{Store: fenced}, beads.MailStore{Store: fenced}, &nudgequeue.State{}, now, 10*time.Minute, 30*time.Minute, 5)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if counts.NudgeClosed != 1 || counts.MailClosed != 1 {
		t.Fatalf("dry-run would close nudge=%d mail=%d, want 1 each (what the sweep closes)", counts.NudgeClosed, counts.MailClosed)
	}
}

// parentReadCountingStore counts the reads a wrapper makes through it.
type parentReadCountingStore struct {
	beads.Store
	gets int
}

func (s *parentReadCountingStore) Get(id string) (beads.Bead, error) {
	s.gets++
	return s.Store.Get(id)
}

// The policy wrapper's transactional Create reads the parent to settle the
// child's lane. Inside a native transaction that read can wait on the
// store's own lock, so when the child already names its owner (the fence
// stamps it before the transaction opens) there is nothing left to read:
// no parent read under the lock.
func TestOwnerStampingTxSkipsTheParentReadWhenTheChildNamesItsOwner(t *testing.T) {
	inner := beads.NewMemStore()
	parent, err := inner.Create(beads.Bead{Title: "root", Labels: []string{"owner:jadegate"}})
	if err != nil {
		t.Fatal(err)
	}
	counting := &parentReadCountingStore{Store: inner}
	policy := wrapStoreWithBeadPolicies(counting, &config.City{Federation: config.FederationConfig{Identity: "citadel"}})
	counting.gets = 0
	err = policy.Tx("create", func(tx beads.Tx) error {
		_, err := tx.Create(beads.Bead{Title: "child", ParentID: parent.ID, Labels: []string{"owner:jadegate"}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if counting.gets != 0 {
		t.Fatalf("the transaction read the parent %d time(s) although the child names its owner", counting.gets)
	}
	// Control: a child that names no owner still reads its parent to inherit.
	err = policy.Tx("create", func(tx beads.Tx) error {
		c, err := tx.Create(beads.Bead{Title: "child", ParentID: parent.ID})
		if err != nil {
			return err
		}
		if strings.Join(c.Labels, ",") != "owner:jadegate" {
			return fmt.Errorf("labels = %q, want the parent's owner", c.Labels)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if counting.gets == 0 {
		t.Fatalf("a child that names no owner must read its parent")
	}
}

// A create that supplies its own id, on a backend that honors it, must not
// keep the permit the recording pass granted that id while it was still
// absent: the ownership decision REPLACES the earlier permit.
func TestAutocloseFenceTxExplicitIDCreateReplacesTheRecordingPermit(t *testing.T) {
	inner := beads.NewMemStore()
	inner.HonorExplicitIDs = true
	fenced := (autocloseGate{identity: "citadel"}).fence(inner, io.Discard, "site")
	err := fenced.Tx("create x then close x", func(tx beads.Tx) error {
		if _, err := tx.Create(beads.Bead{ID: "x", Title: "theirs", Labels: []string{"owner:jadegate"}}); err != nil {
			return err
		}
		return tx.Close("x")
	})
	if !errors.Is(err, errAutomaticWriteFenced) {
		t.Fatalf("Tx err = %v, want errAutomaticWriteFenced", err)
	}
	if got, err := inner.Get("x"); err == nil && got.Status == "closed" {
		t.Fatalf("another city's row x was closed by the transaction that created it")
	}
}

// An ownerless permanent child whose parent cannot be read has no proven
// lane: the fence refuses the create instead of letting a lower layer stamp
// it from a cached or defaulted answer. A child that names its owner, or an
// ephemeral row, still passes. Store create and Tx create alike.
func TestAutocloseFenceCreateRefusesAnOwnerlessChildOfAnUnreadableParent(t *testing.T) {
	inner := newGCStore([]beads.Bead{{ID: "p", Status: "open", Type: "task", Labels: []string{"owner:jadegate"}}})
	inner.getErrors["p"] = errors.New("store unreachable")
	fenced := (autocloseGate{identity: "citadel"}).fence(inner, io.Discard, "site")
	if _, err := fenced.Create(beads.Bead{Title: "child", ParentID: "p"}); !errors.Is(err, errAutomaticWriteFenced) {
		t.Fatalf("ownerless child of an unreadable parent: err = %v, want errAutomaticWriteFenced", err)
	}
	if _, err := fenced.Create(beads.Bead{Title: "child", ParentID: "p", Labels: []string{"owner:jadegate"}}); err != nil {
		t.Fatalf("child naming its owner: %v", err)
	}
	if _, err := fenced.Create(beads.Bead{Title: "wisp", ParentID: "p", Ephemeral: true}); err != nil {
		t.Fatalf("ephemeral child: %v", err)
	}
	err := fenced.Tx("create", func(tx beads.Tx) error {
		_, err := tx.Create(beads.Bead{Title: "child", ParentID: "p"})
		return err
	})
	if !errors.Is(err, errAutomaticWriteFenced) {
		t.Fatalf("Tx: ownerless child of an unreadable parent: err = %v, want errAutomaticWriteFenced", err)
	}
}

// A parent created earlier in the same fenced transaction is known to it
// (a cooked molecule creates its root and then its steps in one Tx): the
// child inherits the lane and both stay permitted.
func TestAutocloseFenceTxKnowsTheParentsItCreated(t *testing.T) {
	fenced, inner, _ := fencedMem(t, "citadel")
	var childID string
	err := fenced.Tx("cook", func(tx beads.Tx) error {
		root, err := tx.Create(beads.Bead{Title: "root"})
		if err != nil {
			return err
		}
		child, err := tx.Create(beads.Bead{Title: "step", ParentID: root.ID})
		if err != nil {
			return err
		}
		childID = child.ID
		return tx.Close(child.ID)
	})
	if err != nil {
		t.Fatalf("cook: %v", err)
	}
	got, err := inner.Get(childID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Labels, ",") != "owner:citadel" || got.Status != "closed" {
		t.Fatalf("child labels = %q status = %q, want owner:citadel closed", got.Labels, got.Status)
	}
}
