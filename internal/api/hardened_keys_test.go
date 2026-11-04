package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/citywriteauth"
	"github.com/gastownhall/gascity/internal/supervisor"
)

func writeTrustedKey(t *testing.T, dir, kid string, pub ed25519.PublicKey) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, kid+".pub"), []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func hardenedRead(t *testing.T, h http.Handler, priv ed25519.PrivateKey, kid, jti string) int {
	t.Helper()
	const path = "/v0/city/acme/beads"
	r := httptest.NewRequest("GET", path, nil)
	if priv != nil {
		g := readGrant(time.Now(), "acme", "GET", path, "", jti)
		g.Kid = kid
		r.Header.Set(readAuthHeader, mintToken(t, priv, g))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

func TestHardenedKeysReload(t *testing.T) {
	dir := t.TempDir()
	pub, priv := mustKeypair(t)
	writeTrustedKey(t, dir, "one", pub)
	sm := newTestSupervisorMux(t, map[string]*fakeState{"acme": newFakeState(t)}).WithAnyHostAllowed()
	cfg := supervisor.Section{Hardened: supervisor.HardenedListener{Port: 8447, TrustedKeysDir: dir}}
	h, err := NewHardenedAuth(sm, cfg, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if got := hardenedRead(t, h, nil, "", ""); got != 401 {
		t.Fatalf("unsigned hardened read: %d", got)
	}
	if got := hardenedRead(t, sm.Handler(), nil, "", ""); got != 200 {
		t.Fatalf("primary changed: %d", got)
	}
	if got := hardenedRead(t, h, priv, "one", "consumed"); got != 200 {
		t.Fatalf("signed read: %d", got)
	}
	writeTrustedKey(t, dir, "two", pub)
	if got := hardenedRead(t, h, priv, "two", "new"); got != 403 {
		t.Fatalf("unloaded key accepted: %d", got)
	}
	if err := h.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := hardenedRead(t, h, priv, "two", "new"); got != 200 {
		t.Fatalf("added key refused: %d", got)
	}
	if got := hardenedRead(t, h, priv, "one", "consumed"); got != 403 {
		t.Fatalf("replay after reload: %d", got)
	}
	// Both audiences use the directory, with independent replay domains.
	pair := h.current.Load()
	read := readGrant(time.Now(), "acme", "GET", "/v0/city/acme/beads", "", "same-jti")
	read.Kid = "one"
	write := read
	write.Aud = writeAuthAudience
	expect := citywriteauth.Expect{City: read.City, ReqDigest: read.Req}
	if _, err := pair.read.Verify(mintToken(t, priv, read), expect); err != nil {
		t.Fatal(err)
	}
	if _, err := pair.write.Verify(mintToken(t, priv, write), expect); err != nil {
		t.Fatal(err)
	}
	if _, err := pair.read.Verify(mintToken(t, priv, write), expect); err == nil {
		t.Fatal("write audience admitted by read verifier")
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.pub"), []byte("malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.Reload(); err == nil || !strings.Contains(err.Error(), "bad.pub") {
		t.Fatalf("bad reload: %v", err)
	}
	if h.current.Load() != pair {
		t.Fatal("bad reload replaced the active pair")
	}
	if err := os.Remove(filepath.Join(dir, "bad.pub")); err != nil {
		t.Fatal(err)
	}
	for _, kid := range []string{"one", "two"} {
		if err := os.Remove(filepath.Join(dir, kid+".pub")); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := hardenedRead(t, h, priv, "one", "revoked"); got != 403 {
		t.Fatalf("removed last key still accepted: %d", got)
	}
	if got := hardenedRead(t, h, nil, "", ""); got != 401 {
		t.Fatalf("empty directory disabled gate: %d", got)
	}
	writeTrustedKey(t, dir, "one", pub)
	if err := h.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := hardenedRead(t, h, priv, "one", "consumed"); got != 403 {
		t.Fatalf("replay after revoke and restore: %d", got)
	}
	// A failed directory read also preserves the active snapshot.
	pair = h.current.Load()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := h.Reload(); err == nil || !strings.Contains(err.Error(), "0777") {
		t.Fatalf("unsafe reload = %v", err)
	}
	if h.current.Load() != pair {
		t.Fatal("unsafe directory replaced the active pair")
	}
}

func TestHardenedKeysBootSkipsMalformedFile(t *testing.T) {
	dir := t.TempDir()
	pub, priv := mustKeypair(t)
	writeTrustedKey(t, dir, "one", pub)
	if err := os.WriteFile(filepath.Join(dir, "bad.pub"), []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	sm := newTestSupervisorMux(t, map[string]*fakeState{"acme": newFakeState(t)}).WithAnyHostAllowed()
	var messages []string
	h, err := NewHardenedAuth(sm, supervisor.Section{Hardened: supervisor.HardenedListener{Port: 8447, TrustedKeysDir: dir}}, func(format string, args ...any) {
		messages = append(messages, fmt.Sprintf(format, args...))
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || !strings.Contains(messages[0], "bad.pub") {
		t.Fatalf("boot diagnostics = %v", messages)
	}
	if got := hardenedRead(t, h, priv, "one", "boot"); got != 200 {
		t.Fatalf("valid file refused: %d", got)
	}
}

func TestHardenedKeysRejectCollisions(t *testing.T) {
	pub, _ := mustKeypair(t)
	key := "one:" + base64.StdEncoding.EncodeToString(pub)
	for _, field := range []string{"inline-write", "inline-read", "primary-write", "primary-read"} {
		t.Run(field, func(t *testing.T) {
			dir := t.TempDir()
			writeTrustedKey(t, dir, "one", pub)
			cfg := supervisor.Section{Hardened: supervisor.HardenedListener{Port: 8447, TrustedKeysDir: dir}}
			switch field {
			case "inline-write":
				cfg.Hardened.WriteAuthVerifyKey = key
			case "inline-read":
				cfg.Hardened.ReadAuthVerifyKey = key
			case "primary-write":
				cfg.WriteAuthVerifyKey = key
			case "primary-read":
				cfg.ReadAuthVerifyKey = key
			}
			sm := newTestSupervisorMux(t, nil)
			if _, err := NewHardenedAuth(sm, cfg, t.Logf); err == nil || !strings.Contains(err.Error(), "one") {
				t.Fatalf("collision: %v", err)
			}
		})
	}
}

func TestHardenedKeysTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		pub, priv := mustKeypair(t)
		writeTrustedKey(t, dir, "one", pub)
		sm := newTestSupervisorMux(t, map[string]*fakeState{"acme": newFakeState(t)}).WithAnyHostAllowed()
		cfg := supervisor.Section{Hardened: supervisor.HardenedListener{Port: 8447, TrustedKeysDir: dir, TrustedKeysReloadInterval: "1s"}}
		h, err := NewHardenedAuth(sm, cfg, t.Logf)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go h.Run(ctx)
		synctest.Wait()
		writeTrustedKey(t, dir, "two", pub)
		<-time.After(time.Second) // Advance virtual time to the first refresh deadline.
		synctest.Wait()
		if got := hardenedRead(t, h, priv, "two", "timer"); got != 200 {
			t.Fatalf("timer did not register new key: %d", got)
		}
	})
}

func TestHardenedKeysTimerDisabledAndInlineMerge(t *testing.T) {
	dir := t.TempDir()
	pub, priv := mustKeypair(t)
	writeTrustedKey(t, dir, "one", pub)
	inline := "inline:" + base64.StdEncoding.EncodeToString(pub)
	local := "local:" + base64.StdEncoding.EncodeToString(pub)
	sm := newTestSupervisorMux(t, map[string]*fakeState{"acme": newFakeState(t)}).WithAnyHostAllowed()
	h, err := NewHardenedAuth(sm, supervisor.Section{ReadAuthVerifyKey: local, Hardened: supervisor.HardenedListener{
		Port: 8447, TrustedKeysDir: dir, TrustedKeysReloadInterval: "0", WriteAuthVerifyKey: inline, ReadAuthVerifyKey: inline,
	}}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	writeTrustedKey(t, dir, "two", pub)
	h.Run(context.Background()) // Disabled timer returns immediately.
	if got := hardenedRead(t, h, priv, "two", "manual"); got != 403 {
		t.Fatalf("timer ran: %d", got)
	}
	if err := h.Reload(); err != nil {
		t.Fatal(err)
	}
	for _, kid := range []string{"two", "inline"} {
		if got := hardenedRead(t, h, priv, kid, kid); got != 200 {
			t.Fatalf("merged %s refused: %d", kid, got)
		}
	}
	before := h.current.Load()
	writeTrustedKey(t, dir, "local", pub)
	if err := h.Reload(); err == nil || !strings.Contains(err.Error(), "local") {
		t.Fatalf("collision reload: %v", err)
	}
	if h.current.Load() != before {
		t.Fatal("collision replaced keys")
	}
}

func TestHardenedKeysConcurrentReload(t *testing.T) {
	dir := t.TempDir()
	pub, priv := mustKeypair(t)
	writeTrustedKey(t, dir, "one", pub)
	sm := newTestSupervisorMux(t, nil).WithAnyHostAllowed()
	h, err := NewHardenedAuth(sm, supervisor.Section{Hardened: supervisor.HardenedListener{Port: 8447, TrustedKeysDir: dir}}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for n := range 4 {
		wg.Go(func() {
			for i := range 20 {
				if got := hardenedRead(t, h, priv, "one", fmt.Sprintf("%d-%d", n, i)); got != http.StatusNotFound {
					t.Errorf("signed unknown-city read = %d", got)
				}
			}
		})
	}
	for range 20 {
		if err := h.Reload(); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}
