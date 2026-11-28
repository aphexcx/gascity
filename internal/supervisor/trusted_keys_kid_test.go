package supervisor

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// A wake list may name exactly the key ids the key loader registers: the
// loader and the list share one rule.
func TestTrustedKeyIDRuleIsTheWakeListRule(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte(base64.StdEncoding.EncodeToString(pub) + "\n")
	for kid, want := range map[string]bool{
		"alpha": true, "a.b-c_1": true,
		"": false, "a:b": false, "a,b": false, " alpha": false, "alpha ": false, "a\rb": false, "a\nb": false,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, kid+".pub"), content, 0o600); err != nil {
			t.Fatal(err)
		}
		keys, _, err := LoadTrustedKeys(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, loaded := keys[kid]; loaded != want {
			t.Errorf("key id %q loaded=%t, want %t", kid, loaded, want)
		}
		if got := config.ValidVerifyKeyID(kid); got != want {
			t.Errorf("ValidVerifyKeyID(%q)=%t, want %t", kid, got, want)
		}
		listed := config.ValidateAgents([]config.Agent{{Name: "reader", MailWakeFrom: []string{config.MailWakePeer(kid)}}}) == nil
		if listed != want {
			t.Errorf("wake list entry for key id %q accepted=%t, want %t", kid, listed, want)
		}
	}
}
