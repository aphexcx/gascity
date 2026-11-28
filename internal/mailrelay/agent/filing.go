package agent

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/mailrelay/envelope"
)

// apiTimeout bounds one filing request to the city's own API.
const apiTimeout = 30 * time.Second

// problemIdempotencyMismatch is the city API's code for a key reused with
// another request body.
const problemIdempotencyMismatch = "idempotency-mismatch"

// detailRecipientNotFound starts the city API's 400 detail when the address
// names no recipient: the words of the session lookup the route answers with.
const detailRecipientNotFound = "session not found"

// What one filing came to.
type filingKind int

const (
	// filingRetry: not filed; the place stays and the mail is tried again.
	filingRetry filingKind = iota
	// filingFiled: the city holds the mail.
	filingFiled
	// filingMismatch: the sender city reused an id with other content; the
	// first mail stands and this one is passed.
	filingMismatch
	// filingRefused: the city says the fault is in the mail itself, so no
	// later try can file it; the mail is passed over.
	filingRefused
)

type filingResult struct {
	kind   filingKind
	id     string // the city's message id, when filed
	to     string // the recipient as the city resolved it, when filed
	detail string // why it was not filed, for the log
}

// cityMail files mail through the city's own mail API.
type cityMail struct {
	client *genclient.ClientWithResponses
	city   string
}

func newCityMail(s Settings, transport http.RoundTripper) (*cityMail, error) {
	client, err := genclient.NewClientWithResponses(s.APIURL, genclient.WithHTTPClient(&http.Client{
		Transport: transport, Timeout: apiTimeout, CheckRedirect: noRedirects,
	}))
	if err != nil {
		return nil, fmt.Errorf("setting %s: %w", SettingAPIURL, err)
	}
	return &cityMail{client: client, city: s.GCCity}, nil
}

// file stores one mail under its filing key. The sender is the sender city,
// a slash and the mail's own sender. There is no filing without the key.
func (c *cityMail) file(ctx context.Context, key, senderCity string, m envelope.Mail) filingResult {
	from := senderCity + "/" + m.From
	body := genclient.SendMailJSONRequestBody{From: &from, To: m.To, Subject: m.Subject, Body: &m.Body}
	raw, err := c.client.SendMail(ctx, c.city, &genclient.SendMailParams{XGCRequest: "true", IdempotencyKey: &key}, body)
	if err != nil {
		return filingResult{kind: filingRetry, detail: "reason=" + transportReason(ctx, err)}
	}
	code := raw.StatusCode
	// The status decides; an answer body that does not parse only loses the
	// detail, so a filing answered 201 is a filing.
	resp, err := genclient.ParseSendMailResponse(raw)
	if err != nil {
		resp = &genclient.SendMailResponse{HTTPResponse: raw}
	}
	status := "status=" + strconv.Itoa(code)
	switch {
	case code == http.StatusCreated:
		r := filingResult{kind: filingFiled}
		if resp.JSON201 != nil {
			r.id, r.to = resp.JSON201.Id, resp.JSON201.To
		}
		return r
	case code == http.StatusUnprocessableEntity && problemCode(resp.ApplicationproblemJSON422) == problemIdempotencyMismatch:
		return filingResult{kind: filingMismatch, detail: status}
	case mailFault(code, resp):
		return filingResult{kind: filingRefused, detail: status + problemSuffix(resp)}
	}
	return filingResult{kind: filingRetry, detail: status + problemSuffix(resp)}
}

// mailFault reports whether the city's answer puts the fault in the mail
// itself: too large, malformed, or addressed to no recipient. Every other
// answer, a refusal included, can pass: the city's store was down, the
// address named two sessions for a moment, or the agent is not let in yet.
// That mail stays at the relay.
func mailFault(code int, resp *genclient.SendMailResponse) bool {
	switch code {
	case http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	case http.StatusBadRequest:
		return strings.HasPrefix(problemDetail(resp.ApplicationproblemJSON400), detailRecipientNotFound)
	}
	return false
}

func problemCode(p *genclient.ErrorModel) string {
	if p == nil || p.Code == nil {
		return ""
	}
	return *p.Code
}

func problemDetail(p *genclient.ErrorModel) string {
	if p == nil || p.Detail == nil {
		return ""
	}
	return *p.Detail
}

// problemSuffix names the city's problem code, never its detail, which can
// quote the request.
func problemSuffix(resp *genclient.SendMailResponse) string {
	for _, p := range []*genclient.ErrorModel{resp.ApplicationproblemJSON400, resp.ApplicationproblemJSON422} {
		if c := problemCode(p); c != "" {
			return " problem=" + c
		}
	}
	return ""
}
