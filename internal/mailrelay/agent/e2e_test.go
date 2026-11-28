package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A1 and A14: hub mails alpha through a real relay, alpha files and tells,
// alpha answers, hub files the answer. The relay never holds the subject or
// body, and no agent log carries a subject, body, grant or private key.
func TestEndToEndBothWays(t *testing.T) {
	hub, alpha := newCityFixture(t, "hub"), newCityFixture(t, "alpha")
	hub.pin(alpha)
	alpha.pin(hub)
	hub.keygen()
	alpha.keygen()
	rel := newRelayFixture(t, map[string]ed25519.PublicKey{"hub": hub.public(), "alpha": alpha.public()})
	hubAPI, alphaAPI := newCityAPI(t, "hub", "writer"), newCityAPI(t, "alpha", "reader")

	start(t, hub.settings, hub.deps(rel.transport, hubAPI.transport()))
	start(t, alpha.settings, alpha.deps(rel.transport, alphaAPI.transport()))
	rel.recorder.await(t, "both agents to publish and ask", func() bool {
		return len(rel.recorder.matching("PUT", "/key")) == 2 && askedBy(rel.recorder, "hub") && askedBy(rel.recorder, "alpha")
	})

	sendDeps := hub.deps(rel.transport, nil)
	id, err := Send(context.Background(), hub.settings, sendDeps, Outgoing{To: "alpha/reader", From: "writer", Subject: subjectSentinel, Body: bodySentinel})
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("send printed no id")
	}
	alphaAPI.recorder.await(t, "alpha to file the mail", func() bool { return len(alphaAPI.inbox("reader")) == 1 })
	got := alphaAPI.inbox("reader")[0]
	if got.From != "hub/writer" || got.Subject != subjectSentinel || got.Body != bodySentinel {
		t.Fatalf("alpha filed %+v", got)
	}
	rel.recorder.await(t, "alpha to tell the recipient", func() bool {
		return slices.ContainsFunc(alpha.gc.commands(), func(c []string) bool { return isNudge(c, "reader") })
	})

	reply, err := Send(context.Background(), alpha.settings, alpha.deps(rel.transport, nil), Outgoing{To: "hub/writer", From: "reader", Subject: "Re: " + subjectSentinel, Body: "reply " + bodySentinel})
	if err != nil {
		t.Fatal(err)
	}
	if reply == id {
		t.Fatal("two envelopes share one id")
	}
	hubAPI.recorder.await(t, "hub to file the reply", func() bool { return len(hubAPI.inbox("writer")) == 1 })
	if back := hubAPI.inbox("writer")[0]; back.From != "alpha/reader" || back.Subject != "Re: "+subjectSentinel {
		t.Fatalf("hub filed %+v", back)
	}

	store := rel.storeBytes()
	for _, secret := range []string{subjectSentinel, bodySentinel} {
		if bytes.Contains(store, []byte(secret)) {
			t.Fatalf("relay store holds %q", secret)
		}
	}

	var grants []string
	for _, r := range rel.recorder.all() {
		grants = append(grants, r.header.Values("X-GC-Relay-Grant")...)
	}
	for _, city := range []*cityFixture{hub, alpha} {
		log := city.log.String()
		secrets := append([]string{subjectSentinel, bodySentinel, base64.StdEncoding.EncodeToString(city.signer.Seed())}, keyFileContents(t, city)...)
		for _, g := range grants {
			secrets = append(secrets, g, strings.SplitN(g, ".", 2)[0])
		}
		for _, secret := range secrets {
			if secret != "" && strings.Contains(log, secret) {
				t.Fatalf("%s log carries a secret %.24q", city.id, secret)
			}
		}
		asks := city.log.lines("ask ")
		if want := askCount(rel.recorder, city.id); len(asks) < want-1 || len(asks) > want {
			t.Fatalf("%s logged %d ask lines for %d reads", city.id, len(asks), want)
		}
		for _, line := range asks {
			if !strings.Contains(line, " outcome=") || !strings.Contains(line, " began=") || !strings.Contains(line, " ended=") {
				t.Fatalf("%s ask line lacks its timing or outcome: %q", city.id, line)
			}
		}
	}
}

func askedBy(r *recorder, city string) bool { return askCount(r, city) > 0 }

func askCount(r *recorder, city string) int {
	return len(r.matching("GET", "/v1/city/"+city+"/queue"))
}

func isNudge(cmd []string, recipient string) bool {
	return len(cmd) >= 4 && cmd[0] == "session" && cmd[1] == "nudge" && slices.Contains(cmd, recipient)
}

func keyFileContents(t *testing.T, c *cityFixture) []string {
	t.Helper()
	var out []string
	matches, err := filepath.Glob(filepath.Join(c.settings.EncryptionKeysDir, "*.key"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, strings.TrimSpace(string(b)))
	}
	return out
}
