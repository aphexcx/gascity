package relay

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"testing"
)

func TestCheckSigningKeyTakesRealKeys(t *testing.T) {
	basePoint := append([]byte{0x58}, bytes.Repeat([]byte{0x66}, 31)...)
	if err := checkSigningKey(basePoint); err != nil {
		t.Fatalf("the base point: %v", err)
	}
	for range 1000 {
		pub, _, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkSigningKey(pub); err != nil {
			t.Fatalf("generated key %x: %v", []byte(pub), err)
		}
	}
}

func TestCheckSigningKeyRefuses(t *testing.T) {
	for _, tc := range notSigningKeys {
		t.Run(tc.name, func(t *testing.T) {
			key, err := base64.StdEncoding.DecodeString(notSigningKey(t, tc.hex))
			if err != nil {
				t.Fatal(err)
			}
			if err := checkSigningKey(key); err == nil || err.Error() != tc.reason {
				t.Fatalf("got %v, want %q", err, tc.reason)
			}
		})
	}
	for _, size := range []int{0, 31, 33, 64} {
		if err := checkSigningKey(make([]byte, size)); err == nil {
			t.Fatalf("a key of %d bytes was taken", size)
		}
	}
}
