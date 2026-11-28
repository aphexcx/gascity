package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/citywriteauth"
	"github.com/gastownhall/gascity/internal/supervisor"
)

type hardenedAuthSnapshot struct {
	write, read *citywriteauth.Verifier
	handler     http.Handler
}

// HardenedAuth serves an independently gated listener. Reload publishes a whole
// handler/verifier pair atomically, retaining replay guards across generations.
type HardenedAuth struct {
	current                 atomic.Pointer[hardenedAuthSnapshot]
	mu                      sync.Mutex
	mux                     *SupervisorMux
	config                  supervisor.Section
	inlineWrite, inlineRead map[string]ed25519.PublicKey
	interval                time.Duration
	logf                    func(string, ...any)
}

// NewHardenedAuth resolves all keys before any listener binds. Invalid individual
// directory files are reported and skipped at boot; directory and key-collision
// errors refuse startup. The primary handler and its verifiers are unchanged.
//
// Directory keys verify write grants only. Reads are verified by the inline
// read keys alone; a listener with a directory and no inline read key refuses
// every per-city read and never takes the primary's read posture.
func NewHardenedAuth(sm *SupervisorMux, cfg supervisor.Section, logf func(string, ...any)) (*HardenedAuth, error) {
	if logf == nil {
		logf = log.Printf
	}
	h := &HardenedAuth{mux: sm, config: cfg, logf: logf}
	var err error
	if h.interval, err = cfg.Hardened.TrustedKeysReloadDuration(); err != nil {
		return nil, err
	}
	if raw := strings.TrimSpace(cfg.Hardened.WriteAuthVerifyKey); raw != "" {
		if h.inlineWrite, err = parseVerifyKeys(raw); err != nil {
			return nil, fmt.Errorf("hardened write-auth: %w", err)
		}
	}
	if raw := strings.TrimSpace(cfg.Hardened.ReadAuthVerifyKey); raw != "" {
		if h.inlineRead, err = parseVerifyKeys(raw); err != nil {
			return nil, fmt.Errorf("hardened read-auth: %w", err)
		}
	}
	writes, err := h.loadKeys(true)
	if err != nil {
		return nil, err
	}
	write, err := ResolveHardenedWriteAuthVerifier(encodeVerifyKeys(writes))
	if err != nil {
		return nil, err
	}
	read, restrictReads, err := h.resolveReadAuth()
	if err != nil {
		return nil, err
	}
	h.current.Store(&hardenedAuthSnapshot{write: write, read: read, handler: sm.hardenedHandlerWithAuth(write, read, restrictReads)})
	return h, nil
}

// resolveReadAuth returns the read gate fixed at startup. Only inline read keys
// verify reads. With a directory and no inline read key the gate holds no key,
// so every per-city read is refused. Only a listener with neither keeps the
// primary's read posture, and read_auth_required refuses that one.
func (h *HardenedAuth) resolveReadAuth() (*citywriteauth.Verifier, bool, error) {
	switch {
	case len(h.inlineRead) != 0:
		read, err := newReadAuthVerifier(encodeVerifyKeys(h.inlineRead))
		if err != nil {
			return nil, false, fmt.Errorf("hardened read-auth: %w", err)
		}
		return read, true, nil
	case h.config.Hardened.TrustedKeysDir != "":
		read, err := newKeylessReadAuthVerifier()
		if err != nil {
			return nil, false, fmt.Errorf("hardened read-auth: %w", err)
		}
		return read, true, nil
	case h.config.Hardened.ReadAuthRequired:
		return nil, false, errors.New("hardened read-auth required but neither read_auth_verify_key nor trusted_keys_dir is configured")
	default:
		return h.mux.readAuth, false, nil
	}
}

// newKeylessReadAuthVerifier returns a read verifier that holds no key, so the
// read gate stays installed and refuses every grant. citywriteauth.New needs a
// key, so it is built with a fresh key whose private half is never kept, then
// emptied.
func newKeylessReadAuthVerifier() (*citywriteauth.Verifier, error) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	read, err := newReadAuthVerifier(encodeVerifyKeys(map[string]ed25519.PublicKey{"keyless": pub}))
	if err != nil {
		return nil, err
	}
	return read.WithKeys(nil)
}

