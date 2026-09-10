package main

import (
	"bytes"
	"context"
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
