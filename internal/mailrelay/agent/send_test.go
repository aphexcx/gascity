package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/mailrelay/envelope"
)

// sendPair sets up hub sending to alpha through a real relay, with alpha's
// key statement published.
func sendPair(t *testing.T) (hub, alpha *cityFixture, rel *relayFixture) {
	t.Helper()
	hub, alpha = pair(t)
	rel = newRelayFixture(t, map[string]ed25519.PublicKey{"hub": hub.public(), "alpha": alpha.public()})
	publish(t, rel, alpha)
	return hub, alpha, rel
}

// publish puts a city's newest key statement on the relay.
func publish(t *testing.T, rel *relayFixture, c *cityFixture) {
	t.Helper()
	ring, err := loadKeyRing(c.settings.EncryptionKeysDir, c.id, c.public())
	if err != nil {
		t.Fatal(err)
	}
	put(t, rel, c, ring.newestWire)
}

func put(t *testing.T, rel *relayFixture, c *cityFixture, wire []byte) {
	t.Helper()
	req, err := http.NewRequest("PUT", relayBase+"/v1/city/"+c.id+"/key", bytes.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := envelope.MintGrant(c.signer, c.id, c.id, envelope.RequestFromURL("PUT", req.URL, wire), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-GC-Relay-Grant", tok)
	resp, err := rel.transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("publishing %s's key: %d", c.id, resp.StatusCode)
	}
}

func outgoing() Outgoing {
	return Outgoing{To: "alpha/reader", From: "writer", Subject: subjectSentinel, Body: bodySentinel}
}

// A9: a statement signed by any key but the pinned one is refused and
// nothing is posted.
func TestSendRefusesKeyNotSignedByPinnedKey(t *testing.T) {
	hub, _, rel := sendPair(t)
	_, swapped, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hub.pinKey("alpha", swapped.Public().(ed25519.PublicKey))
	_, err = Send(context.Background(), hub.settings, hub.deps(rel.transport, nil), outgoing())
	if err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("send with a swapped key: %v", err)
	}
	if n := len(rel.recorder.matching("POST", "/envelopes")); n != 0 {
		t.Fatalf("send posted %d envelopes", n)
	}
}

// A9: a statement older than the newest one seen is refused.
func TestSendRefusesOlderStatement(t *testing.T) {
	hub, alpha, rel := sendPair(t)
	older, err := loadKeyRing(alpha.settings.EncryptionKeysDir, "alpha", alpha.public())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Keygen(alpha.settings, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	publish(t, rel, alpha)
	if _, err := Send(context.Background(), hub.settings, hub.deps(rel.transport, nil), outgoing()); err != nil {
		t.Fatal(err)
	}
	posted := len(rel.recorder.matching("POST", "/envelopes"))
	rollback := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/keys/alpha") {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(older.newestWire)), Request: r}, nil
		}
		return rel.transport.RoundTrip(r)
	})
	_, err = Send(context.Background(), hub.settings, hub.deps(rollback, nil), outgoing())
	if err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("send with a rolled-back key: %v", err)
	}
	if n := len(rel.recorder.matching("POST", "/envelopes")); n != posted {
		t.Fatal("send posted after a rolled-back key")
	}
}

// A12: a lost answer leads to a second post of the same envelope.
func TestSendPostsAgainWithSameIDWhenAnswerIsLost(t *testing.T) {
	hub, _, rel := sendPair(t)
	var lost atomic.Bool
	transport := rel.recorder.wrap(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := handlerTransport{rel.server}.RoundTrip(r)
		if r.Method == "POST" && lost.CompareAndSwap(false, true) {
			if err == nil {
				_ = resp.Body.Close()
			}
			return nil, errors.New("connection reset by peer")
		}
		return resp, err
	}))
	id, err := Send(context.Background(), hub.settings, hub.deps(transport, nil), outgoing())
	if err != nil {
		t.Fatal(err)
	}
	posts := rel.recorder.matching("POST", "/envelopes")
	if len(posts) != 2 || !bytes.Equal(posts[0].body, posts[1].body) {
		t.Fatalf("%d posts; same bytes: %v", len(posts), len(posts) == 2 && bytes.Equal(posts[0].body, posts[1].body))
	}
	env, err := envelope.ParseEnvelope(posts[0].body)
	if err != nil || env.Label.ID != id {
		t.Fatalf("posted id %q, printed %q", env.Label.ID, id)
	}
}

// A12: a pair the relay refuses makes send fail with the relay's status.
func TestSendReportsRelayRefusal(t *testing.T) {
	_, alpha, rel := sendPair(t)
	beta := newCityFixture(t, "beta")
	beta.keygen()
	rel.trust("beta", beta.public())
	alpha.pin(beta)
	_, err := Send(context.Background(), alpha.settings, alpha.deps(rel.transport, nil), Outgoing{To: "beta/reader", From: "reader", Subject: "s", Body: "b"})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("send to a refused pair: %v", err)
	}
	if n := len(rel.recorder.matching("POST", "/envelopes")); n != 0 {
		t.Fatalf("posted %d envelopes", n)
	}
}

func TestSendGivesEachEnvelopeAFreshID(t *testing.T) {
	hub, _, rel := sendPair(t)
	first, err := Send(context.Background(), hub.settings, hub.deps(rel.transport, nil), outgoing())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Send(context.Background(), hub.settings, hub.deps(rel.transport, nil), outgoing())
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two sends reused an id")
	}
}

func TestSendRefusesAddressesItCannotDeliver(t *testing.T) {
	hub, _, rel := sendPair(t)
	for name, m := range map[string]Outgoing{
		"no city":       {To: "reader", From: "writer", Subject: "s"},
		"no address":    {To: "alpha/", From: "writer", Subject: "s"},
		"not a peer":    {To: "beta/reader", From: "writer", Subject: "s"},
		"no subject":    {To: "alpha/reader", From: "writer"},
		"bad city name": {To: "Alpha/reader", From: "writer", Subject: "s"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Send(context.Background(), hub.settings, hub.deps(rel.transport, nil), m); err == nil {
				t.Fatal("send accepted it")
			}
		})
	}
	if n := len(rel.recorder.all()); n != 1 {
		t.Fatalf("refused sends made %d relay requests beyond the publish", n-1)
	}
}
