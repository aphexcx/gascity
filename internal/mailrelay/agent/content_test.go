package agent

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/mailrelay/envelope"
)

// unfitMail lists mail no city should file: a subject the city's store can
// never take (blank, starting with a dash, over 500 bytes), and a sender name
// that holds a control character.
func unfitMail() []struct {
	name string
	edit func(m *envelope.Mail)
	rule string
} {
	sender := func(from string) func(m *envelope.Mail) { return func(m *envelope.Mail) { m.From = from } }
	subject := func(s string) func(m *envelope.Mail) { return func(m *envelope.Mail) { m.Subject = s } }
	return []struct {
		name string
		edit func(m *envelope.Mail)
		rule string
	}{
		{"empty subject", subject(""), "blank"},
		{"blank subject", subject(" \t "), "blank"},
		{"subject starts with a dash", subject("-1 from me"), "dash"},
		{"subject is an option", subject("--help"), "dash"},
		{"subject over 500 bytes", subject(strings.Repeat("s", 501)), "500 bytes"},
		{"subject over 500 bytes in fewer characters", subject(strings.Repeat("é", 251)), "500 bytes"},
		{"sender name with an escape", sender("writer\x1b[2J"), "control character"},
		{"sender name with a newline", sender("writer\nfiled"), "control character"},
		{"sender name with DEL", sender("writer\x7f"), "control character"},
		{"sender name with a C1 control", sender("writer\u009b2J"), "control character"},
		{"sender name with a line separator", sender("writer\u2028x"), "control character"},
		{"sender name with a paragraph separator", sender("writer\u2029x"), "control character"},
	}
}

// The receiver passes unfit mail over as malformed at once, with one log line
// and nothing sent to the city's API, and files the good mail behind it in
// the same batch. The API here answers 500 to the unfit mail, as a city whose
// store refuses it does.
func TestUnfitMailIsPassedOverAtOnce(t *testing.T) {
	for _, tc := range unfitMail() {
		t.Run(tc.name, func(t *testing.T) {
			hub, alpha := pair(t)
			rel := newScriptedRelay()
			clock := newTestClock()
			city := newCityAPI(t, "alpha", "reader")
			bad := filingKey("hub", "bad-1")
			posts := newRecorder()
			apiTransport := posts.wrap(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Idempotency-Key") == bad {
					return problem(r, http.StatusInternalServerError, "internal"), nil
				}
				return city.transport().RoundTrip(r)
			}))
			m := mailTo("reader")
			tc.edit(&m)
			rel.add(1, seal(t, hub, alpha, "bad-1", clock.Now(), m))
			rel.add(2, seal(t, hub, alpha, "good-2", clock.Now(), mailTo("reader")))
			run := start(t, alpha.settings, clock.use(alpha.deps(rel.transport(), apiTransport)))
			rel.recorder.await(t, "the good mail, or an hour", func() bool {
				return len(city.inbox("reader")) == 1 || clock.elapsed() >= time.Hour
			})
			rel.recorder.await(t, "the next read", func() bool { return len(rel.recorder.reads()) >= 2 })
			run.stop(t)
			for _, p := range posts.matching("POST", "/mail") {
				if p.header.Get("Idempotency-Key") == bad {
					t.Fatal("the unfit mail reached the city's API")
				}
			}
			if n := len(city.inbox("reader")); n != 1 {
				t.Fatalf("inbox holds %d mails after %s", n, clock.elapsed())
			}
			if reads := rel.recorder.reads(); reads[1] != 2 {
				t.Fatalf("the good mail was not filed in the first batch: reads %v", reads)
			}
			var lines []string
			for _, l := range strings.Split(alpha.log.String(), "\n") {
				if strings.Contains(l, `id="bad-1"`) {
					lines = append(lines, l)
				}
			}
			if len(lines) != 1 || countDrops(alpha.log, "bad-1", "malformed") != 1 {
				t.Fatalf("the unfit mail logged %q, want one drop line", lines)
			}
		})
	}
}

// A subject of exactly 500 bytes, and one that starts with a space before a
// dash, are filed: the store takes both.
func TestSubjectAtTheLimitIsFiled(t *testing.T) {
	for _, s := range []string{strings.Repeat("s", 500), " -1 from me"} {
		hub, alpha := pair(t)
		rel := newScriptedRelay()
		city := newCityAPI(t, "alpha", "reader")
		m := mailTo("reader")
		m.Subject = s
		rel.add(1, seal(t, hub, alpha, "m-1", time.Now(), m))
		run := start(t, alpha.settings, alpha.deps(rel.transport(), city.transport()))
		city.recorder.await(t, "the filing", func() bool { return len(city.inbox("reader")) == 1 })
		run.stop(t)
		if got := city.inbox("reader")[0].Subject; got != s {
			t.Fatalf("filed subject %q, want %q", got, s)
		}
	}
}

// send refuses unfit mail before anything is fetched, signed or posted, and
// names the rule it broke.
func TestSendRefusesUnfitMail(t *testing.T) {
	for _, tc := range unfitMail() {
		t.Run(tc.name, func(t *testing.T) {
			hub, _, rel := sendPair(t)
			m := envelope.Mail{From: "writer", Subject: subjectSentinel, Body: bodySentinel}
			tc.edit(&m)
			before := len(rel.recorder.all())
			_, err := Send(context.Background(), hub.settings, hub.deps(rel.transport, nil), Outgoing{To: "alpha/reader", From: m.From, Subject: m.Subject, Body: m.Body})
			if err == nil || !strings.Contains(err.Error(), tc.rule) {
				t.Fatalf("send: %v, want an error naming %q", err, tc.rule)
			}
			if asked := rel.recorder.all()[before:]; len(asked) != 0 {
				t.Fatalf("send asked the relay %d times", len(asked))
			}
			if strings.ContainsAny(err.Error(), "\x1b\u009b\u2028\u2029") {
				t.Fatalf("the error carries the sender's control text: %q", err.Error())
			}
		})
	}
}
