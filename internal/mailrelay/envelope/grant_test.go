package envelope

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
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
	replay := &recordingReplay{next: citywriteauth.NewMemoryReplayGuard()}
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
	used := len(replay.ids)
	if err := v.Verify(signGrantForTest(t, grant, key), other); !errors.Is(err, citywriteauth.ErrCityMismatch) {
		t.Fatalf("kid and city differ: %v", err)
	}
	if len(replay.ids) != used {
		t.Fatalf("rejected identity reached the replay guard: %q", replay.ids[used:])
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

// recordingReplay records every ID a verifier hands its replay guard.
type recordingReplay struct {
	next citywriteauth.ReplayGuard
	ids  []string
}

func (r *recordingReplay) Use(id string, exp time.Time) error {
	r.ids = append(r.ids, id)
	return r.next.Use(id, exp)
}

func TestGrantReplayIsPerCity(t *testing.T) {
	signers := map[string]ed25519.PrivateKey{"alpha": signingKey(1), "beta": signingKey(2)}
	keys := map[string]ed25519.PublicKey{}
	for city, key := range signers {
		keys[city] = key.Public().(ed25519.PublicKey)
	}
	cities := []string{"alpha", "beta"}
	now := time.Now().Unix()
	for _, supplied := range []bool{false, true} {
		name := "default guard"
		var guard citywriteauth.ReplayGuard
		var record *recordingReplay
		if supplied {
			name = "supplied guard"
			record = &recordingReplay{next: citywriteauth.NewMemoryReplayGuard()}
			guard = record
		}
		t.Run(name, func(t *testing.T) {
			v, err := NewGrantVerifier(keys, guard)
			if err != nil {
				t.Fatal(err)
			}
			// Each city signs, with its own key, a grant for its own path. The
			// two grants carry the same ID.
			tokens, reqs := map[string]string{}, map[string]Request{}
			for _, city := range cities {
				req := Request{Method: "POST", Path: "/v1/city/" + city + "/mail", Body: []byte("sealed")}
				grant := citywriteauth.Grant{Kid: city, Aud: GrantAudience, City: city, IAT: now, Exp: now + 60, JTI: "shared-id"}
				grant.Req = citywriteauth.ReqDigest(req.Method, req.Path, req.RawQuery, req.Body)
				tokens[city], reqs[city] = signGrantForTest(t, grant, signers[city]), req
			}
			for _, city := range cities {
				if err := v.Verify(tokens[city], reqs[city]); err != nil {
					t.Errorf("%s: first use of its own grant = %v, want accepted", city, err)
				}
			}
			for _, city := range cities {
				if err := v.Verify(tokens[city], reqs[city]); !errors.Is(err, citywriteauth.ErrReplay) {
					t.Errorf("%s: second use of its own grant = %v, want ErrReplay", city, err)
				}
			}
			if record == nil {
				return
			}
			want := []string{"alpha/shared-id", "beta/shared-id", "alpha/shared-id", "beta/shared-id"}
			if !slices.Equal(record.ids, want) {
				t.Errorf("replay IDs given to the supplied guard = %q, want %q", record.ids, want)
			}
		})
	}
	if _, err := NewGrantVerifier(nil, nil); err == nil {
		t.Error("accepted an empty key set")
	}
}

func TestGrantBindsEscapedPath(t *testing.T) {
	key := signingKey(1)
	v, err := NewGrantVerifier(map[string]ed25519.PublicKey{"alpha": key.Public().(ed25519.PublicKey)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fromURL := RequestFromURL
	parse := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	body := []byte("sealed")

	// Two escaped paths that decode alike are two routes, so two bindings.
	for _, pair := range [][2]string{
		{"/v1/city/alpha/mail/a%2Fb", "/v1/city/alpha/mail/a/b"},
		{"/v1/city/alpha/mail/%61", "/v1/city/alpha/mail/a"},
	} {
		first := fromURL("POST", parse(pair[0]+"?page=1"), body)
		second := fromURL("POST", parse(pair[1]+"?page=1"), body)
		firstExpect, err := first.expectation()
		if err != nil {
			t.Fatal(err)
		}
		secondExpect, err := second.expectation()
		if err != nil {
			t.Fatal(err)
		}
		if firstExpect.ReqDigest == secondExpect.ReqDigest {
			t.Errorf("%s and %s share one binding", pair[0], pair[1])
		}
		token, err := MintGrant(key, "alpha", "alpha", first, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.Verify(token, second); !errors.Is(err, citywriteauth.ErrReqMismatch) {
			t.Errorf("a grant for %s on the route %s = %v, want ErrReqMismatch", pair[0], pair[1], err)
		}
	}

	// The city is read from the same escaped form.
	if _, err := MintGrant(key, "alpha", "alpha", fromURL("POST", parse("/v1/city/al%70ha/mail"), body), time.Minute); !errors.Is(err, ErrAddress) {
		t.Errorf("a grant for an escaped city segment = %v, want ErrAddress", err)
	}

	// The minting side and the verifying side derive the same request: what
	// Go writes on the wire for one is what Go reads back for the other.
	out, err := http.NewRequest("POST", "https://relay.example/v1/city/alpha/mail/a%2Fb?page=1", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	if err := out.Write(&wire); err != nil {
		t.Fatal(err)
	}
	in, err := http.ReadRequest(bufio.NewReader(&wire))
	if err != nil {
		t.Fatal(err)
	}
	minted, served := fromURL(out.Method, out.URL, body), fromURL(in.Method, in.URL, body)
	if minted.Path != "/v1/city/alpha/mail/a%2Fb" || served.Path != minted.Path || served.RawQuery != minted.RawQuery {
		t.Errorf("minted %q?%q, served %q?%q, want the escaped path and page=1 on both sides", minted.Path, minted.RawQuery, served.Path, served.RawQuery)
	}
	fresh, err := MintGrant(key, "alpha", "alpha", minted, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Verify(fresh, served); err != nil {
		t.Errorf("the verifying side refused the minting side's own grant: %v", err)
	}

	// A nil URL is refused, never a panic.
	if _, err := MintGrant(key, "alpha", "alpha", RequestFromURL("POST", nil, body), time.Minute); !errors.Is(err, ErrAddress) {
		t.Errorf("a grant for a nil URL = %v, want ErrAddress", err)
	}
}
