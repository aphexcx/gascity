package agent

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/mailrelay/envelope"
)

const (
	keySuffix       = ".key"
	statementSuffix = ".statement"
	maxKeyFileBytes = 4096
	privateFileMode = 0o600
	privateDirMode  = 0o700
	publicFileMode  = 0o644
)

// KeyInfo names an encryption key keygen made.
type KeyInfo struct {
	KeyID     string
	PublicKey []byte
}

// keyRing is every private key the city can open mail with, and the newest
// key statement, the one peers are told to seal to.
type keyRing struct {
	private    []*ecdh.PrivateKey
	newest     envelope.KeyStatement
	newestWire []byte
}

// readSigningKey reads the city's Ed25519 signing key. It refuses a file the
// group or others can read or write, and never writes the file.
func readSigningKey(path string) (ed25519.PrivateKey, error) {
	data, err := readPrivateFile(path, "signing key")
	if err != nil {
		return nil, err
	}
	key, err := parseSigningKey(data)
	if err != nil {
		return nil, fmt.Errorf("signing key %s: %w", path, err)
	}
	return key, nil
}

// readPrivateFile reads a small regular file only its owner can reach.
func readPrivateFile(path, what string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", what, err)
	}
	defer f.Close() //nolint:errcheck // Read-only key file.
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", what, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s %s is not a regular file", what, path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%s %s is open to group or others (mode %#o); it must be 0600 or stricter", what, path, perm)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", what, err)
	}
	if len(data) > maxKeyFileBytes {
		return nil, fmt.Errorf("%s %s exceeds %d bytes", what, path, maxKeyFileBytes)
	}
	return data, nil
}

// parseSigningKey accepts the forms gc's grant minter reads: PEM PKCS#8, or a
// 32-byte seed or 64-byte private key as hex, base64 or raw bytes.
func parseSigningKey(data []byte) (ed25519.PrivateKey, error) {
	if block, _ := pem.Decode(data); block != nil {
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing PEM private key: %w", err)
		}
		priv, ok := key.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PEM key is %T, not Ed25519", key)
		}
		return priv, nil
	}
	trimmed := strings.TrimSpace(string(data))
	var candidates [][]byte
	for _, decode := range []func(string) ([]byte, error){hex.DecodeString, base64.StdEncoding.DecodeString, base64.RawURLEncoding.DecodeString} {
		if b, err := decode(trimmed); err == nil {
			candidates = append(candidates, b)
		}
	}
	for _, c := range append(candidates, data) {
		switch len(c) {
		case ed25519.SeedSize:
			return ed25519.NewKeyFromSeed(c), nil
		case ed25519.PrivateKeySize:
			key := ed25519.NewKeyFromSeed(c[:ed25519.SeedSize])
			if !key.Equal(ed25519.PrivateKey(c)) {
				return nil, errors.New("the public half does not belong to the private key")
			}
			return key, nil
		}
	}
	return nil, errors.New("not PEM, nor a 32-byte seed or 64-byte Ed25519 key as hex, base64 or raw bytes")
}

// Keygen makes a new encryption key and its signed statement in the
// encryption key folder and prints the public half. It never overwrites a
// key: with keys present it refuses unless rotate is set, and a rotated key
// gets a new key id and a later issue time. It contacts nobody.
func Keygen(s Settings, rotate bool, out io.Writer) (KeyInfo, error) {
	if err := s.checkKeygen(); err != nil {
		return KeyInfo{}, err
	}
	signer, err := readSigningKey(s.SigningKey)
	if err != nil {
		return KeyInfo{}, err
	}
	dir := s.EncryptionKeysDir
	if err := os.MkdirAll(dir, privateDirMode); err != nil {
		return KeyInfo{}, fmt.Errorf("making encryption key folder: %w", err)
	}
	ring, err := readKeyRing(dir, s.City, signer.Public().(ed25519.PublicKey))
	if err != nil {
		return KeyInfo{}, err
	}
	if len(ring.private) > 0 && !rotate {
		return KeyInfo{}, fmt.Errorf("an encryption key already exists in %s; use keygen --rotate to add a new one", dir)
	}
	issued := time.Now().Unix()
	if ring.newestWire != nil && issued <= ring.newest.Issued {
		issued = ring.newest.Issued + 1
	}
	key, err := envelope.GenerateKey()
	if err != nil {
		return KeyInfo{}, fmt.Errorf("making encryption key: %w", err)
	}
	nonce := make([]byte, 4)
	if _, err := rand.Read(nonce); err != nil {
		return KeyInfo{}, err
	}
	keyID := time.Unix(issued, 0).UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(nonce)
	stmt, err := envelope.SignStatement(s.City, keyID, key.PublicKey(), issued, signer)
	if err != nil {
		return KeyInfo{}, fmt.Errorf("signing key statement: %w", err)
	}
	wire, err := stmt.MarshalBinary()
	if err != nil {
		return KeyInfo{}, err
	}
	private, err := envelope.MarshalPrivateKey(key)
	if err != nil {
		return KeyInfo{}, err
	}
	if err := writeNewFile(dir, keyID+keySuffix, encodeLine(private), privateFileMode); err != nil {
		return KeyInfo{}, err
	}
	if err := writeNewFile(dir, keyID+statementSuffix, encodeLine(wire), publicFileMode); err != nil {
		return KeyInfo{}, err
	}
	public := key.PublicKey().Bytes()
	if _, err := fmt.Fprintf(out, "key-id %s\npublic-key %s\n", keyID, base64.StdEncoding.EncodeToString(public)); err != nil {
		return KeyInfo{}, err
	}
	return KeyInfo{KeyID: keyID, PublicKey: public}, nil
}

