package envelope

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citywriteauth"
)

// GrantAudience is the sole audience accepted by the relay.
const GrantAudience = "gc-mail-relay.v1"

const maxGrantTTL = 2 * time.Minute

// A method is a nonempty HTTP token (RFC 9110, section 9.1).
var methodToken = regexp.MustCompile("^[!#$%&'*+.^_`|~A-Za-z0-9-]+$")

// Request contains the parts of a relay request bound by its grant. Path is
// the escaped path without the query, exactly as URL.EscapedPath reports it
// and never URL.Path, starting with /v1/city/{city}/. The city and the request
// binding are read from that escaped form, so an escaped slash and a path
// separator never share a binding, and a city segment that carries a percent
// escape is refused. The minting side and the verifying side both build a
// Request with RequestFromURL.
type Request struct {
	Method, Path, RawQuery string
	Body                   []byte
}

// RequestFromURL derives the Request bound by a grant from a request URL, the
// same way on the minting and the verifying side: Path is u.EscapedPath() and
// RawQuery is u.RawQuery. A nil URL gives an empty path, which MintGrant and
// Verify refuse with ErrAddress.
func RequestFromURL(method string, u *url.URL, body []byte) Request {
	if u == nil {
		return Request{Method: method, Body: body}
	}
	return Request{Method: method, Path: u.EscapedPath(), RawQuery: u.RawQuery, Body: body}
}

func (r Request) expectation() (citywriteauth.Expect, error) {
	control := func(c rune) bool { return c < 0x20 || c == 0x7f }
	if !methodToken.MatchString(r.Method) || strings.ContainsFunc(r.Path, control) || strings.ContainsFunc(r.RawQuery, control) {
		return citywriteauth.Expect{}, ErrFormat
	}
	tail, found := strings.CutPrefix(r.Path, "/v1/city/")
	city, rest, slash := strings.Cut(tail, "/")
	if !found || !slash || !ValidCityID(city) || rest == "" {
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
type GrantVerifier struct {
	byCity map[string]*citywriteauth.Verifier
}

// cityReplay hands a replay guard one city's grant IDs. A city ID holds no
// slash, so the city and the ID cannot run together.
type cityReplay struct {
	city  string
	guard citywriteauth.ReplayGuard
}

// Use records the city, a slash and the ID as one string.
func (r cityReplay) Use(jti string, exp time.Time) error {
	return r.guard.Use(r.city+"/"+jti, exp)
}

// NewGrantVerifier pins the relay audience, lifetime cap and 30-second skew.
// A grant's ID is single-use within its own city, so two cities may use the
// same ID. The replay guard receives the city, a slash and the ID as one
// string, such as "alpha/shared-id", and never the ID alone. A guard that
// stores IDs stores that string. A nil replay guard becomes one in-memory
// guard, which receives the same strings as a supplied guard.
func NewGrantVerifier(keys map[string]ed25519.PublicKey, replay citywriteauth.ReplayGuard) (*GrantVerifier, error) {
	if len(keys) == 0 {
		return nil, errors.New("relay grant verifier requires at least one city key")
	}
	if replay == nil {
		replay = citywriteauth.NewMemoryReplayGuard()
	}
	// Only the path city's key can authorize its request, so each city's
	// verifier trusts that one key. The city it adds to a replay ID is
	// therefore both the grant's signer and the city of the request path.
	byCity := make(map[string]*citywriteauth.Verifier, len(keys))
	for city, key := range keys {
		if !ValidCityID(city) {
			return nil, ErrAddress
		}
		v, err := citywriteauth.New(citywriteauth.Options{
			Keys:   map[string]ed25519.PublicKey{city: key},
			Aud:    GrantAudience,
			MaxTTL: maxGrantTTL,
			Skew:   30 * time.Second,
			Replay: cityReplay{city: city, guard: replay},
		})
		if err != nil {
			return nil, err
		}
		byCity[city] = v
	}
	return &GrantVerifier{byCity: byCity}, nil
}

// Verify checks the signed request and requires its authenticated signer to
// be the city in the request path.
func (v *GrantVerifier) Verify(token string, req Request) error {
	expect, err := req.expectation()
	if err != nil {
		return err
	}
	verifier := v.byCity[expect.City]
	if verifier == nil {
		return citywriteauth.ErrUnknownKey
	}
	g, err := verifier.Verify(token, expect)
	if errors.Is(err, citywriteauth.ErrUnknownKey) {
		return citywriteauth.ErrCityMismatch
	}
	if err != nil {
		return err
	}
	if g.Kid != g.City || g.City != expect.City {
		return citywriteauth.ErrCityMismatch
	}
	return nil
}