// ServeHTTP dispatches through one complete immutable snapshot per request.
func (h *HardenedAuth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.current.Load().handler.ServeHTTP(w, r)
}

// Reload refreshes only the trusted-key directory, which feeds the write
// verifier alone. Any failure retains the full previous set; a successful empty
// set revokes all directory keys, keeping both gates installed. The read gate,
// inline keys and primary configuration are fixed at startup.
func (h *HardenedAuth) Reload() error {
	if h.config.Hardened.TrustedKeysDir == "" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	writes, err := h.loadKeys(false)
	if err != nil {
		return err
	}
	previous := h.current.Load()
	write, err := previous.write.WithKeys(writes)
	if err != nil {
		return err
	}
	h.current.Store(&hardenedAuthSnapshot{write: write, read: previous.read, handler: h.mux.hardenedHandlerWithAuth(write, previous.read, true)})
	return nil
}

// Run refreshes the directory on its configured interval until ctx is canceled.
// A zero interval or absent directory disables this timer, not explicit Reload.
func (h *HardenedAuth) Run(ctx context.Context) {
	if h.config.Hardened.TrustedKeysDir == "" || h.interval == 0 {
		return
	}
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := h.Reload(); err != nil {
				h.logf("hardened trusted-key reload: retaining previous keys: %s", strings.ReplaceAll(err.Error(), "\n", "; "))
			}
		}
	}
}

// loadKeys reads the directory and returns the write keys: the inline write
// keys merged with the directory's. Directory keys never verify reads, but a
// directory kid that is also an inline or primary read kid is still refused, as
// it was when the directory fed the read verifier too.
func (h *HardenedAuth) loadKeys(boot bool) (map[string]ed25519.PublicKey, error) {
	var directory map[string]ed25519.PublicKey
	if dir := h.config.Hardened.TrustedKeysDir; dir != "" {
		keys, refused, err := supervisor.LoadTrustedKeys(dir)
		if err != nil {
			return nil, err
		}
		if !boot && len(refused) != 0 {
			return nil, errors.Join(refused...)
		}
		for _, err := range refused {
			h.logf("hardened trusted key refused: %v", err)
		}
		directory = keys
	}
	writes, err := mergeTrustedKeys(h.inlineWrite, directory, "write_auth_verify_key")
	if err != nil {
		return nil, err
	}
	readKids, err := mergeTrustedKeys(h.inlineRead, directory, "read_auth_verify_key")
	if err != nil {
		return nil, err
	}
	if len(writes) != 0 {
		if err := HardenedVerifierKidsDisjoint(h.config.WriteAuthVerifyKey, encodeVerifyKeys(writes)); err != nil {
			return nil, err
		}
	}
	if len(readKids) != 0 {
		if err := hardenedReadVerifierKidsDisjoint(h.config.ReadAuthVerifyKey, encodeVerifyKeys(readKids)); err != nil {
			return nil, err
		}
	}
	return writes, nil
}

func mergeTrustedKeys(inline, directory map[string]ed25519.PublicKey, field string) (map[string]ed25519.PublicKey, error) {
	merged := make(map[string]ed25519.PublicKey, len(inline)+len(directory))
	for kid, key := range inline {
		merged[kid] = key
	}
	for kid, key := range directory {
		if _, exists := merged[kid]; exists {
			return nil, fmt.Errorf("hardened %s: kid %q appears in both inline keys and trusted_keys_dir", field, kid)
		}
		merged[kid] = key
	}
	return merged, nil
}

func encodeVerifyKeys(keys map[string]ed25519.PublicKey) string {
	parts := make([]string, 0, len(keys))
	for kid, key := range keys {
		parts = append(parts, kid+":"+base64.StdEncoding.EncodeToString(key))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}
