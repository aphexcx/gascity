package envelope

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hpke"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

var instant = time.Unix(1800000000, 0)

// The fixed seeds and X25519 scalars in these tests are made-up test keys.
func signingKey(fill byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{fill}, ed25519.SeedSize))
}

func boxKey(t *testing.T, fill byte) *ecdh.PrivateKey {
	t.Helper()
	key, err := ParsePrivateKey(bytes.Repeat([]byte{fill}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func sample() (Label, Mail) {
	return Label{Version: 1, ID: "m1", From: "alpha", To: "beta", Sent: instant.Unix()},
		Mail{From: "outbox", To: "inbox", Subject: "a private subject", Body: "a private body", ID: "m1"}
}

func seal(t *testing.T, label Label, mail Mail) Envelope {
	t.Helper()
	env, err := Seal(label, mail, signingKey(1), boxKey(t, 2).PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func open(t *testing.T, env Envelope, now time.Time) (Mail, error) {
	t.Helper()
	return Open(env, "beta", []*ecdh.PrivateKey{boxKey(t, 2)}, "alpha", signingKey(1).Public().(ed25519.PublicKey), now)
}

func TestRoundTrip(t *testing.T) {
	label, mail := sample()
	wire, err := seal(t, label, mail).MarshalBinary()
	if err != nil || wire[0] != 1 {
		t.Fatalf("wire version: %v", err)
	}
	env, err := ParseEnvelope(wire)
	if err != nil || env.Label != label {
		t.Fatalf("parsed label = %+v, %v", env.Label, err)
	}
	got, err := open(t, env, instant)
	if got != mail || err != nil {
		t.Fatalf("opened = %+v, %v", got, err)
	}
}

func TestLabelBinding(t *testing.T) {
	label, mail := sample()
	original := seal(t, label, mail)
	for _, change := range []func(*Label){
		func(l *Label) { l.Version++ }, func(l *Label) { l.ID = "n1" },
		func(l *Label) { l.From = "blpha" }, func(l *Label) { l.To = "zeta" },
		func(l *Label) { l.Sent++ },
	} {
		env := original
		change(&env.Label)
		if _, err := open(t, env, instant); !errors.Is(err, ErrNotOpened) {
			t.Errorf("changed label %+v: %v", env.Label, err)
		}
	}
}

func TestOpenChecks(t *testing.T) {
	label, mail := sample()
	env := seal(t, label, mail)
	signer := signingKey(1).Public().(ed25519.PublicKey)
	right, wrong := boxKey(t, 2), boxKey(t, 3)
	for _, tc := range []struct {
		name, own, sender string
		keys              []*ecdh.PrivateKey
		key               ed25519.PublicKey
		want              error
	}{
		{"wrong key", "beta", "alpha", []*ecdh.PrivateKey{wrong}, signer, ErrNotOpened},
		{"rotated", "beta", "alpha", []*ecdh.PrivateKey{wrong, right}, signer, nil},
		{"wrong signature", "beta", "alpha", []*ecdh.PrivateKey{right}, signingKey(3).Public().(ed25519.PublicKey), ErrSignature},
		{"wrong recipient", "hub", "alpha", []*ecdh.PrivateKey{right}, signer, ErrAddress},
		{"wrong sender identity", "beta", "hub", []*ecdh.PrivateKey{right}, signer, ErrAddress},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Open(env, tc.own, tc.keys, tc.sender, tc.key, instant)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Open = %v, want %v", err, tc.want)
			}
		})
	}
	// A peer can sign and encrypt inconsistent IDs even though Seal refuses them.
	mail.ID = "m2"
	if _, err := Seal(label, mail, signingKey(1), right.PublicKey()); !errors.Is(err, ErrAddress) {
		t.Fatalf("Seal mismatched ID = %v", err)
	}
	info, err := label.bytes()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := mail.bytes()
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(signingKey(1), signedMail(info, payload))
	pub, err := hpke.NewDHKEMPublicKey(right.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	env.Sealed, err = hpke.Seal(pub, hpke.HKDFSHA256(), hpke.ChaCha20Poly1305(), info, append(sig, payload...))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := open(t, env, instant); !errors.Is(err, ErrAddress) {
		t.Fatalf("signed mismatched ID = %v", err)
	}
}

func TestFreshness(t *testing.T) {
	label, mail := sample()
	env := seal(t, label, mail)
	for _, tc := range []struct {
		age  time.Duration
		want error
	}{
		{31 * 24 * time.Hour, ErrTime},
		{29 * 24 * time.Hour, nil},
		{30 * 24 * time.Hour, nil},
		{30*24*time.Hour + time.Nanosecond, ErrTime},
		{-6 * time.Minute, ErrTime},
		{-5 * time.Minute, nil},
	} {
		if _, err := open(t, env, instant.Add(tc.age)); !errors.Is(err, tc.want) {
			t.Errorf("age %v: %v", tc.age, err)
		}
	}
}

func TestWireLimitsAndPrivacy(t *testing.T) {
	label, mail := sample()
	for _, size := range []int{1024, 262144} {
		mail.Body = strings.Repeat("~", size-len(mail.Subject))
		wire, err := seal(t, label, mail).MarshalBinary()
		if err != nil || len(wire) > MaxEnvelopeSize || bytes.Contains(wire, []byte(mail.Subject)) || bytes.Contains(wire, []byte(mail.Body)) {
			t.Fatalf("%d-byte mail in %d wire bytes: %v", size, len(wire), err)
		}
	}
	mail.Body += "!"
	if _, err := Seal(label, mail, nil, nil); !errors.Is(err, ErrSize) {
		t.Fatalf("oversize before signing = %v", err)
	}
}

func TestWireBounds(t *testing.T) {
	label, mail := sample()
	label.ID = strings.Repeat("i", 65535)
	label.From, label.To = strings.Repeat("a", 40), strings.Repeat("b", 40)
	mail.ID, mail.From, mail.To = label.ID, label.ID, label.ID
	mail.Body, mail.Subject = strings.Repeat("x", MaxMailSize), ""
	env := seal(t, label, mail)
	wire, err := env.MarshalBinary()
	if err != nil || len(wire) != MaxEnvelopeSize {
		t.Fatalf("maximum wire = %d, %v", len(wire), err)
	}
	if _, err := ParseEnvelope(wire); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseEnvelope(append(wire, 0)); err == nil {
		t.Fatal("accepted oversized envelope")
	}
	label, mail = sample()
	env = seal(t, label, mail)
	wire, err = env.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	// Truncation in the label or ciphertext must never deliver mail.
	for end := range len(wire) {
		short, err := ParseEnvelope(wire[:end])
		if err == nil {
			if _, err := open(t, short, instant); err == nil {
				t.Fatalf("accepted %d of %d bytes", end, len(wire))
			}
		}
	}
	statement := fixture(t, "testdata/statement.b64")
	for end := range len(statement) {
		if _, err := ParseStatement(statement[:end]); err == nil {
			t.Fatalf("accepted %d statement bytes", end)
		}
	}
	if _, err := ParseStatement(append(statement, 0)); err == nil {
		t.Fatal("accepted trailing statement byte")
	}
}

func fixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestFixedVectors(t *testing.T) {
	wire := fixture(t, "testdata/envelope.b64")
	label, want := sample()
	want.Subject, want.Body = "", "~"
	if bytes.Contains(wire, []byte(want.Body)) {
		t.Fatal("one-byte fixture contains the plaintext marker")
	}
	env, err := ParseEnvelope(wire)
	if err != nil || env.Label != label {
		t.Fatalf("fixed label = %+v, %v", env.Label, err)
	}
	if got, err := open(t, env, instant); got != want || err != nil {
		t.Fatalf("fixed mail = %+v, %v", got, err)
	}
	statement, err := ParseStatement(fixture(t, "testdata/statement.b64"))
	if err != nil {
		t.Fatal(err)
	}
	if err := statement.Verify("beta", signingKey(3).Public().(ed25519.PublicKey)); err != nil {
		t.Fatal(err)
	}
	if statement.KeyID != "k1" || statement.Issued != instant.Unix() || !statement.PublicKey.Equal(boxKey(t, 2).PublicKey()) {
		t.Fatal("fixed statement fields changed")
	}
}

func TestCityIDs(t *testing.T) {
	for _, tc := range []struct {
		id    string
		valid bool
	}{
		{"0", true},
		{"a-1", true},
		{strings.Repeat("a", 40), true},
		{"", false},
		{"-a", false},
		{"Alpha", false},
		{"a/b", false},
		{"ß", false},
		{strings.Repeat("a", 41), false},
	} {
		if ValidCityID(tc.id) != tc.valid {
			t.Errorf("ValidCityID(%q) != %t", tc.id, tc.valid)
		}
	}
}

func TestKeyMaterialAndStatements(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := MarshalPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	copyKey, err := ParsePrivateKey(raw)
	if err != nil || !copyKey.Equal(key) {
		t.Fatalf("key round trip: %v", err)
	}
	if _, err := ParsePrivateKey(raw[:len(raw)-1]); err == nil {
		t.Fatal("accepted truncated key")
	}
	signer := signingKey(3)
	makeStatement := func(issued int64) KeyStatement {
		s, err := SignStatement("beta", "k1", key.PublicKey(), issued, signer)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	old, latest := makeStatement(10), makeStatement(11)
	wire, err := latest.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseStatement(wire)
	if err != nil {
		t.Fatal(err)
	}
	pub := signer.Public().(ed25519.PublicKey)
	if err := got.Verify("beta", pub); err != nil {
		t.Fatal(err)
	}
	if err := got.Verify("hub", pub); err == nil {
		t.Fatal("accepted another identity")
	}
	if err := got.Verify("beta", signingKey(4).Public().(ed25519.PublicKey)); !errors.Is(err, ErrSignature) {
		t.Fatalf("wrong signing key: %v", err)
	}
	got.Issued++
	if err := got.Verify("beta", pub); !errors.Is(err, ErrSignature) {
		t.Fatalf("tampered statement: %v", err)
	}
	if err := old.CheckSuccessor(latest); err != nil {
		t.Fatal(err)
	}
	if err := latest.CheckSuccessor(latest); err != nil {
		t.Fatal(err)
	}
	if err := latest.CheckSuccessor(old); err == nil {
		t.Fatal("accepted rollback")
	}
}
