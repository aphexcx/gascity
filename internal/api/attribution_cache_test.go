package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

type attributionPageStore struct {
	*beads.MemStore
	calls        atomic.Int64
	gate         chan struct{}
	err          error
	ignoreCancel bool
}

func (s *attributionPageStore) ReadMetadataPathPage(ctx context.Context, q beads.MetadataPathQuery) (beads.MetadataPathPage, error) {
	s.calls.Add(1)
	if s.gate != nil {
		if s.ignoreCancel {
			<-s.gate
		} else {
			select {
			case <-s.gate:
			case <-ctx.Done():
				return beads.MetadataPathPage{}, ctx.Err()
			}
		}
	}
	if s.err != nil {
		return beads.MetadataPathPage{}, s.err
	}
	return s.MemStore.ReadMetadataPathPage(ctx, q)
}

func TestAttributionMemoCoalescesConcurrentReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, _, mem := splitClassState(t)
		store := &attributionPageStore{MemStore: mem, gate: make(chan struct{})}
		st.sessionsBeadStore = store
		srv := &Server{state: st}
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got := srv.agentAttributionSnapshot(context.Background(), st.cfg)
				if !got.Complete {
					t.Errorf("snapshot incomplete: %+v", got)
				}
			}()
		}
		synctest.Wait()
		if got := store.calls.Load(); got != 1 {
			t.Fatalf("in-flight backing reads=%d want 1", got)
		}
		close(store.gate)
		wg.Wait()
		if got := store.calls.Load(); got != 2 {
			t.Fatalf("backing calls=%d want one type/label union", got)
		}
	})
}

func TestAttributionMemoUsesRequestConfigSnapshot(t *testing.T) {
	st, _, mem := splitClassState(t)
	cfg := &config.City{
		Agents: []config.Agent{{Name: "observer", Dir: "demo", MaxActiveSessions: intPtr(1)}},
		Rigs:   []config.Rig{{Name: "demo", Path: "/previous-rig"}},
	}
	st.cfg = &config.City{Agents: cfg.Agents, Rigs: []config.Rig{{Name: "demo", Path: "/reloaded-rig"}}}
	if _, err := mem.Create(beads.Bead{Title: "retained owner", Type: "session", Metadata: map[string]string{"work_dir": "/previous-rig", "template": "removed"}}); err != nil {
		t.Fatal(err)
	}
	got := (&Server{state: st}).agentAttributionSnapshot(context.Background(), cfg)
	if !got.Complete || len(got.Infos) != 1 {
		t.Fatalf("request snapshot owner lost after reload: %+v", got)
	}
}

func TestAttributionMemoExpiresAndSuppressesWarnings(t *testing.T) {
	st, _, mem := splitClassState(t)
	store := &attributionPageStore{MemStore: mem, err: errors.New("history incomplete")}
	st.sessionsBeadStore = store
	srv := &Server{state: st}
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	srv.attribution.now = func() time.Time { return now }
	warnings := 0
	srv.attribution.warn = func(string) { warnings++ }
	for i := 0; i < 3; i++ {
		if got := srv.agentAttributionSnapshot(context.Background(), st.cfg); got.Complete {
			t.Fatal("failed history authorized attribution")
		}
	}
	if store.calls.Load() != 1 || warnings != 1 {
		t.Fatalf("calls=%d warnings=%d", store.calls.Load(), warnings)
	}
	now = now.Add(3 * time.Second)
	store.err = nil
	if got := srv.agentAttributionSnapshot(context.Background(), st.cfg); !got.Complete {
		t.Fatal("expired failure was not retried")
	}
	if store.calls.Load() != 3 || warnings != 1 {
		t.Fatalf("calls=%d warnings=%d", store.calls.Load(), warnings)
	}
	now = now.Add(3 * time.Second)
	store.err = errors.New("history incomplete again")
	_ = srv.agentAttributionSnapshot(context.Background(), st.cfg)
	if warnings != 2 {
		t.Fatalf("warnings=%d want 2 windows", warnings)
	}
	st.cfg.Agents[0].WorkDir = "/another-directory"
	_ = srv.agentAttributionSnapshot(context.Background(), st.cfg)
	if warnings != 2 {
		t.Fatalf("config change repeated warning inside window: %d", warnings)
	}
}

func TestAttributionMemoRetainsSlotUntilTimedOutReadEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st, _, mem := splitClassState(t)
		store := &attributionPageStore{MemStore: mem, gate: make(chan struct{}), ignoreCancel: true}
		st.sessionsBeadStore = store
		srv := &Server{state: st}
		now := time.Now()
		srv.attribution.now = func() time.Time { return now }
		if got := srv.agentAttributionSnapshot(context.Background(), st.cfg); got.Complete {
			t.Fatal("timeout authorized absence")
		}
		now = now.Add(10 * time.Second)
		if got := srv.agentAttributionSnapshot(context.Background(), st.cfg); got.Complete {
			t.Fatal("stuck read authorized absence")
		}
		if store.calls.Load() != 1 {
			t.Fatal("overlapping read after timeout")
		}
		close(store.gate)
		synctest.Wait()
		if got := srv.agentAttributionSnapshot(context.Background(), st.cfg); !got.Complete {
			t.Fatal("completed backend did not release slot")
		}
	})
}

