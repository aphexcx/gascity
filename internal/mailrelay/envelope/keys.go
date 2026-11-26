package envelope

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
)

const statementContext = "gc-mail-relay.v1/key\x00"

// ErrStaleKey indicates rollback or conflicting statements for the same time.
var ErrStaleKey = errors.New("relay key statement superseded or conflicting")

// GenerateKey makes an independent X25519 encryption key.
func GenerateKey() (*ecdh.PrivateKey, error) { return ecdh.X25519().GenerateKey(rand.Reader) }

// MarshalPrivateKey returns the 32-byte X25519 private scalar.
func MarshalPrivateKey(key *ecdh.PrivateKey) ([]byte, error) {
	if key == nil || key.Curve() != ecdh.X25519() {
		return nil, ErrKey
	}
	return key.Bytes(), nil
}

// ParsePrivateKey decodes the raw X25519 private scalar.
func ParsePrivateKey(data []byte) (*ecdh.PrivateKey, error) { return ecdh.X25519().NewPrivateKey(data) }

// KeyStatement binds a city's encryption key to its signing identity.
// Issued is a Unix timestamp in seconds.
type KeyStatement struct {
	Version     byte
	City, KeyID string
	PublicKey   *ecdh.PublicKey
	Issued      int64
	Signature   [ed25519.SignatureSize]byte
}

func (s KeyStatement) bytes() ([]byte, error) {
	if !ValidCityID(s.City) {
		return nil, ErrAddress
	}
	if s.Version != Version || !shortString(s.KeyID) {
		return nil, ErrFormat
	}
	if s.PublicKey == nil || s.PublicKey.Curve() != ecdh.X25519() {
		return nil, ErrKey
	}
	b := appendShort([]byte{s.Version}, s.City)
	b = appendShort(b, s.KeyID)
	b = append(b, s.PublicKey.Bytes()...)
	return binary.BigEndian.AppendUint64(b, uint64(s.Issued)), nil
}

// SignStatement signs the version, city, key ID, public key and issuance time.
func SignStatement(city, keyID string, key *ecdh.PublicKey, issued int64, signer ed25519.PrivateKey) (KeyStatement, error) {
	s := KeyStatement{Version: Version, City: city, KeyID: keyID, PublicKey: key, Issued: issued}
	b, err := s.bytes()
	if err != nil {
		return KeyStatement{}, err
	}
	if len(signer) != ed25519.PrivateKeySize {
		return KeyStatement{}, ErrKey
	}
	copy(s.Signature[:], ed25519.Sign(signer, append([]byte(statementContext), b...)))
	return s, nil
}

// Verify checks the statement against the city and signing key from a trusted source.
func (s KeyStatement) Verify(city string, signer ed25519.PublicKey) error {
	b, err := s.bytes()
	if err != nil {
		return err
	}
	if s.City != city {
		return ErrAddress
	}
	if len(signer) != ed25519.PublicKeySize || !ed25519.Verify(signer, append([]byte(statementContext), b...), s.Signature[:]) {
		return ErrSignature
	}
	return nil
}

// CheckSuccessor compares two already verified statements for the same city.
// An identical re-publication is accepted.
func (s KeyStatement) CheckSuccessor(next KeyStatement) error {
	if !ValidCityID(s.City) || s.City != next.City {
		return ErrAddress
	}
	if next.Issued < s.Issued || (next.Issued == s.Issued && next.Signature != s.Signature) {
		return ErrStaleKey
	}
	return nil
}

// MarshalBinary appends the Ed25519 signature to the statement fields.
func (s KeyStatement) MarshalBinary() ([]byte, error) {
	b, err := s.bytes()
	if err != nil {
		return nil, err
	}
	return append(b, s.Signature[:]...), nil
}

// ParseStatement decodes a signed statement. Verify it before using its key.
func ParseStatement(data []byte) (KeyStatement, error) {
	if len(data) == 0 || data[0] != Version {
		return KeyStatement{}, ErrFormat
	}
	r := reader{b: data[1:]}
	s := KeyStatement{Version: data[0], City: r.short(), KeyID: r.short()}
	if r.err != nil || len(r.b) != 32+8+ed25519.SignatureSize {
		return KeyStatement{}, ErrFormat
	}
	if !ValidCityID(s.City) {
		return KeyStatement{}, ErrAddress
	}
	key, err := ecdh.X25519().NewPublicKey(r.b[:32])
	if err != nil {
		return KeyStatement{}, err
	}
	s.PublicKey = key
	s.Issued = int64(binary.BigEndian.Uint64(r.b[32:40]))
	copy(s.Signature[:], r.b[40:])
	return s, nil
}
