package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServeRequiresHub(t *testing.T) {
	t.Setenv("GC_MAIL_RELAY_HUB", "")
	cmd := newCommand()
	cmd.SetArgs([]string{"serve"})
	cmd.SetOut(io.Discard)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "hub") {
		t.Fatalf("serve without hub: %v", err)
	}
}

// keysDir returns a keys directory that pins the named cities.
func keysDir(t *testing.T, cities ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, city := range cities {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, city+".pub"), []byte(base64.StdEncoding.EncodeToString(pub)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// serve runs the serve verb with args and returns its error. The listen
// address has no port, so a run that passes every check fails at the
// listener instead of serving.
func serve(t *testing.T, args ...string) error {
	t.Helper()
	cmd := newCommand()
	cmd.SetArgs(append([]string{"serve", "--hub", "hub", "--listen", "no-port", "--store", filepath.Join(t.TempDir(), "relay.db")}, args...))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return cmd.Execute()
}

func TestServeRequiresPinnedHub(t *testing.T) {
	for _, address := range []string{"", "https://registry.test/v1/cities"} {
		err := serve(t, "--keys-dir", keysDir(t, "alpha"), "--registry-url", address)
		if err == nil || !strings.Contains(err.Error(), "pinned") || strings.Contains(err.Error(), "\n") {
			t.Fatalf("serve without a pinned hub, registry %q: want one plain line, got %v", address, err)
		}
	}
}

func TestServeRefusesPlainHTTPRegistry(t *testing.T) {
	err := serve(t, "--keys-dir", keysDir(t, "hub"), "--registry-url", "http://registry.test/v1/cities")
	if err == nil || !strings.Contains(err.Error(), "registry URL") {
		t.Fatalf("serve with an http registry on a remote host: %v", err)
	}
}

func TestServeReadsRegistrySettingsFromEnvironment(t *testing.T) {
	t.Setenv("GC_MAIL_RELAY_REGISTRY_URL", "http://registry.test/v1/cities")
	t.Setenv("GC_MAIL_RELAY_REGISTRY_ALLOW_EMPTY", "false")
	if err := serve(t, "--keys-dir", keysDir(t, "hub")); err == nil || !strings.Contains(err.Error(), "registry URL") {
		t.Fatalf("GC_MAIL_RELAY_REGISTRY_URL was not applied: %v", err)
	}
	t.Setenv("GC_MAIL_RELAY_REGISTRY_URL", "")
	t.Setenv("GC_MAIL_RELAY_REGISTRY_ALLOW_EMPTY", "sometimes")
	if err := serve(t, "--keys-dir", keysDir(t, "hub")); err == nil || !strings.Contains(err.Error(), "GC_MAIL_RELAY_REGISTRY_ALLOW_EMPTY") {
		t.Fatalf("GC_MAIL_RELAY_REGISTRY_ALLOW_EMPTY was not read: %v", err)
	}
}
