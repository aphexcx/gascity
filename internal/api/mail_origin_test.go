package api

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// listedOriginState records the key id the mail handler hands to the
// recipient-list policy.
type listedOriginState struct {
	*fakeState
	kids []string
}

func (s *listedOriginState) NotifyListedMail(_ mail.Message, originKid string) (listed, notified bool) {
	s.kids = append(s.kids, originKid)
	return true, true
}

// The list policy learns who sent a request from the key the write gate
// verified. Nothing the request writes about itself can set or change it.
func TestMailSendHandsVerifiedKidToListPolicy(t *testing.T) {
	now := time.Now()
	directoryPub, directoryPriv := mustKeypair(t)
	inlinePub, inlinePriv := mustKeypair(t)
	_, strangerPriv := mustKeypair(t)
	keys := t.TempDir()
	writeTrustedKey(t, keys, "alpha", directoryPub)

	state := &listedOriginState{fakeState: newFakeState(t)}
	path := cityURL(state, "/mail")
	city := state.CityName()
	newMux := func() *SupervisorMux {
		return NewSupervisorMux(&stateCityResolver{state: state}, nil, false, "test", "", now).WithAnyHostAllowed()
	}
	open := newMux().Handler()
	primary := newMux().WithWriteAuth(newTestWriteVerifier(t, inlinePub, now)).Handler()
	hardened, err := NewHardenedAuth(newMux(), supervisor.Section{Hardened: supervisor.HardenedListener{Port: 8447, TrustedKeysDir: keys}}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}

	type signer struct {
		priv ed25519.PrivateKey
		kid  string
	}
	for _, tc := range []struct {
		name    string
		handler http.Handler
		sign    *signer
		body    string
		headers map[string]string
		status  int
		want    []string
	}{
		{
			name: "directory key on the hardened listener", handler: hardened, sign: &signer{directoryPriv, "alpha"},
			body: `{"from":"alpha/mayor","to":"worker","subject":"update","notify":true}`, status: 201, want: []string{"alpha"},
		},
		{
			name: "inline key on the primary listener", handler: primary, sign: &signer{inlinePriv, "k1"},
			body: `{"from":"alpha/mayor","to":"worker","subject":"update","notify":true}`, status: 201, want: []string{"k1"},
		},
		{
			name: "no key on an ungated listener", handler: open,
			body: `{"from":"alpha/mayor","to":"worker","subject":"update","notify":true}`, status: 201, want: []string{""},
		},
		{
			name: "written sender and headers claim another origin", handler: hardened, sign: &signer{directoryPriv, "alpha"},
			body:    `{"from":"city:beta","to":"worker","subject":"update","notify":true}`,
			headers: map[string]string{"X-GC-Origin": "beta", "X-GC-Origin-Kid": "beta", "X-GC-Kid": "beta"},
			status:  201, want: []string{"alpha"},
		},
		{
			name: "unverified grant on an ungated listener", handler: open, sign: &signer{strangerPriv, "alpha"},
			body:    `{"from":"city:alpha","to":"worker","subject":"update","notify":true}`,
			headers: map[string]string{"X-GC-Origin": "alpha", "X-GC-Origin-Kid": "alpha", "X-GC-Kid": "alpha"},
			status:  201, want: []string{""},
		},
		{
			name: "body field claiming an origin is refused before the policy", handler: hardened, sign: &signer{directoryPriv, "alpha"},
			body: `{"from":"alpha/mayor","to":"worker","subject":"update","notify":true,"origin_kid":"beta"}`, status: 422,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state.kids = nil
			r := httptest.NewRequest("POST", path, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set(csrfHeaderName, "true")
			for name, value := range tc.headers {
				r.Header.Set(name, value)
			}
			if tc.sign != nil {
				g := grantFor(now, city, "POST", r.URL.Path, []byte(tc.body), "jti-"+tc.name)
				g.Kid = tc.sign.kid
				r.Header.Set(writeAuthHeader, mintToken(t, tc.sign.priv, g))
			}
			w := httptest.NewRecorder()
			tc.handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if !slices.Equal(state.kids, tc.want) {
				t.Fatalf("list policy received key ids %q, want %q", state.kids, tc.want)
			}
		})
	}
}
