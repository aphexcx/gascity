package supervisor

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadTrustedKeys(t *testing.T) {
	dir := t.TempDir()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(pub)
	for name, content := range map[string]string{
		"good.pub": encoded + "\n", "commented.pub": "# city key\n" + encoded + "\n",
		"bad.pub": "not a key", "extra.pub": encoded + "\n" + encoded,
		"ignore.tmp": "incomplete staging file",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	keys, refused, err := LoadTrustedKeys(dir)
	if err != nil || len(keys) != 2 || len(refused) != 2 {
		t.Fatalf("loaded=%v refused=%v error=%v", keys, refused, err)
	}
	for i, name := range []string{"bad.pub", "extra.pub"} {
		if !strings.Contains(refused[i].Error(), name) {
			t.Errorf("refusal %v does not name %s", refused[i], name)
		}
	}
	if err := os.Chmod(filepath.Join(dir, "good.pub"), 0o666); err != nil {
		t.Fatal(err)
	}
	keys, refused, err = LoadTrustedKeys(dir)
	if err != nil || len(keys) != 1 || len(refused) != 3 || !strings.Contains(refused[2].Error(), "0666") {
		t.Fatalf("world-writable file: keys=%v refused=%v err=%v", keys, refused, err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadTrustedKeys(dir); err == nil || !strings.Contains(err.Error(), "0777") {
		t.Fatalf("world-writable directory: %v", err)
	}
	if _, _, err := LoadTrustedKeys(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("accepted absent directory")
	}
}

func TestTrustedKeysReloadInterval(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    time.Duration
		invalid bool
	}{
		{"", time.Minute, false},
		{"0", 0, false},
		{"0s", 0, false},
		{"2s", 2 * time.Second, false},
		{"-1s", 0, true},
		{"bad", 0, true},
	} {
		h := HardenedListener{TrustedKeysReloadInterval: tc.raw}
		got, err := h.TrustedKeysReloadDuration()
		if (err != nil) != tc.invalid || got != tc.want {
			t.Errorf("%q: got %v, %v", tc.raw, got, err)
		}
	}
}
