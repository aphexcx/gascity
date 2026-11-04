package citywriteauth

import (
	"crypto/ed25519"
	"errors"
	"testing"
	"time"
)

// Rotation must preserve replay state, own its keys, and support full revocation.
func TestWithKeysPreservesReplayAndRevokes(t *testing.T) {
	v, priv, grant, expect := fixture(t, time.Now())
	token := mintFor(t, priv, grant)
	if _, err := v.Verify(token, expect); err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PublicKey{"k1": priv.Public().(ed25519.PublicKey)}
	next, err := v.WithKeys(keys)
	if err != nil {
		t.Fatal(err)
	}
	keys["k1"][0] ^= 1
	if _, err := next.Verify(token, expect); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay after rotation = %v", err)
	}
	grant.JTI = "fresh"
	if _, err := next.Verify(mintFor(t, priv, grant), expect); err != nil {
		t.Fatal(err)
	}
	if _, err := next.WithKeys(map[string]ed25519.PublicKey{"bad": {1}}); err == nil {
		t.Fatal("accepted invalid replacement")
	}
	empty, err := next.WithKeys(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := empty.Verify(token, expect); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("revoked key = %v", err)
	}
}