func TestAgentAttributionWithholdsIncompleteHistory(t *testing.T) {
	st := newFakeState(t)
	st.cfg.Workspace.Provider = "claude"
	st.cfg.Agents = []config.Agent{{Name: "observer", Provider: "claude", WorkDir: st.cityPath, MaxActiveSessions: intPtr(1)}}
	st.sessionsBeadStore = &attributionPageStore{MemStore: beads.NewMemStore(), err: &beads.PartialResultError{Op: "history page", Err: errors.New("archive truncated")}}
	if err := st.sp.Start(context.Background(), agentSessionName(st.cityName, "observer", st.cfg.Workspace.SessionTemplate), runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	search := t.TempDir()
	dir := filepath.Join(search, sessionlog.ProjectSlug(st.cityPath))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "retained.jsonl"), []byte("{\"type\":\"assistant\",\"message\":{\"role\":\"assistant\",\"model\":\"other-session-model\"}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := New(st)
	srv.sessionLogSearchPaths = []string{search}
	handler := newTestCityHandlerWith(t, st, srv)
	for _, path := range []string{"/agents", "/agent/observer"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(st, path), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body)
		}
		var rows []agentResponse
		if path == "/agents" {
			var body ListBody[agentResponse]
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			rows = body.Items
		} else {
			var body agentResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			rows = []agentResponse{body}
		}
		if len(rows) != 1 || rows[0].Model != "" {
			t.Fatalf("%s attributed incomplete history: %+v", path, rows)
		}
	}
}

func TestAgentAttributionRigQualifiedRemovedOwner(t *testing.T) {
	for _, tc := range []struct {
		name       string
		template   string
		incomplete bool
		wantModel  string
	}{
		{name: "closed removed owner", template: "alpha/removed"},
		{name: "own history", template: "alpha/reviewer", wantModel: "claude-opus-4-5-20251101"},
		{name: "incomplete census", template: "alpha/removed", incomplete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeState(t)
			rigDir := filepath.Join(st.cityPath, "alpha")
			st.cfg.Workspace.Provider = "claude"
			st.cfg.Agents = []config.Agent{{Name: "reviewer", Scope: "rig", Provider: "claude", MaxActiveSessions: intPtr(1)}}
			st.cfg.Rigs = []config.Rig{{Name: "alpha", Path: rigDir}}
			store := &attributionPageStore{MemStore: beads.NewMemStore()}
			st.sessionsBeadStore = store
			if _, err := store.Create(beads.Bead{
				Title: "retained session", Type: "session", Status: "closed", Labels: []string{"gc:session"},
				Metadata: map[string]string{"work_dir": "alpha/../alpha", "template": tc.template, "provider": "claude"},
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.sp.Start(context.Background(), agentSessionName(st.cityName, "alpha/reviewer", st.cfg.Workspace.SessionTemplate), runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			search := t.TempDir()
			dir := filepath.Join(search, sessionlog.ProjectSlug(rigDir))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			line := `{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-5-20251101","stop_reason":"tool_use","content":[{"type":"tool_use"}],"usage":{"input_tokens":10000}}}` + "\n"
			if err := os.WriteFile(filepath.Join(dir, "retained.jsonl"), []byte(line), 0o600); err != nil {
				t.Fatal(err)
			}
			srv := New(st)
			srv.sessionLogSearchPaths = []string{search}
			// A list census covers the unexpanded generic template. A qualified
			// GET must not treat that cached result as proof about the rig root.
			if got := srv.agentAttributionSnapshot(context.Background(), st.cfg); !got.Complete {
				t.Fatal("initial census incomplete")
			}
			initialCalls := store.calls.Load()
			if tc.incomplete {
				store.err = &beads.PartialResultError{Op: "history page", Err: errors.New("archive truncated")}
			}
			handler := newTestCityHandlerWith(t, st, srv)
			for i := 0; i < 2; i++ {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(st, "/agent/alpha/reviewer"), nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
				}
				var body agentResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body.Model != tc.wantModel {
					t.Errorf("model=%q want %q", body.Model, tc.wantModel)
				}
				if tc.wantModel == "" {
					if body.ContextPct != nil || body.ContextWindow != nil || body.Activity != "" {
						t.Errorf("attributed another session's context or activity: %+v", body)
					}
				} else if body.ContextPct == nil || body.ContextWindow == nil || body.Activity != "in-turn" {
					t.Errorf("unique owner's enrichment missing: %+v", body)
				}
			}
			srv.attribution.mu.Lock()
			snapshot := srv.attribution.value
			srv.attribution.mu.Unlock()
			if snapshot.Complete == tc.incomplete {
				t.Errorf("census Complete=%v, incomplete=%v", snapshot.Complete, tc.incomplete)
			}
			if !tc.incomplete && (len(snapshot.Infos) != 1 || snapshot.Infos[0].Template != tc.template) {
				t.Errorf("rig-qualified history omitted: %+v", snapshot)
			}
			wantCalls := initialCalls + 2 // One new type/label union, then a memo hit.
			if tc.incomplete {
				wantCalls = initialCalls + 1 // Failed first page is also memoized.
			}
			if got := store.calls.Load(); got != wantCalls {
				t.Errorf("backing calls=%d want %d", got, wantCalls)
			}
		})
	}
}
