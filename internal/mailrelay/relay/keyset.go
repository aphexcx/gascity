package relay

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"maps"

	"github.com/gastownhall/gascity/internal/mailrelay/envelope"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// cityKeys maps a city ID to its Ed25519 signing public key.
type cityKeys = map[string]ed25519.PublicKey

// A keySource reads one whole set of city signing keys for sign-in. A set it
// returns is new and owned by the caller.
type keySource interface {
	read(ctx context.Context) (cityKeys, error)
}

// keysDirectory is the pinned key source: one <city>.pub file per city. One
// invalid file fails the whole read.
type keysDirectory string

func (dir keysDirectory) read(context.Context) (cityKeys, error) {
	keys, refused, err := supervisor.LoadTrustedKeys(string(dir))
	if err != nil {
		return nil, fmt.Errorf("read trusted keys: %w", err)
	}
	if len(refused) > 0 {
		return nil, fmt.Errorf("trust directory contains %d invalid key files", len(refused))
	}
	return keys, nil
}

// publish swaps in the key set built from the pinned and registry keys, as one
// whole, for every request that follows. A pinned key wins over a registry key
// for the same city. Every verifier shares the store's record of spent grants,
// so a grant used before a swap stays spent after it. The caller holds keyMu.
func (s *Server) publish(pinned, registry cityKeys) error {
	keys := make(cityKeys, len(pinned)+len(registry))
	maps.Copy(keys, registry)
	maps.Copy(keys, pinned)
	set := &trust{keys: keys}
	if len(keys) > 0 {
		v, err := envelope.NewGrantVerifier(keys, s.store)
		if err != nil {
			return fmt.Errorf("build grant verifier: %w", err)
		}
		set.verifier = v
	}
	s.trusted.Store(set)
	s.pinned, s.fromRegistry = pinned, registry
	return nil
}

// unpinned returns the listed keys of cities that are neither pinned nor the
// hub, and how many it left out. The hub's key never comes from the registry.
func (s *Server) unpinned(listed, pinned cityKeys) (cityKeys, int) {
	keys := make(cityKeys, len(listed))
	for city, key := range listed {
		if city != s.config.Hub && pinned[city] == nil {
			keys[city] = key
		}
	}
	return keys, len(listed) - len(keys)
}

// copyRegistry reads one copy of the registry's list. A good copy replaces the
// registry keys as one whole; a bad copy, or an empty list unless empty lists
// are allowed, changes nothing. Either way it logs one line, never a key.
// Registry keys are kept without the cities they lost to a pin, so removing a
// pin later uncovers nothing until a newer good copy lists that city.
func (s *Server) copyRegistry(ctx context.Context) {
	if s.registry == nil {
		return
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	listed, err := s.registry.read(ctx)
	switch {
	case err != nil && ctx.Err() != nil:
		return // shutting down; not a bad copy
	case err != nil:
		s.logBadCopy(err)
		return
	case len(listed) == 0 && !s.config.RegistryAllowEmpty:
		s.logBadCopy(&copyError{reason: "empty list"})
		return
	}
	next, leftOut := s.unpinned(listed, s.pinned)
	added, removed, changed := countChanges(s.fromRegistry, next)
	if err := s.publish(s.pinned, next); err != nil {
		s.logBadCopy(&copyError{reason: "verifier refused the key set", entries: len(listed)})
		return
	}
	s.logger.Printf("registry copy good entries=%d pinned=%d added=%d removed=%d changed=%d", len(listed), leftOut, added, removed, changed)
}

func (s *Server) logBadCopy(err error) {
	reason, entries := "request failed", 0
	var bad *copyError
	if errors.As(err, &bad) {
		reason, entries = bad.reason, bad.entries
	}
	s.logger.Printf("registry copy bad entries=%d kept=%d reason=%q", entries, len(s.fromRegistry), reason)
}

// countChanges compares the registry keys in force before and after a copy.
func countChanges(before, after cityKeys) (added, removed, changed int) {
	for city, key := range after {
		old, ok := before[city]
		switch {
		case !ok:
			added++
		case !bytes.Equal(old, key):
			changed++
		}
	}
	for city := range before {
		if _, ok := after[city]; !ok {
			removed++
		}
	}
	return added, removed, changed
}
