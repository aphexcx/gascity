package envelope

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hpke"
	"errors"
	"fmt"
	"time"
)

const mailContext = "gc-mail-relay.v1/mail\x00"

// Open returns a distinct error for each authentication or freshness check.
var (
	ErrNotOpened = errors.New("relay envelope did not open")
	ErrSignature = errors.New("relay signature did not verify")
	ErrTime      = errors.New("relay mail outside delivery window")
	ErrKey       = errors.New("invalid relay key")
)

func signedMail(info, mail []byte) []byte {
	b := make([]byte, 0, len(mailContext)+len(info)+len(mail))
	b = append(b, mailContext...)
	b = append(b, info...)
	return append(b, mail...)
}

// Seal signs the label and mail with the sender's Ed25519 key, then encrypts
// the signature and mail to the recipient's X25519 public key.
func Seal(label Label, mail Mail, signer ed25519.PrivateKey, recipient *ecdh.PublicKey) (Envelope, error) {
	info, err := label.bytes()
	if err != nil {
		return Envelope{}, err
	}
	payload, err := mail.bytes()
	if err != nil {
		return Envelope{}, err
	}
	if mail.ID != label.ID {
		return Envelope{}, ErrAddress
	}
	if len(signer) != ed25519.PrivateKeySize || recipient == nil || recipient.Curve() != ecdh.X25519() {
		return Envelope{}, ErrKey
	}
	pk, err := hpke.NewDHKEMPublicKey(recipient)
	if err != nil {
		return Envelope{}, fmt.Errorf("HPKE recipient: %w", err)
	}
	sig := ed25519.Sign(signer, signedMail(info, payload))
	sealed, err := hpke.Seal(pk, hpke.HKDFSHA256(), hpke.ChaCha20Poly1305(), info, append(sig, payload...))
	if err != nil {
		return Envelope{}, fmt.Errorf("sealing mail: %w", err)
	}
	return Envelope{Label: label, Sealed: sealed}, nil
}

// Open tries the recipient's keys, authenticates the sender, checks the
// recipient and repeated ID, and enforces the delivery window. senderCity
// identifies the trusted signer key. The caller deduplicates the returned ID.
func Open(env Envelope, ownCity string, keys []*ecdh.PrivateKey, senderCity string, signer ed25519.PublicKey, now time.Time) (Mail, error) {
	info, err := env.Label.bytes()
	if err != nil || len(env.Sealed) < sealOverhead || len(env.Sealed) > maxPlainMailSize+sealOverhead {
		return Mail{}, ErrNotOpened
	}
	for _, key := range keys {
		if key == nil || key.Curve() != ecdh.X25519() {
			continue
		}
		sk, err := hpke.NewDHKEMPrivateKey(key)
		if err != nil {
			return Mail{}, fmt.Errorf("HPKE recipient: %w", err)
		}
		plain, err := hpke.Open(sk, hpke.HKDFSHA256(), hpke.ChaCha20Poly1305(), info, env.Sealed)
		if err != nil {
			continue
		}
		return verifyMail(env.Label, info, plain, ownCity, senderCity, signer, now)
	}
	return Mail{}, ErrNotOpened
}

func verifyMail(label Label, info, plain []byte, ownCity, senderCity string, signer ed25519.PublicKey, now time.Time) (Mail, error) {
	if len(plain) < ed25519.SignatureSize || len(signer) != ed25519.PublicKeySize {
		return Mail{}, ErrSignature
	}
	sig, payload := plain[:ed25519.SignatureSize], plain[ed25519.SignatureSize:]
	if !ed25519.Verify(signer, signedMail(info, payload), sig) {
		return Mail{}, ErrSignature
	}
	mail, err := parseMail(payload)
	if err != nil {
		return Mail{}, err
	}
	if label.To != ownCity || label.From != senderCity || mail.ID != label.ID {
		return Mail{}, ErrAddress
	}
	oldest := now.Add(-30 * 24 * time.Hour)
	firstSecond := oldest.Unix()
	if oldest.Nanosecond() != 0 {
		firstSecond++
	}
	if label.Sent < firstSecond || label.Sent > now.Add(5*time.Minute).Unix() {
		return Mail{}, ErrTime
	}
	return mail, nil
}
