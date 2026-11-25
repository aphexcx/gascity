package envelope

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citywriteauth"
)

func TestGrant(t *testing.T) {
	key := signingKey(1)
	keys := map[string]ed25519.PublicKey{"alpha": key.Public().(ed25519.PublicKey), "beta": signingKey(2).Public().(ed25519.PublicKey)}
	req := Request{Method: "POST", Path: "/v1/city/alpha/mail", RawQuery: "page=1", Body: []byte("sealed")}
	token, err := MintGrant(key, "alpha", "alpha", req, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	expect := citywriteauth.Expect{City: "alpha", ReqDigest: citywriteauth.ReqDigest(req.Method, req.Path, req.RawQuery, req.Body)}
	var grant citywriteauth.Grant
	for _, aud := range []string{"gc-mail-relay.v1", "gc-city-write", "gc-city-write.v2", "gc-city-read"} {
		v, err := citywriteauth.New(citywriteauth.Options{Keys: keys, Aud: aud, MaxTTL: 2 * time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		got, err := v.Verify(token, expect)
		if aud == "gc-mail-relay.v1" {
			if err != nil || got.Exp-got.IAT != 120 {
				t.Fatalf("existing verifier: %v", err)
			}
			grant = *got
		} else if !errors.Is(err, citywriteauth.ErrAudience) {
			t.Fatalf("listener %s: %v", aud, err)
		}
	}
	replay := citywriteauth.NewMemoryReplayGuard()
	v, err := NewGrantVerifier(keys, replay)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Request){
		func(r *Request) { r.Method = "GET" }, func(r *Request) { r.Path += "box" },
		func(r *Request) { r.RawQuery = "page=2" }, func(r *Request) { r.Body = nil },
		func(r *Request) { r.Path = "/v1/city/beta/mail" },
	} {
		other := req
		change(&other)
		if err := v.Verify(token, other); err == nil {
			t.Fatal("accepted changed request")
		}
	}
	for _, aud := range []string{"gc-city-write", "gc-city-write.v2", "gc-city-read"} {
		other := grant
		other.Aud = aud
		if err := v.Verify(signGrantForTest(t, other, key), req); !errors.Is(err, citywriteauth.ErrAudience) {
			t.Fatalf("relay accepted audience %s: %v", aud, err)
		}
	}
	if err := v.Verify(token, req); err != nil {
		t.Fatal(err)
	}
	if err := v.Verify(token, req); !errors.Is(err, citywriteauth.ErrReplay) {
		t.Fatalf("replay = %v", err)
	}

	// A trusted signer can construct a grant claiming another city's path.
	other := req
	other.Path = "/v1/city/beta/mail"
	grant.City, grant.JTI = "beta", "another-use"
	grant.Req = citywriteauth.ReqDigest(other.Method, other.Path, other.RawQuery, other.Body)
	if err := v.Verify(signGrantForTest(t, grant, key), other); !errors.Is(err, citywriteauth.ErrCityMismatch) {
		t.Fatalf("kid and city differ: %v", err)
	}
	if err := replay.Use(grant.JTI, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("rejected identity consumed a replay ID: %v", err)
	}
	for _, ttl := range []time.Duration{0, -time.Second, 121 * time.Second} {
		if _, err := MintGrant(key, "alpha", "alpha", req, ttl); err == nil {
			t.Fatalf("accepted TTL %v", ttl)
		}
	}
	second, err := MintGrant(key, "alpha", "alpha", req, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Verify(second, req); err != nil {
		t.Fatalf("fresh JTI: %v", err)
	}
}

func signGrantForTest(t *testing.T, g citywriteauth.Grant, key ed25519.PrivateKey) string {
	t.Helper()
	data, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(data) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, data))
}

func TestGrantRequestSyntax(t *testing.T) {
	key := signingKey(1)
	v, err := NewGrantVerifier(map[string]ed25519.PublicKey{"alpha": key.Public().(ed25519.PublicKey)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []Request{
		{Method: "BAD METHOD", Path: "/v1/city/alpha/mail"},
		{Method: "GET:", Path: "/v1/city/alpha/mail"},
		{Method: "GET", Path: "/v1/city/alpha/mail\n"},
		{Method: "GET", Path: "/v1/city/alpha/mail\x00"},
		{Method: "GET", Path: "/v1/city/alpha/mail", RawQuery: "tag=\r"},
		{Method: "GET", Path: "/v1/city/alpha/mail", RawQuery: "\x7f"},
	} {
		if _, err := MintGrant(key, "alpha", "alpha", req, time.Minute); !errors.Is(err, ErrFormat) {
			t.Errorf("MintGrant(%+q) = %v, want ErrFormat", req, err)
		}
		if err := v.Verify("", req); !errors.Is(err, ErrFormat) {
			t.Errorf("Verify(%+q) = %v, want ErrFormat", req, err)
		}
	}
}
