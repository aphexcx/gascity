package api

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

const attributionMemoTTL = 3 * time.Second

type attributionMemo struct {
	mu        sync.Mutex
	key       string
	value     session.WorkDirOwners
	expires   time.Time
	warnAfter time.Time
	flight    *attributionFlight
	now       func() time.Time
	warn      func(string)
}

type attributionFlight struct {
	ready    chan struct{}
	timedOut bool
}

func (m *attributionMemo) timeNow() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// agentAttributionSnapshot shares one ownership census across agent list and
// GET. Query all configured singleton directories so either endpoint can reuse
// it; historical owners are never restricted to currently configured templates.
func (s *Server) agentAttributionSnapshot(ctx context.Context, cfg *config.City) session.WorkDirOwners {
	if ctx.Err() != nil {
		return session.WorkDirOwners{}
	}
	var paths []string
	cityPath := s.state.CityPath()
	cityName := workdirutil.CityName(cityPath, cfg)
	for _, a := range cfg.Agents {
		if !isMultiSessionAgent(a) {
			if path := workdirutil.ResolveWorkDirPath(cityPath, cityName, a.QualifiedName(), a, cfg.Rigs); path != "" {
				paths = append(paths, path)
			}
		}
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	key := s.state.CityPath() + "\x00" + strings.Join(paths, "\x00")
	m := &s.attribution
	for {
		m.mu.Lock()
		if m.key == key && m.timeNow().Before(m.expires) {
			value := m.value
			m.mu.Unlock()
			return value
		}
		flight := m.flight
		if flight != nil && flight.timedOut {
			// A provider that ignores cancellation still owns the single query
			// slot. Never pile another read onto that stuck backend.
			m.mu.Unlock()
			return session.WorkDirOwners{}
		}
		if flight == nil {
			flight = &attributionFlight{ready: make(chan struct{})}
			m.flight = flight
			go s.loadAgentAttribution(context.WithoutCancel(ctx), key, paths, flight)
		}
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return session.WorkDirOwners{}
		case <-flight.ready:
		}
	}
}

func (s *Server) loadAgentAttribution(ctx context.Context, key string, paths []string, flight *attributionFlight) {
	readCtx, cancel := context.WithTimeout(ctx, statusStoreReadTimeout)
	defer cancel()
	type result struct {
		owners session.WorkDirOwners
		err    error
	}
	done := make(chan result, 1)
	go func() {
		sessions := s.state.SessionsBeadStore()
		if sessions.Store == nil {
			if s.state.CityBeadStore() != nil {
				done <- result{err: fmt.Errorf("session-class bead store unavailable")}
			} else {
				done <- result{owners: session.WorkDirOwners{Complete: true}}
			}
			return
		}
		if scoped, err := s.state.ScopedStoreLike(readCtx, sessions.Store); err != nil {
			done <- result{err: fmt.Errorf("resolving scoped store: %w", err)}
			return
		} else if scoped != nil {
			sessions = beads.SessionStore{Store: scoped}
		}
		owners, err := session.NewStore(sessions).ListWorkDirOwners(readCtx, s.state.CityPath(), paths)
		done <- result{owners: owners, err: err}
	}()
	var got result
	timedOut := false
	select {
	case got = <-done:
	case <-readCtx.Done():
		got.err = fmt.Errorf("loading session ownership: %w", readCtx.Err())
		timedOut = true
	}
	m := &s.attribution
	m.mu.Lock()
	if got.err != nil || !got.owners.Complete {
		got.owners = session.WorkDirOwners{}
		if !m.timeNow().Before(m.warnAfter) {
			message := fmt.Sprintf("api: agent session attribution unavailable: %v", got.err)
			if m.warn != nil {
				m.warn(message)
			} else {
				log.Print(message)
			}
			m.warnAfter = m.timeNow().Add(attributionMemoTTL)
		}
	}
	m.key, m.value, m.expires = key, got.owners, m.timeNow().Add(attributionMemoTTL)
	flight.timedOut = timedOut
	if !timedOut {
		m.flight = nil
	}
	close(flight.ready)
	m.mu.Unlock()
	if timedOut {
		// Retain the slot until the actual read ends, even after its result
		// has timed out and been memoized as incomplete.
		<-done
		m.mu.Lock()
		m.flight = nil
		m.mu.Unlock()
	}
}
