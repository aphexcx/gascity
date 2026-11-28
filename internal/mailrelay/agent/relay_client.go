package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/mailrelay/envelope"
	"github.com/gastownhall/gascity/internal/mailrelay/relay"
)

const (
	grantHeader = "X-GC-Relay-Grant"
	// grantTTL covers one request; each request gets a fresh grant.
	grantTTL = time.Minute
	// maxBatchBytes bounds a read's answer: the relay sends at most two
	// envelopes' worth of bytes, base64 in JSON.
	maxBatchBytes      = 3*envelope.MaxEnvelopeSize + 64<<10
	maxStatementBytes  = 4096
	maxRefusalBodySize = 4096
)

// The ways one relay read can end.
const (
	outcomeMail    = "mail"
	outcomeEmpty   = "empty"
	outcomeBusy    = "busy"
	outcomeCut     = "cut"
	outcomeError   = "error"
	outcomeStopped = "stopped"
)

// askOutcome is how one read ended. Status is the HTTP status, if one came.
type askOutcome struct {
	kind   string
	reason string
	status int
	count  int
}

// relayClient makes the city's signed requests to the relay.
type relayClient struct {
	base   url.URL
	city   string
	signer ed25519.PrivateKey
	http   *http.Client
	limit  time.Duration
}

func newRelayClient(s Settings, signer ed25519.PrivateKey, transport http.RoundTripper) (*relayClient, error) {
	u, err := url.Parse(s.RelayURL)
	if err != nil {
		return nil, fmt.Errorf("setting %s is not a valid address", SettingRelayURL)
	}
	return &relayClient{
		base:   url.URL{Scheme: u.Scheme, Host: u.Host},
		city:   s.City,
		signer: signer,
		http:   &http.Client{Transport: transport, CheckRedirect: noRedirects},
		limit:  s.RequestLimit,
	}, nil
}

// noRedirects keeps every request at the address it was signed for.
func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func (c *relayClient) path(route string) string { return "/v1/city/" + c.city + "/" + route }

// do sends one request with a fresh grant bound to its method, path, query
// and body, within the request limit.
func (c *relayClient) do(ctx context.Context, method, route, query string, body []byte) (*http.Response, error) {
	u := c.base
	u.Path, u.RawQuery = c.path(route), query
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	token, err := envelope.MintGrant(c.signer, c.city, c.city, envelope.RequestFromURL(method, req.URL, body), grantTTL)
	if err != nil {
		return nil, fmt.Errorf("minting relay grant: %w", err)
	}
	req.Header.Set(grantHeader, token)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	return c.http.Do(req)
}

// call sends one request and returns its status and a bounded body.
func (c *relayClient) call(ctx context.Context, method, route string, body []byte, maxBody int64) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.limit)
	defer cancel()
	resp, err := c.do(ctx, method, route, "", body)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // The body is read in full below.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return 0, nil, err
	}
	if int64(len(data)) > maxBody {
		return resp.StatusCode, nil, fmt.Errorf("relay answer exceeds %d bytes", maxBody)
	}
	return resp.StatusCode, data, nil
}

func (c *relayClient) publish(ctx context.Context, statement []byte) (int, error) {
	code, _, err := c.call(ctx, http.MethodPut, "key", statement, maxRefusalBodySize)
	return code, err
}

func (c *relayClient) peerKey(ctx context.Context, peer string) (int, []byte, error) {
	return c.call(ctx, http.MethodGet, "keys/"+peer, nil, maxStatementBytes)
}

func (c *relayClient) post(ctx context.Context, wire []byte) (int, error) {
	code, _, err := c.call(ctx, http.MethodPost, "envelopes", wire, maxRefusalBodySize)
	return code, err
}

// read is the held-open read: it acknowledges everything at or below after
// and waits up to wait for more. The answer must be in queue order, above
// after.
func (c *relayClient) read(ctx context.Context, after int64, wait time.Duration) ([]relay.Delivery, askOutcome) {
	reqCtx, cancel := context.WithTimeout(ctx, c.limit)
	defer cancel()
	query := url.Values{"after": {strconv.FormatInt(after, 10)}, "wait": {wait.String()}}.Encode()
	resp, err := c.do(reqCtx, http.MethodGet, "queue", query, nil)
	if err != nil {
		return nil, transportOutcome(ctx, reqCtx, err)
	}
	defer resp.Body.Close() //nolint:errcheck // The body is read in full below.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBatchBytes+1))
	if err != nil {
		return nil, transportOutcome(ctx, reqCtx, err)
	}
	switch {
	case resp.StatusCode == http.StatusConflict:
		return nil, askOutcome{kind: outcomeBusy, status: resp.StatusCode}
	case resp.StatusCode != http.StatusOK:
		return nil, askOutcome{kind: outcomeError, status: resp.StatusCode}
	case len(data) > maxBatchBytes:
		return nil, askOutcome{kind: outcomeError, status: resp.StatusCode, reason: "too-large"}
	}
	var batch []relay.Delivery
	if err := json.Unmarshal(data, &batch); err != nil {
		return nil, askOutcome{kind: outcomeError, status: resp.StatusCode, reason: "malformed"}
	}
	last := after
	for _, d := range batch {
		if d.Number <= last {
			return nil, askOutcome{kind: outcomeError, status: resp.StatusCode, reason: "out-of-order"}
		}
		last = d.Number
	}
	if len(batch) == 0 {
		return nil, askOutcome{kind: outcomeEmpty, status: resp.StatusCode}
	}
	return batch, askOutcome{kind: outcomeMail, status: resp.StatusCode, count: len(batch)}
}

// transportOutcome names a read that failed below HTTP. A read cut by the
// agent's own limit or by the connection is "cut"; a relay that cannot be
// reached is an error.
func transportOutcome(parent, req context.Context, err error) askOutcome {
	var op *net.OpError
	switch {
	case parent.Err() != nil:
		return askOutcome{kind: outcomeStopped}
	case req.Err() != nil:
		return askOutcome{kind: outcomeCut, reason: "limit"}
	case errors.As(err, &op) && op.Op == "dial":
		return askOutcome{kind: outcomeError, reason: "unreachable"}
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return askOutcome{kind: outcomeCut, reason: "eof"}
	case errors.Is(err, syscall.ECONNRESET):
		return askOutcome{kind: outcomeCut, reason: "reset"}
	}
	return askOutcome{kind: outcomeCut, reason: "transport"}
}

// transportReason names a failed request in a log line without its text.
func transportReason(ctx context.Context, err error) string {
	var op *net.OpError
	switch {
	case ctx.Err() != nil:
		return "stopped"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &op) && op.Op == "dial":
		return "unreachable"
	}
	return "transport"
}