func encodeLine(b []byte) []byte { return []byte(base64.StdEncoding.EncodeToString(b) + "\n") }

// loadKeyRing reads the city's keys for a run; it needs at least one key and
// one statement.
func loadKeyRing(dir, city string, signer ed25519.PublicKey) (*keyRing, error) {
	ring, err := readKeyRing(dir, city, signer)
	if err != nil {
		return nil, err
	}
	switch {
	case len(ring.private) == 0:
		return nil, fmt.Errorf("no encryption key in %s; make one with gc-mail-relay keygen", dir)
	case ring.newestWire == nil:
		return nil, fmt.Errorf("no key statement in %s; make a new key with gc-mail-relay keygen --rotate", dir)
	}
	return ring, nil
}

// readKeyRing reads every key and statement in the folder, which must be
// closed to the group and others. Each statement must verify under the
// city's signing key and name a key the folder holds.
func readKeyRing(dir, city string, signer ed25519.PublicKey) (*keyRing, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("encryption key folder: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("encryption key folder %s is not a folder", dir)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("encryption key folder %s is open to group or others (mode %#o); it must be 0700", dir, perm)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("encryption key folder: %w", err)
	}
	ring := &keyRing{}
	held := make(map[string]bool)
	var statements []string
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasPrefix(name, "."):
		case strings.HasSuffix(name, keySuffix):
			key, err := readEncryptionKey(filepath.Join(dir, name))
			if err != nil {
				return nil, err
			}
			ring.private = append(ring.private, key)
			held[string(key.PublicKey().Bytes())] = true
		case strings.HasSuffix(name, statementSuffix):
			statements = append(statements, filepath.Join(dir, name))
		}
	}
	for _, path := range statements {
		stmt, wire, err := readOwnStatement(path, city, signer)
		if err != nil {
			return nil, err
		}
		if !held[string(stmt.PublicKey.Bytes())] {
			return nil, fmt.Errorf("key statement %s names a key the folder does not hold", path)
		}
		switch {
		case ring.newestWire == nil || stmt.Issued > ring.newest.Issued:
			ring.newest, ring.newestWire = stmt, wire
		case stmt.Issued == ring.newest.Issued:
			return nil, fmt.Errorf("key statements %s and %s were issued at the same second", path, ring.newest.KeyID)
		}
	}
	return ring, nil
}

func readEncryptionKey(path string) (*ecdh.PrivateKey, error) {
	data, err := readPrivateFile(path, "encryption key")
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("encryption key %s: %w", path, err)
	}
	key, err := envelope.ParsePrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("encryption key %s: %w", path, err)
	}
	return key, nil
}

func readOwnStatement(path, city string, signer ed25519.PublicKey) (envelope.KeyStatement, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return envelope.KeyStatement{}, nil, fmt.Errorf("reading key statement: %w", err)
	}
	wire, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return envelope.KeyStatement{}, nil, fmt.Errorf("key statement %s: %w", path, err)
	}
	stmt, err := envelope.ParseStatement(wire)
	if err != nil {
		return envelope.KeyStatement{}, nil, fmt.Errorf("key statement %s: %w", path, err)
	}
	if err := stmt.Verify(city, signer); err != nil {
		return envelope.KeyStatement{}, nil, fmt.Errorf("key statement %s does not verify as %s under the signing key: %w", path, city, err)
	}
	return stmt, wire, nil
}
