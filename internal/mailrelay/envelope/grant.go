package envelope

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citywriteauth"
)

// GrantAudience is the sole audience accepted by the relay.
const GrantAudience = "gc-mail-relay.v1"

const maxGrantTTL = 2 * time.Minute

// Request contains the parts of a relay request bound by its grant. Path is
// the request URL's path without the query, starting with /v1/city/{city}/.
type Request struct {
	Method, Path, RawQuery string
	Body                   []byte
}

func (r Request) expectation() (citywriteauth.Expect, error) {
	tail, found := strings.CutPrefix(r.Path, "/v1/city/")
	city, rest, slash := strings.Cut(tail, "/")
	if !found || !slash || !ValidCityID(city) || r.Method == "" || rest == "" {
		return citywriteauth.Expect{}, ErrAddress
	}
	return citywriteauth.Expect{City: city, ReqDigest: citywriteauth.ReqDigest(r.Method, r.Path, r.RawQuery, r.Body)}, nil
}

// MintGrant signs a fresh, single-use request grant using the existing city
// grant wire format. The signing key ID, city and request path must agree.
func MintGrant(key ed25519.PrivateKey, kid, city string, req Request, ttl time.Duration) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", ErrKey
	}
	if ttl < time.Second || ttl > maxGrantTTL {
		return "", errors.New("relay grant lifetime must be between one and 120 seconds")
	}
	expect, err := req.expectation()
	if err != nil {
		return "", err
	}
	if kid != expect.City || city != expect.City {
		return "", citywriteauth.ErrCityMismatch
	}
	now := time.Now().Unix()
	g := citywriteauth.Grant{Kid: kid, Aud: GrantAudience, City: city, IAT: now, Exp: now + int64(ttl/time.Second), JTI: rand.Text(), Req: expect.ReqDigest}
	data, err := json.Marshal(g)
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(key, data)
	return base64.RawURLEncoding.EncodeToString(data) + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// GrantVerifier verifies request grants and consumes their single-use IDs.
type GrantVerifier struct{ verifier *citywriteauth.Verifier }

// NewGrantVerifier pins the relay audience, lifetime cap and 30-second skew.
// A nil replay guard uses the city verifier's in-memory single-use guard.
func NewGrantVerifier(keys map[string]ed25519.PublicKey, replay citywriteauth.ReplayGuard) (*GrantVerifier, error) {
	for city := range keys {
		if !ValidCityID(city) {
			return nil, ErrAddress
		}
	}
	v, err := citywriteauth.New(citywriteauth.Options{Keys: keys, Aud: GrantAudience, MaxTTL: maxGrantTTL, Skew: 30 * time.Second, Replay: replay})
	if err != nil {
		return nil, err
	}
	return &GrantVerifier{verifier: v}, nil
}

// Verify checks the signed request and requires its authenticated signer to
// be the city in the request path.
func (v *GrantVerifier) Verify(token string, req Request) error {
	expect, err := req.expectation()
	if err != nil {
		return err
	}
	g, err := v.verifier.Verify(token, expect)
	if err != nil {
		return err
	}
	if g.Kid != g.City || g.City != expect.City {
		return citywriteauth.ErrCityMismatch
	}
	return nil
}
