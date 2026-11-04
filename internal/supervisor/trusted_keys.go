package supervisor

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// LoadTrustedKeys reads a directory of <kid>.pub files. Directory failures are
// fatal; individual file failures are returned separately with valid keys so
// boot may report and skip them. Reload callers must reject any partial result.
func LoadTrustedKeys(dir string) (map[string]ed25519.PublicKey, []error, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("trusted_keys_dir %q: %w", dir, err)
	}
	defer root.Close() //nolint:errcheck // Read-only directory handle.
	info, err := root.Stat(".")
	if err != nil {
		return nil, nil, fmt.Errorf("trusted_keys_dir %q: %w", dir, err)
	}
	if info.Mode().Perm()&0o002 != 0 {
		return nil, nil, fmt.Errorf("trusted_keys_dir %q: world-writable mode %#04o refused", dir, info.Mode().Perm())
	}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, nil, fmt.Errorf("trusted_keys_dir %q: %w", dir, err)
	}
	keys := make(map[string]ed25519.PublicKey)
	var refused []error
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".pub") {
			continue
		}
		kid := strings.TrimSuffix(name, ".pub")
		key, err := readTrustedKey(root, name, kid)
		if err != nil {
			refused = append(refused, fmt.Errorf("trusted key %q in %q: %w", name, dir, err))
			continue
		}
		keys[kid] = key
	}
	return keys, refused, nil
}

func readTrustedKey(root *os.Root, name, kid string) (ed25519.PublicKey, error) {
	if kid == "" || strings.TrimSpace(kid) != kid || strings.ContainsAny(kid, ":,\r\n\x00") {
		return nil, errors.New("invalid key id")
	}
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("non-regular file mode %s refused", info.Mode())
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // Read-only key file.
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, errors.New("file changed while opening")
	}
	if opened.Mode().Perm()&0o002 != 0 {
		return nil, fmt.Errorf("world-writable mode %#04o refused", opened.Mode().Perm())
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return nil, err
	}
	if len(data) > 4096 {
		return nil, errors.New("key file exceeds 4096 bytes")
	}
	var encoded string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if encoded != "" || line == "" || strings.ContainsAny(line, "\r\t ") {
			return nil, errors.New("want one base64 public-key line with optional # comments")
		}
		encoded = line
	}
	key, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("want standard base64 of a 32-byte Ed25519 public key")
	}
	return ed25519.PublicKey(key), nil
}
