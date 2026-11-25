// Package envelope implements the versioned mail formats shared by a relay
// and its clients. Only the routing label is visible outside the HPKE seal.
package envelope

import (
	"encoding/binary"
	"errors"
	"math"
)

// Version identifies the wire format and cryptographic suite.
const Version = 1

// MaxMailSize bounds the combined subject and body in bytes.
const MaxMailSize = 256 * 1024

const (
	maxCityID        = 40
	maxLabelSize     = 1 + 3*2 + math.MaxUint16 + 2*maxCityID + 8
	maxPlainMailSize = 3*(2+math.MaxUint16) + 4 + MaxMailSize
	// X25519 encapsulation, ChaCha20-Poly1305 tag, and Ed25519 signature.
	sealOverhead = 32 + 16 + 64
)

// MaxEnvelopeSize bounds a serialized envelope, including all address and ID fields.
const MaxEnvelopeSize = maxLabelSize + maxPlainMailSize + sealOverhead

// Wire and validation errors returned by the codecs.
var (
	ErrFormat  = errors.New("invalid relay encoding")
	ErrSize    = errors.New("relay mail exceeds size limit")
	ErrAddress = errors.New("relay identity or message ID mismatch")
)

// ValidCityID reports whether id matches the relay city namespace.
func ValidCityID(id string) bool {
	if len(id) == 0 || len(id) > maxCityID || id[0] == '-' {
		return false
	}
	for i := range len(id) {
		c := id[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// Label contains the public routing fields. Sent is a Unix timestamp in seconds.
type Label struct {
	Version      byte
	ID, From, To string
	Sent         int64
}

// Mail contains the private addresses and content. ID repeats the public message ID.
type Mail struct {
	From, To, Subject, Body, ID string
}

// Envelope pairs the clear routing label with the HPKE encapsulation and ciphertext.
type Envelope struct {
	Label  Label
	Sealed []byte
}

// Short strings are nonempty byte strings prefixed with a big-endian uint16.
func shortString(s string) bool { return len(s) > 0 && len(s) <= math.MaxUint16 }

func appendShort(b []byte, s string) []byte {
	b = binary.BigEndian.AppendUint16(b, uint16(len(s)))
	return append(b, s...)
}

// The label is self-delimiting so its exact bytes can prefix both the wire
// envelope and the signed message.
func (l Label) bytes() ([]byte, error) {
	if l.Version != Version || !shortString(l.ID) {
		return nil, ErrFormat
	}
	if !ValidCityID(l.From) || !ValidCityID(l.To) {
		return nil, ErrAddress
	}
	b := appendShort([]byte{l.Version}, l.ID)
	b = appendShort(b, l.From)
	b = appendShort(b, l.To)
	return binary.BigEndian.AppendUint64(b, uint64(l.Sent)), nil
}

func (m Mail) bytes() ([]byte, error) {
	if len(m.Subject) > MaxMailSize || len(m.Body) > MaxMailSize-len(m.Subject) {
		return nil, ErrSize
	}
	if !shortString(m.From) || !shortString(m.To) || !shortString(m.ID) {
		return nil, ErrFormat
	}
	b := appendShort(nil, m.From)
	b = appendShort(b, m.To)
	b = appendShort(b, m.ID)
	b = binary.BigEndian.AppendUint32(b, uint32(len(m.Subject)))
	b = append(b, m.Subject...)
	return append(b, m.Body...), nil
}

type reader struct {
	b   []byte
	err error
}

func (r *reader) short() string {
	if len(r.b) < 2 {
		r.err = ErrFormat
		return ""
	}
	n := int(binary.BigEndian.Uint16(r.b))
	if n == 0 || n > len(r.b)-2 {
		r.err = ErrFormat
		return ""
	}
	s := string(r.b[2 : 2+n])
	r.b = r.b[2+n:]
	return s
}

func parseMail(b []byte) (Mail, error) {
	r := reader{b: b}
	m := Mail{From: r.short(), To: r.short(), ID: r.short()}
	if r.err != nil || len(r.b) < 4 {
		return Mail{}, ErrFormat
	}
	n := binary.BigEndian.Uint32(r.b)
	payload := r.b[4:]
	if len(payload) > MaxMailSize {
		return Mail{}, ErrSize
	}
	if n > uint32(len(payload)) {
		return Mail{}, ErrFormat
	}
	m.Subject, m.Body = string(payload[:n]), string(payload[n:])
	return m, nil
}

// MarshalBinary encodes the version first, then the rest of the label and the seal.
func (e Envelope) MarshalBinary() ([]byte, error) {
	label, err := e.Label.bytes()
	if err != nil {
		return nil, err
	}
	if len(e.Sealed) < sealOverhead || len(e.Sealed) > maxPlainMailSize+sealOverhead {
		return nil, ErrFormat
	}
	return append(label, e.Sealed...), nil
}

// ParseEnvelope decodes and bounds an envelope before any private-key operation.
// Its sealed bytes refer to the input buffer.
func ParseEnvelope(data []byte) (Envelope, error) {
	if len(data) == 0 || len(data) > MaxEnvelopeSize || data[0] != Version {
		return Envelope{}, ErrFormat
	}
	r := reader{b: data[1:]}
	label := Label{Version: data[0], ID: r.short(), From: r.short(), To: r.short()}
	if r.err != nil || len(r.b) < 8+sealOverhead || len(r.b)-8 > maxPlainMailSize+sealOverhead {
		return Envelope{}, ErrFormat
	}
	if !ValidCityID(label.From) || !ValidCityID(label.To) {
		return Envelope{}, ErrAddress
	}
	label.Sent = int64(binary.BigEndian.Uint64(r.b))
	return Envelope{Label: label, Sealed: r.b[8:]}, nil
}
