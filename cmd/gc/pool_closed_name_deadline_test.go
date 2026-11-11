package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

type closedPoolBlockedStore struct {
	beads.Store
	release <-chan struct{}
}

func (s closedPoolBlockedStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	<-s.release
	return s.Store.List(q)
}

func TestClosedPoolSlingDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
		cfg := &config.City{Agents: []config.Agent{a}}
		release := make(chan struct{})
		store := closedPoolBlockedStore{Store: beads.NewMemStore(), release: release}
		go func() {
			// Unblock only after the asserted deadline. The delayed release
			// lets this test fail cleanly on the synchronous implementation.
			<-time.After(10 * time.Second)
			close(release)
		}()
		var stderr bytes.Buffer
		start := time.Now()
		printClosedPoolSessionNameWarnings(slingOpts{Target: a}, slingDeps{Cfg: cfg, Store: store}, &stderr)
		elapsed := time.Since(start)
		before := stderr.String()
		<-release
		synctest.Wait()
		if elapsed > 3*time.Second {
			t.Errorf("blocked advisory delayed sling by %s, want at most 3s", elapsed)
		}
		if strings.Count(before, "\n") != 1 || !strings.Contains(before, "pool-name check was skipped") {
			t.Errorf("deadline must print one skipped warning, got %q", before)
		}
		if stderr.String() != before {
			t.Errorf("diagnostic wrote after sling returned: %q", stderr.String())
		}
	})
}

func TestClosedPoolSlingStoreOpenDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		previous := slingOpenCityStore
		defer func() { slingOpenCityStore = previous }()
		finished := make(chan struct{})
		slingOpenCityStore = func(string) (beads.Store, error) {
			defer close(finished)
			<-time.After(10 * time.Second)
			return nil, errors.New("late store error")
		}
		a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
		deps := slingDeps{Cfg: &config.City{Agents: []config.Agent{a}}, CityPath: "/fixture", StoreRef: "rig:project"}
		var stderr bytes.Buffer
		start := time.Now()
		printClosedPoolSessionNameWarnings(slingOpts{Target: a}, deps, &stderr)
		elapsed := time.Since(start)
		<-finished
		synctest.Wait()
		if elapsed > 3*time.Second || stderr.String() != "warning: pool-name check was skipped after 3s\n" {
			t.Fatalf("store open delayed sling by %s or leaked a late error: %q", elapsed, stderr.String())
		}
	})
}

func TestClosedPoolSlingErrorAndForce(t *testing.T) {
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	store := closedPoolSnapshotFailStore{Store: beads.NewMemStore(), err: errors.New("snapshot unavailable")}
	deps := slingDeps{Cfg: &config.City{Agents: []config.Agent{a}}, Store: store}
	var stderr bytes.Buffer
	printClosedPoolSessionNameWarnings(slingOpts{Target: a}, deps, &stderr)
	if strings.Count(stderr.String(), "\n") != 1 || !strings.Contains(stderr.String(), "snapshot unavailable") {
		t.Fatalf("snapshot error lost: %q", stderr.String())
	}
	// A forced sling must not even start a store read.
	synctest.Test(t, func(t *testing.T) {
		deps.Store = closedPoolBlockedStore{Store: beads.NewMemStore(), release: make(chan struct{})}
		stderr.Reset()
		start := time.Now()
		printClosedPoolSessionNameWarnings(slingOpts{Target: a, Force: true}, deps, &stderr)
		if time.Since(start) != 0 || stderr.Len() != 0 {
			t.Fatalf("forced sling ran the advisory: elapsed=%s output=%q", time.Since(start), stderr.String())
		}
	})
}
