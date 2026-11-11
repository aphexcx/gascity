package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
)

// The fixture preserves the reported store sizes, not private city contents.
// FileStore measures local scans; it does not model bd startup or Dolt latency.
type closedPoolScaleFixture struct {
	cfg      *config.City
	cityPath string
	stores   map[string]*closedPoolMeasuredStore
}

type closedPoolMeasuredStore struct {
	beads.Store
	mu            sync.Mutex
	queries       int
	metadataCalls int
	metadataTime  time.Duration
	otherTime     time.Duration
	snapshotTime  time.Duration
}

func (s *closedPoolMeasuredStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	start := time.Now()
	rows, err := s.Store.List(q)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries++
	switch {
	case len(q.Metadata) > 0:
		s.metadataCalls++
		s.metadataTime += time.Since(start)
	case q.IncludeClosed:
		s.snapshotTime += time.Since(start)
	default:
		s.otherTime += time.Since(start)
	}
	return rows, err
}

func newClosedPoolScaleFixture(tb testing.TB) *closedPoolScaleFixture {
	tb.Helper()
	f := &closedPoolScaleFixture{
		cfg:      &config.City{Workspace: config.Workspace{Name: "scale-city"}},
		cityPath: tb.TempDir(), stores: make(map[string]*closedPoolMeasuredStore),
	}
	for i := range 155 {
		a := config.Agent{Name: fmt.Sprintf("worker-%03d", i), MaxActiveSessions: intPtr(2)}
		if i%7 != 0 {
			a.Dir = fmt.Sprintf("rig-%d", i%7)
		}
		f.cfg.Agents = append(f.cfg.Agents, a)
	}
	for i := range 7 {
		path, openCount, closedCount := f.cityPath, 9400, 8460
		if i > 0 {
			name := fmt.Sprintf("rig-%d", i)
			path = filepath.Join(f.cityPath, name)
			f.cfg.Rigs = append(f.cfg.Rigs, config.Rig{Name: name, Path: path})
			openCount, closedCount = 60, 40
			if i == 1 {
				openCount, closedCount = 480, 320
			}
		}
		rows := make([]beads.Bead, 0, openCount+closedCount)
		for n := range openCount + closedCount {
			status := "open"
			if n >= openCount {
				status = "closed"
			}
			row := beads.Bead{ID: fmt.Sprintf("store%d-%05d", i, n), Type: "task", Status: status}
			if n%20 == 0 {
				row.Type = session.BeadType
				row.Labels = []string{session.LabelSession}
				row.Metadata = map[string]string{"session_name": row.ID, "session_origin": "manual", "state": "asleep"}
			}
			rows = append(rows, row)
		}
		data, err := json.Marshal(struct {
			Seq   int          `json:"seq"`
			Beads []beads.Bead `json:"beads"`
		}{len(rows), rows})
		if err != nil {
			tb.Fatal(err)
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			tb.Fatal(err)
		}
		file := filepath.Join(path, "beads.json")
		if err := os.WriteFile(file, data, 0o600); err != nil {
			tb.Fatal(err)
		}
		store, err := beads.OpenFileStore(fsys.OSFS{}, file)
		if err != nil {
			tb.Fatal(err)
		}
		f.stores[path] = &closedPoolMeasuredStore{Store: store}
	}
	return f
}

func (f *closedPoolScaleFixture) open(path string) (beads.Store, error) {
	store, ok := f.stores[path]
	if !ok {
		return nil, fmt.Errorf("unknown fixture store %q", path)
	}
	return store, nil
}

func (f *closedPoolScaleFixture) measure(tb testing.TB, count int) {
	tb.Helper()
	start := time.Now()
	warnings, err := closedPoolSessionNameWarnings(f.cfg, f.cityPath, f.cfg.Agents[:count], f.stores[f.cityPath], f.open)
	elapsed := time.Since(start)
	if err != nil || len(warnings) != 0 {
		tb.Fatalf("warnings=%v err=%v", warnings, err)
	}
	var queries, metadataCalls int
	var metadataTime, otherTime, snapshotTime time.Duration
	for _, s := range f.stores {
		s.mu.Lock()
		queries += s.queries
		metadataCalls += s.metadataCalls
		metadataTime += s.metadataTime
		otherTime += s.otherTime
		snapshotTime += s.snapshotTime
		s.queries, s.metadataCalls, s.metadataTime, s.otherTime = 0, 0, 0, 0
		s.snapshotTime = 0
		s.mu.Unlock()
	}
	tb.Logf("agents=%d total=%s store_calls=%d metadata_calls=%d", count, elapsed, queries, metadataCalls)
	tb.Logf("agents=%d metadata_time=%s census_store_time=%s reservation_snapshot_time=%s", count, metadataTime, otherTime, snapshotTime)
}

func TestClosedPoolSessionNameScaleMeasurement(t *testing.T) {
	f := newClosedPoolScaleFixture(t)
	for _, count := range []int{1, 155} {
		t.Run(fmt.Sprintf("agents_%d", count), func(t *testing.T) { f.measure(t, count) })
	}
}

func BenchmarkClosedPoolSessionNameWarnings(b *testing.B) {
	for _, count := range []int{1, 155} {
		b.Run(fmt.Sprintf("agents_%d", count), func(b *testing.B) {
			f := newClosedPoolScaleFixture(b)
			b.ResetTimer()
			for range b.N {
				f.measure(b, count)
			}
		})
	}
}

type closedPoolLatencyStore struct {
	beads.Store
}

func (s closedPoolLatencyStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	// Deliberate simulated backend cost, not a measurement of Dolt. synctest
	// advances it virtually so the regression does not cost a minute of CI.
	<-time.After(100 * time.Millisecond)
	return s.Store.List(q)
}

func TestClosedPoolDoctorScaleBudget(t *testing.T) {
	f := newClosedPoolScaleFixture(t)
	budget, err := newDoctorCmd(io.Discard, io.Discard).Flags().GetDuration("check-timeout")
	if err != nil {
		t.Fatal(err)
	}
	for _, store := range f.stores {
		store.Store = closedPoolLatencyStore{Store: store.Store}
	}
	synctest.Test(t, func(t *testing.T) {
		d := &doctor.Doctor{CheckTimeout: budget, UseCheckTimeouts: true}
		d.Register(&sessionModelDoctorCheck{cfg: f.cfg, cityPath: f.cityPath, newStore: f.open})
		defer d.Wait()
		start := time.Now()
		report := d.RunCollect(&doctor.CheckContext{}, false)
		elapsed := time.Since(start)
		if len(report.Results) != 1 || report.Results[0].TimedOut || elapsed >= budget {
			t.Fatalf("session-model exceeded default budget %s: elapsed=%s results=%+v", budget, elapsed, report.Results)
		}
		t.Logf("session-model elapsed=%s default_budget=%s (100ms simulated latency per List)", elapsed, budget)
	})
}
