package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/mailrelay/envelope"
)

// A2: keygen writes a private key only it can read and a statement the
// city's signing key verifies; it never overwrites a key.
func TestKeygenWritesPrivateKeyAndSignedStatement(t *testing.T) {
	c := newCityFixture(t, "alpha")
	var out bytes.Buffer
	info, err := Keygen(c.settings, false, &out)
	if err != nil {
		t.Fatal(err)
	}
	dir := c.settings.EncryptionKeysDir
	assertMode(t, dir, 0o700)
	keyFile := filepath.Join(dir, info.KeyID+".key")
	assertMode(t, keyFile, 0o600)
	if !strings.Contains(out.String(), base64.StdEncoding.EncodeToString(info.PublicKey)) {
		t.Fatalf("keygen output %q does not print the public key", out.String())
	}
	if strings.Contains(out.String(), readTrimmed(t, keyFile)) {
		t.Fatal("keygen printed the private key")
	}
	stmt := readStatement(t, filepath.Join(dir, info.KeyID+".statement"))
	if err := stmt.Verify("alpha", c.public()); err != nil {
		t.Fatalf("statement does not verify under the signing key: %v", err)
	}
	if !bytes.Equal(stmt.PublicKey.Bytes(), info.PublicKey) || stmt.KeyID != info.KeyID {
		t.Fatal("statement does not name the new key")
	}
}

func TestKeygenRefusesSecondPlainRun(t *testing.T) {
	c := newCityFixture(t, "alpha")
	if _, err := Keygen(c.settings, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, c.settings.EncryptionKeysDir)
	_, err := Keygen(c.settings, false, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--rotate") {
		t.Fatalf("second plain keygen: %v", err)
	}
	if after := snapshot(t, c.settings.EncryptionKeysDir); !equalSnapshots(before, after) {
		t.Fatal("refused keygen changed the key folder")
	}
}

func TestKeygenRotateAddsNewKeyAndKeepsOld(t *testing.T) {
	c := newCityFixture(t, "alpha")
	first, err := Keygen(c.settings, false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, c.settings.EncryptionKeysDir)
	second, err := Keygen(c.settings, true, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if second.KeyID == first.KeyID || bytes.Equal(second.PublicKey, first.PublicKey) {
		t.Fatal("rotation did not make a new key with a new key id")
	}
	after := snapshot(t, c.settings.EncryptionKeysDir)
	for name, content := range before {
		if !bytes.Equal(after[name], content) {
			t.Fatalf("rotation changed %s", name)
		}
	}
	assertMode(t, filepath.Join(c.settings.EncryptionKeysDir, second.KeyID+".key"), 0o600)
	older := readStatement(t, filepath.Join(c.settings.EncryptionKeysDir, first.KeyID+".statement"))
	newer := readStatement(t, filepath.Join(c.settings.EncryptionKeysDir, second.KeyID+".statement"))
	if err := newer.Verify("alpha", c.public()); err != nil {
		t.Fatal(err)
	}
	if err := older.CheckSuccessor(newer); err != nil || newer.Issued <= older.Issued {
		t.Fatalf("rotated statement does not supersede the first: %v", err)
	}
	ring, err := loadKeyRing(c.settings.EncryptionKeysDir, "alpha", c.public())
	if err != nil {
		t.Fatal(err)
	}
	if len(ring.private) != 2 || ring.newest.KeyID != second.KeyID {
		t.Fatalf("ring holds %d keys, newest %q", len(ring.private), ring.newest.KeyID)
	}
}

func TestKeygenRefusesOpenKeyFolder(t *testing.T) {
	c := newCityFixture(t, "alpha")
	if err := os.Mkdir(c.settings.EncryptionKeysDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(c.settings.EncryptionKeysDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Keygen(c.settings, false, io.Discard); err == nil {
		t.Fatal("keygen wrote a private key into a folder others can read")
	}
	if names := snapshot(t, c.settings.EncryptionKeysDir); len(names) != 0 {
		t.Fatalf("refused keygen left %d files", len(names))
	}
}

// A3: a signing key file the group or others can read stops every verb
// before it does anything, and no verb ever changes the signing key file.
func TestSigningKeyOpenToOthersStopsEveryVerb(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o604} {
		t.Run(mode.String(), func(t *testing.T) {
			c := newCityFixture(t, "alpha")
			hub := newCityFixture(t, "hub")
			c.pin(hub)
			c.keygen()
			if err := os.Chmod(c.settings.SigningKey, mode); err != nil {
				t.Fatal(err)
			}
			digest := fileDigest(t, c.settings.SigningKey)
			keysBefore := snapshot(t, c.settings.EncryptionKeysDir)
			rel := newRecorder()
			deps := c.deps(rel.wrap(roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, refused() })), nil)

			_, err := Keygen(c.settings, true, io.Discard)
			assertSigningKeyRefusal(t, "keygen", err)
			_, err = Send(context.Background(), c.settings, deps, Outgoing{To: "hub/writer", From: "reader", Subject: "s", Body: "b"})
			assertSigningKeyRefusal(t, "send", err)
			err = Run(context.Background(), c.settings, deps)
			assertSigningKeyRefusal(t, "run", err)

			if got := rel.all(); len(got) != 0 {
				t.Fatalf("a refused verb contacted the relay %d times", len(got))
			}
			if !equalSnapshots(keysBefore, snapshot(t, c.settings.EncryptionKeysDir)) {
				t.Fatal("a refused verb changed the key folder")
			}
			if _, err := os.Stat(c.settings.StateDir); !os.IsNotExist(err) {
				t.Fatalf("a refused verb made the state folder: %v", err)
			}
			if fileDigest(t, c.settings.SigningKey) != digest {
				t.Fatal("the signing key file changed")
			}
		})
	}
}

