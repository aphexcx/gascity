package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/mailrelay/envelope"
)

// sendAttempts bounds how often send posts one envelope when answers are lost.
const sendAttempts = 5

// Outgoing is one mail to send. To is a peer's city id, a slash and an
// address in that city; From is the sender's address in this city.
type Outgoing struct {
	To, From, Subject, Body string
}

// Send seals one mail to the peer's newest key and posts it to the relay.
// The peer's key statement must verify under the peer's pinned signing key
// and must not be older than the newest one seen. Each mail gets a fresh
// random id; a post whose answer is lost is posted again with the same id.
// It returns the id.
func Send(ctx context.Context, s Settings, d Deps, m Outgoing) (string, error) {
	if err := s.checkSend(); err != nil {
		return "", err
	}
	signer, err := readSigningKey(s.SigningKey)
	if err != nil {
		return "", err
	}
	peer, address, ok := strings.Cut(m.To, "/")
	switch {
	case !ok || !envelope.ValidCityID(peer) || address == "":
		return "", fmt.Errorf("recipient %q must be a relay city id, a slash and an address", m.To)
	case m.From == "":
		return "", errors.New("a sender address is required")
	case m.Subject == "":
		return "", errors.New("a subject is required")
	}
	peers, err := loadPeers(s.PeersDir)
	if err != nil {
		return "", err
	}
	pinned, ok := peers[peer]
	if !ok {
		return "", fmt.Errorf("%s is not a peer: no %s.pub in %s", peer, peer, s.PeersDir)
	}
	if err := ensureStateDir(s.StateDir); err != nil {
		return "", err
	}
	d = d.withDefaults(s)
	rc, err := newRelayClient(s, signer, d.RelayTransport)
	if err != nil {
		return "", err
	}
	stmt, err := peerKey(ctx, rc, s.StateDir, peer, pinned)
	if err != nil {
		return "", err
	}
	id := rand.Text()
	label := envelope.Label{Version: envelope.Version, ID: id, From: s.City, To: peer, Sent: d.Now().Unix()}
	env, err := envelope.Seal(label, envelope.Mail{From: m.From, To: address, Subject: m.Subject, Body: m.Body, ID: id}, signer, stmt.PublicKey)
	if err != nil {
		return "", fmt.Errorf("sealing mail: %w", err)
	}
	wire, err := env.MarshalBinary()
	if err != nil {
		return "", err
	}
	if err := postEnvelope(ctx, rc, wire, d.Sleep); err != nil {
		return "", err
	}
	return id, nil
}

// peerKey fetches a peer's key statement and accepts it only when the
// peer's pinned signing key signed it and it is no older than the newest
// statement seen for that peer, which it then keeps.
func peerKey(ctx context.Context, rc *relayClient, stateDir, peer string, pinned ed25519.PublicKey) (envelope.KeyStatement, error) {
	code, wire, err := rc.peerKey(ctx, peer)
	if err != nil {
		return envelope.KeyStatement{}, fmt.Errorf("fetching %s's key statement: reason=%s", peer, transportReason(ctx, err))
	}
	if code != http.StatusOK {
		return envelope.KeyStatement{}, fmt.Errorf("relay answered HTTP %d to the key request for %s", code, peer)
	}
	stmt, err := envelope.ParseStatement(wire)
	if err != nil {
		return envelope.KeyStatement{}, fmt.Errorf("relay sent a malformed key statement for %s: %w", peer, err)
	}
	if err := stmt.Verify(peer, pinned); err != nil {
		return envelope.KeyStatement{}, fmt.Errorf("refused the relay's key statement for %s: it does not verify under the pinned signing key: %w", peer, err)
	}
	unlock, err := lockDir(filepath.Join(stateDir, peersDir), true)
	if err != nil {
		return envelope.KeyStatement{}, err
	}
	defer unlock()
	prevWire, seen, err := peerStatement(stateDir, peer)
	if err != nil {
		return envelope.KeyStatement{}, err
	}
	if seen {
		prev, err := envelope.ParseStatement(prevWire)
		// A kept statement that no longer verifies predates a new pin; the
		// pinned key decides, so it sets no floor.
		if err == nil && prev.Verify(peer, pinned) == nil {
			if err := prev.CheckSuccessor(stmt); err != nil {
				return envelope.KeyStatement{}, fmt.Errorf("refused the relay's key statement for %s: it is older than, or conflicts with, the newest seen (issued %d, newest %d)", peer, stmt.Issued, prev.Issued)
			}
			if stmt.Issued == prev.Issued {
				return stmt, nil
			}
		}
	}
	if err := savePeerStatement(stateDir, peer, wire); err != nil {
		return envelope.KeyStatement{}, err
	}
	return stmt, nil
}

// postEnvelope posts until the relay stores the envelope. A lost answer or a
// server error is posted again, the same bytes and id; a refusal is final.
// The relay never stores a refused post, including a 409 for an id it holds
// for another pair.
func postEnvelope(ctx context.Context, rc *relayClient, wire []byte, sleep func(context.Context, time.Duration) error) error {
	var b backoff
	for attempt := 1; ; attempt++ {
		code, err := rc.post(ctx, wire)
		switch {
		case err == nil && code == http.StatusNoContent:
			return nil
		case err == nil && code != http.StatusRequestTimeout && code < 500:
			return fmt.Errorf("relay refused the mail with HTTP %d", code)
		case attempt == sendAttempts && err != nil:
			return fmt.Errorf("posting the mail: no answer after %d attempts: reason=%s", attempt, transportReason(ctx, err))
		case attempt == sendAttempts:
			return fmt.Errorf("posting the mail: relay answered HTTP %d after %d attempts", code, attempt)
		}
		if err := sleep(ctx, b.step()); err != nil {
			return fmt.Errorf("posting the mail: %w", err)
		}
	}
}
