package api

import (
	"context"
	"crypto/ed25519"
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
	writes, reads, err := h.loadKeys(true)
	if err != nil {
		return nil, err
	}
	write, err := ResolveHardenedWriteAuthVerifier(encodeVerifyKeys(writes))
	if err != nil {
		return nil, err
	}
	read := sm.readAuth
	if len(reads) != 0 {
		read, err = newReadAuthVerifier(encodeVerifyKeys(reads))
		if err != nil {
			return nil, fmt.Errorf("hardened read-auth: %w", err)
		}
	} else if cfg.Hardened.ReadAuthRequired || cfg.Hardened.TrustedKeysDir != "" {
		return nil, errors.New("hardened read-auth required but no read_auth_verify_key or trusted directory key configured")
	}
	restrictReads := len(reads) != 0
	h.current.Store(&hardenedAuthSnapshot{write: write, read: read, handler: sm.hardenedHandlerWithAuth(write, read, restrictReads)})
	return h, nil
}

// ServeHTTP dispatches through one complete immutable snapshot per request.
func (h *HardenedAuth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.current.Load().handler.ServeHTTP(w, r)
}

// Reload refreshes only the trusted-key directory. Any failure retains the full
// previous set; a successful empty set revokes all directory keys, keeping both
// gates installed. Inline keys and primary configuration are fixed at startup.
func (h *HardenedAuth) Reload() error {
	if h.config.Hardened.TrustedKeysDir == "" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	writes, reads, err := h.loadKeys(false)
	if err != nil {
		return err
	}
	previous := h.current.Load()
	write, err := previous.write.WithKeys(writes)
	if err != nil {
		return err
	}
	read, err := previous.read.WithKeys(reads)
	if err != nil {
		return err
	}
	h.current.Store(&hardenedAuthSnapshot{write: write, read: read, handler: h.mux.hardenedHandlerWithAuth(write, read, true)})
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

func (h *HardenedAuth) loadKeys(boot bool) (map[string]ed25519.PublicKey, map[string]ed25519.PublicKey, error) {
	var directory map[string]ed25519.PublicKey
	if dir := h.config.Hardened.TrustedKeysDir; dir != "" {
		keys, refused, err := supervisor.LoadTrustedKeys(dir)
		if err != nil {
			return nil, nil, err
		}
		if !boot && len(refused) != 0 {
			return nil, nil, errors.Join(refused...)
		}
		for _, err := range refused {
			h.logf("hardened trusted key refused: %v", err)
		}
		directory = keys
	}
	writes, err := mergeTrustedKeys(h.inlineWrite, directory, "write_auth_verify_key")
	if err != nil {
		return nil, nil, err
	}
	reads, err := mergeTrustedKeys(h.inlineRead, directory, "read_auth_verify_key")
	if err != nil {
		return nil, nil, err
	}
	if len(writes) != 0 {
		if err := HardenedVerifierKidsDisjoint(h.config.WriteAuthVerifyKey, encodeVerifyKeys(writes)); err != nil {
			return nil, nil, err
		}
	}
	if len(reads) != 0 {
		if err := hardenedReadVerifierKidsDisjoint(h.config.ReadAuthVerifyKey, encodeVerifyKeys(reads)); err != nil {
			return nil, nil, err
		}
	}
	return writes, reads, nil
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