// A pinned peer key that the group or others can change is not a pin: run
// and send refuse it before contacting anyone.
func TestPinsOthersCanWriteAreRefused(t *testing.T) {
	for name, chmod := range map[string]func(c *cityFixture) error{
		"pin file":     func(c *cityFixture) error { return os.Chmod(filepath.Join(c.settings.PeersDir, "hub.pub"), 0o620) },
		"peers folder": func(c *cityFixture) error { return os.Chmod(c.settings.PeersDir, 0o770) },
		"others, file": func(c *cityFixture) error { return os.Chmod(filepath.Join(c.settings.PeersDir, "hub.pub"), 0o602) },
	} {
		t.Run(name, func(t *testing.T) {
			hub, alpha := pair(t)
			_ = hub
			if err := chmod(alpha); err != nil {
				t.Fatal(err)
			}
			rel := newRecorder()
			deps := alpha.deps(rel.wrap(roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, refused() })), nil)
			_, sendErr := Send(context.Background(), alpha.settings, deps, Outgoing{To: "hub/writer", From: "reader", Subject: "s"})
			runErr := Run(context.Background(), alpha.settings, deps)
			for verb, err := range map[string]error{"send": sendErr, "run": runErr} {
				if err == nil || !strings.Contains(err.Error(), "group or others") {
					t.Fatalf("%s with a writable pin: %v", verb, err)
				}
			}
			if n := len(rel.all()); n != 0 {
				t.Fatalf("contacted the relay %d times", n)
			}
		})
	}
}

func assertSigningKeyRefusal(t *testing.T, verb string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s ran with an open signing key", verb)
	}
	msg := err.Error()
	if !strings.Contains(msg, "signing key") || strings.Contains(msg, "\n") {
		t.Fatalf("%s refusal is not one plain line naming the signing key: %q", verb, msg)
	}
}

func TestReadSigningKeyFormats(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		"pem":         pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}),
		"seed base64": []byte(base64.StdEncoding.EncodeToString(priv.Seed()) + "\n"),
		"seed hex":    []byte(hex.EncodeToString(priv.Seed())),
		"key base64":  []byte(base64.StdEncoding.EncodeToString(priv)),
		"seed raw":    priv.Seed(),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "key")
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := readSigningKey(path)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(priv) {
				t.Fatal("parsed a different key")
			}
		})
	}
}

func TestReadSigningKeyRefusesMismatchedHalves(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	bad := append(append([]byte(nil), priv.Seed()...), other...)
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(bad)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSigningKey(path); err == nil {
		t.Fatal("accepted a private key whose public half belongs to another key")
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s has mode %#o, want %#o", path, got, want)
	}
}

func readTrimmed(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func readStatement(t *testing.T, path string) envelope.KeyStatement {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(readTrimmed(t, path))
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := envelope.ParseStatement(raw)
	if err != nil {
		t.Fatal(err)
	}
	return stmt
}

// snapshot reads every file in dir; a missing dir is empty.
func snapshot(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return out
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = b
	}
	return out
}

func equalSnapshots(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !bytes.Equal(b[k], v) {
			return false
		}
	}
	return true
}
