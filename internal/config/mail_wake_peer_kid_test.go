package config

import "testing"

func TestMailWakePeerKid(t *testing.T) {
	for entry, want := range map[string]string{"city:alpha": "alpha", "city:a:b": "a:b", "city:": ""} {
		if kid, ok := MailWakePeerKid(entry); !ok || kid != want {
			t.Errorf("MailWakePeerKid(%q) = %q, %t", entry, kid, ok)
		}
	}
	for _, entry := range []string{"human", "writer", "rig/city:alpha", "City:alpha", ""} {
		if kid, ok := MailWakePeerKid(entry); ok {
			t.Errorf("MailWakePeerKid(%q) = %q, want no peer", entry, kid)
		}
	}
	if got := MailWakePeer("alpha"); got != "city:alpha" {
		t.Errorf("MailWakePeer = %q", got)
	}
}
