package relay

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/mailrelay/envelope"
)

const (
	registryTimeout      = 10 * time.Second
	registryBodyLimit    = 1 << 20
	registryUserAgent    = "gc-mail-relay"
	registryMaxRedirects = 10
)

var (
	errRedirectAway     = errors.New("redirect away from the registry address")
	errTooManyRedirects = errors.New("too many redirects")
)

// copyError says why a copy of the registry's list is bad. Its words never
// carry the answer's content, so it may be logged.
type copyError struct {
	reason  string
	entries int // entries in the list, when the list was read
}

func (e *copyError) Error() string { return e.reason }

// registryCopy reads the key registry's list of city signing keys from one
// address. A read sends no cookies and no credentials, follows a redirect only
// on the address's own scheme and host, and reads at most 1 MiB in 10 seconds.
type registryCopy struct {
	address string
	client  *http.Client
}

// newRegistryCopy checks the registry address and builds its client. An empty
// address means no registry and returns nil. The address must be https, or
// http to a loopback host as gc's client contexts allow. transport carries
// every read; nil means http.DefaultTransport, as for any http.Client.
func newRegistryCopy(address string, transport http.RoundTripper) (*registryCopy, error) {
	if address == "" {
		return nil, nil
	}
	// The address is not repeated in a refusal: it could carry a secret.
	u, err := url.Parse(address)
	switch {
	case err != nil:
		return nil, errors.New("registry URL is not a valid URL")
	case u.Scheme != "https" && (u.Scheme != "http" || !isLoopbackHost(u.Hostname())):
		return nil, errors.New("registry URL must use https; http is allowed only for a loopback host")
	case u.Hostname() == "":
		return nil, errors.New("registry URL has no host")
	case u.User != nil:
		return nil, errors.New("registry URL must not carry a user name or password")
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   registryTimeout,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if next.URL.Scheme != u.Scheme || !strings.EqualFold(next.URL.Host, u.Host) || next.URL.User != nil {
				return errRedirectAway
			}
			if len(via) >= registryMaxRedirects {
				return errTooManyRedirects
			}
			return nil
		},
	}
	return &registryCopy{address: address, client: client}, nil
}

// isLoopbackHost is the rule gc's client contexts apply to plain http.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// read fetches and checks one whole copy of the list. Every failure is a
// *copyError.
func (r *registryCopy) read(ctx context.Context) (cityKeys, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.address, nil)
	if err != nil {
		return nil, &copyError{reason: "request failed"}
	}
	req.Header.Set("User-Agent", registryUserAgent)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, &copyError{reason: requestFailure(err)}
	}
	defer resp.Body.Close() //nolint:errcheck // Read-only response body.
	if resp.StatusCode != http.StatusOK {
		return nil, &copyError{reason: fmt.Sprintf("status %d", resp.StatusCode)}
	}
	if resp.ContentLength > registryBodyLimit {
		return nil, &copyError{reason: "body over 1 MiB"}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, registryBodyLimit+1))
	if err != nil {
		return nil, &copyError{reason: requestFailure(err)}
	}
	if len(body) > registryBodyLimit {
		return nil, &copyError{reason: "body over 1 MiB"}
	}
	return parseRegistryList(body)
}

// requestFailure names a transport failure without its text, which carries
// the address.
func requestFailure(err error) string {
	var timeout interface{ Timeout() bool }
	switch {
	case errors.Is(err, errRedirectAway):
		return errRedirectAway.Error()
	case errors.Is(err, errTooManyRedirects):
		return errTooManyRedirects.Error()
	case errors.As(err, &timeout) && timeout.Timeout():
		return "timeout"
	default:
		return "request failed"
	}
}

// registryList is the registry's answer: a JSON object with one list named
// cities. Fields the relay does not read are ignored.
type registryList struct {
	Cities []registryEntry `json:"cities"`
}

// registryEntry is one city's row. registered_at must be present as a
// nonempty string; the relay does not interpret it.
type registryEntry struct {
	City         string `json:"city"`
	PublicKey    string `json:"public_key"`
	RegisteredAt string `json:"registered_at"`
}

// parseRegistryList checks a copy whole: it is good only when every entry
// passes, and two entries with one city ID make it bad.
func parseRegistryList(body []byte) (cityKeys, error) {
	var answer registryList
	if err := json.Unmarshal(body, &answer); err != nil {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			return nil, &copyError{reason: "body is not JSON"}
		}
		return nil, &copyError{reason: "body does not have the list's shape"}
	}
	if answer.Cities == nil {
		return nil, &copyError{reason: "body has no cities list"}
	}
	entries := len(answer.Cities)
	keys := make(cityKeys, entries)
	for i, entry := range answer.Cities {
		bad := func(what string) error {
			return &copyError{reason: fmt.Sprintf("entry %d: %s", i+1, what), entries: entries}
		}
		// The base64 decoder skips line breaks, so the key must also be the
		// exact encoding of its bytes.
		key, err := base64.StdEncoding.Strict().DecodeString(entry.PublicKey)
		switch {
		case !envelope.ValidCityID(entry.City):
			return nil, bad("invalid city id")
		case keys[entry.City] != nil:
			return nil, bad("duplicate city id")
		case err != nil || len(key) != ed25519.PublicKeySize || base64.StdEncoding.EncodeToString(key) != entry.PublicKey:
			return nil, bad("public key is not standard base64 of 32 bytes")
		case strings.TrimSpace(entry.RegisteredAt) == "":
			return nil, bad("no registered_at")
		}
		if err := checkSigningKey(key); err != nil {
			return nil, bad("public key is " + err.Error())
		}
		keys[entry.City] = key
	}
	return keys, nil
}
