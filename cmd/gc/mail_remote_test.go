package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clientcontext"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/supervisor"
)

const remoteMailMessageJSON = `{"id":"mc-wisp-1","from":"alpha/mayor","to":"mayor","subject":"hello","body":"round trip","created_at":"2026-08-18T17:00:00Z","read":false}`

// clearRemoteMailIdentityEnv strips the identity env so tests control the
// default remote sender deterministically.
func clearRemoteMailIdentityEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"GC_ALIAS", "GC_AGENT", "GC_SESSION_ID", "GC_CITY", "GC_CITY_PATH", "GC_CITY_ROOT", "GC_RIG", "GC_DIR"} {
		t.Setenv(k, "")
	}
}

// seedLocalCity creates a minimal local city named "alpha" and points the
// explicit city env at it, so the remote sender resolves "<local city>/…".
func seedLocalCity(t *testing.T) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "alpha-home")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"alpha\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", dir)
}

// The default remote sender is city-qualified ("<local city>/<identity>") with
// alias > agent > session id, and --from wins verbatim.
func TestRemoteMailSender(t *testing.T) {
	clearRemoteMailIdentityEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	if got := remoteMailSender("citadel/mayor"); got != "citadel/mayor" {
		t.Errorf("--from must pass verbatim, got %q", got)
	}
	// Chdir to a non-city dir so cwd discovery cannot find a real city.
	t.Chdir(t.TempDir())
	if got := remoteMailSender(""); got != "human" {
		t.Errorf("no city, no identity: want human, got %q", got)
	}
	t.Setenv("GC_SESSION_ID", "s-1")
	if got := remoteMailSender(""); got != "s-1" {
		t.Errorf("session id fallback: got %q", got)
	}
	t.Setenv("GC_AGENT", "rig/worker")
	if got := remoteMailSender(""); got != "rig/worker" {
		t.Errorf("agent beats session id: got %q", got)
	}
	t.Setenv("GC_ALIAS", "mayor")
	if got := remoteMailSender(""); got != "mayor" {
		t.Errorf("alias beats agent: got %q", got)
	}
	seedLocalCity(t)
	if got := remoteMailSender(""); got != "alpha/mayor" {
		t.Errorf("city-qualified sender: got %q", got)
	}
}

func TestCmdMail_ContextDispatchesRemote(t *testing.T) {
	for _, provider := range []string{"file", "exec"} {
		t.Run(provider, func(t *testing.T) { testRemoteMailRoundTrip(t, provider) })
	}
}

func testRemoteMailRoundTrip(t *testing.T, eventProvider string) {
	t.Helper()
	clearRemoteMailIdentityEnv(t)
	t.Setenv("GC_NO_API", "")
	t.Setenv("GC_MAIL", "")
	origin := newCharCity(t, charCityBasic, nil)
	seedLocalCity(t)
	cityPath := os.Getenv("GC_CITY")
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	receiver := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "alpha", cityPath)
	cities := http.NewServeMux()
	for _, state := range []*controllerState{origin.cs, receiver} {
		mux := api.NewSupervisorMux(&singleCityStateResolver{state: state}, nil, false, "test", "", time.Now())
		cities.Handle("/v0/city/"+state.cityName+"/", mux.WithAnyHostAllowed().Handler())
	}
	var hits []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Method+" "+r.URL.Path)
		cities.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	var out, errOut bytes.Buffer
	if rc := doContextAdd(clientcontext.Context{Name: "peer", City: "alpha", URL: server.URL}, &out, &errOut); rc != 0 {
		t.Fatal(errOut.String())
	}
	prev := contextFlag
	t.Cleanup(func() { contextFlag = prev })
	contextFlag = "peer"
	t.Setenv("GC_CITY", origin.cityPath)
	from := remoteMailSender("")
	if rc := cmdMailSendJSON([]string{"human", "Any news?"}, false, false, "", "", "Question", "", "", false, &out, &errOut); rc != 0 {
		t.Fatal(errOut.String())
	}
	local := receiver.MailProvider("")
	incoming, err := local.Inbox("human")
	if err != nil || len(incoming) != 1 || incoming[0].From != from {
		t.Fatalf("receiving city inbox=%+v err=%v", incoming, err)
	}
	original := incoming[0]
	if rc := cmdMailInboxWithJSON([]string{"human"}, false, &out, &errOut); rc != 0 {
		t.Fatal(errOut.String())
	}
	contextFlag = ""
	// The message belongs to the explicitly selected city. GC_CITY still
	// points at the origin city that issued the first send.
	prevCity := cityFlag
	t.Cleanup(func() { cityFlag = prevCity })
	cityFlag = cityPath
	eventPath := filepath.Join(cityPath, ".gc", "events.jsonl")
	supervisor, err := events.NewFileRecorder(eventPath, io.Discard, events.WithMaxSize(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = supervisor.Close() })
	supervisor.Record(events.Event{Type: events.MailSent, Subject: "before"})
	activeFile, err := os.Stat(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_EVENTS_ROTATION_ENABLED", "true")
	t.Setenv("GC_EVENTS_ROTATION_MAX_SIZE_BYTES", "1")
	replyEventPath := eventPath
	if eventProvider == "exec" {
		dir := t.TempDir()
		script := filepath.Join(dir, "events-provider")
		replyEventPath = filepath.Join(dir, "records.jsonl")
		if err := os.WriteFile(script, []byte(`#!/bin/sh
if [ "$1" = record ]; then
  cat >> "${0%/*}/records.jsonl"
  printf '\n' >> "${0%/*}/records.jsonl"
fi
`), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GC_EVENTS", "exec:"+script)
	} else {
		t.Setenv("GC_EVENTS", "file")
	}
	replyEvents := func() []events.Event {
		t.Helper()
		all, err := events.ReadAll(replyEventPath)
		if err != nil {
			t.Fatal(err)
		}
		var replies []events.Event
		for _, event := range all {
			if event.Type == events.MailReplied {
				replies = append(replies, event)
			}
		}
		return replies
	}
	for _, jsonOut := range []bool{false, true} {
		out.Reset()
		errOut.Reset()
		rc := cmdMailReplyJSON([]string{original.ID, "An answer"}, "", "", true, jsonOut, &out, &errOut)
		duds, err := local.Inbox(from)
		if err != nil {
			t.Fatal(err)
		}
		if rc == 0 || len(duds) != 0 || !strings.Contains(out.String()+errOut.String(), "gc --context "+charCityName+" mail send human") {
			t.Errorf("missing context (json=%t): exit=%d local replies=%d stdout=%q stderr=%q", jsonOut, rc, len(duds), &out, &errOut)
		}
	}
	if rc := doContextAdd(clientcontext.Context{Name: "return-route", City: charCityName, URL: server.URL}, &out, &errOut); rc != 0 {
		t.Fatal(errOut.String())
	}
	if got := replyEvents(); len(got) != 0 {
		t.Errorf("refused replies recorded events: %+v", got)
	}
	var previousID string
	for i, jsonOut := range []bool{false, true} {
		out.Reset()
		errOut.Reset()
		rc := cmdMailReplyJSON([]string{original.ID, "An answer"}, "", "", false, jsonOut, &out, &errOut)
		if rc != 0 {
			t.Fatalf("configured context: exit=%d stderr=%q", rc, &errOut)
		}
		if jsonOut {
			var result mailActionResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Command != "mail.reply" || result.Message == nil || result.Message.To != "human" {
				t.Errorf("reply JSON: %s (%v)", &out, err)
			}
		} else if !strings.Contains(out.String(), charCityName) {
			t.Errorf("confirmation missing destination city: %q", &out)
		}
		inbox, err := origin.cs.MailProvider("").Inbox("human")
		if err != nil || len(inbox) != i+1 {
			t.Errorf("origin inbox: got %d deliveries, want %d (err=%v)", len(inbox), i+1, err)
		} else {
			m := inbox[0]
			if i == 1 && m.ID == previousID {
				m = inbox[1]
			}
			previousID = m.ID
			if m.From != "alpha/human" || m.Subject != "Re: Question" || m.Body != "An answer" {
				t.Errorf("delivered reply: %+v", m)
			}
			replies := replyEvents()
			if len(replies) != i+1 {
				t.Errorf("after reply %d, got %d mail.replied events", i+1, len(replies))
			} else if e := replies[i]; e.Actor != "alpha/human" || e.Subject != m.ID || e.Message != charCityName+"/human" || !bytes.Equal(e.Payload, mailEventPayload(&m)) {
				t.Errorf("mail.replied event=%+v payload=%s, delivered=%+v", e, e.Payload, m)
			}
		}
	}
	if duds, err := local.Inbox(from); err != nil || len(duds) != 0 {
		t.Errorf("local replies=%d err=%v", len(duds), err)
	}
	// An explicitly selected city still replies inside its own store.
	cityFlag = ""
	contextFlag = "peer"
	if rc := cmdMailReplyJSON([]string{original.ID, "Within alpha"}, "", "", false, false, &out, &errOut); rc != 0 {
		t.Fatal(errOut.String())
	}
	if got := replyEvents(); len(got) != 2 {
		t.Errorf("after explicit context reply, got %d mail.replied events, want 2", len(got))
	}
	after, err := os.Stat(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(activeFile, after) {
		t.Error("a one-shot reply rotated the active supervisor log")
	}
	supervisor.Record(events.Event{Type: events.MailSent, Subject: "after"})
	all, err := events.ReadAll(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 || all[len(all)-1].Subject != "after" {
		t.Error("supervisor event after the reply is missing from the log")
	}
	want := []string{"POST /v0/city/alpha/mail", "GET /v0/city/alpha/mail", "POST /v0/city/" + charCityName + "/mail", "POST /v0/city/" + charCityName + "/mail", "POST /v0/city/alpha/mail/" + original.ID + "/reply"}
	if strings.Join(hits, ",") != strings.Join(want, ",") {
		t.Errorf("requests=%v want %v", hits, want)
	}
}

func TestMailReplyToLocalSender(t *testing.T) {
	clearRemoteMailIdentityEnv(t)
	t.Setenv("GC_MAIL", "")
	const rigConfig = "\n[[rigs]]\nname = 'forge'\npath = 'forge'\n\n[[agent]]\nname = 'worker'\nscope = 'rig'\n"
	city := newCharCity(t, charCityBasic+rigConfig, func(t *testing.T, store beads.Store) {
		for _, alias := range []string{"operator", "crew/night"} {
			_, err := store.Create(beads.Bead{
				Type: session.BeadType, Labels: []string{session.LabelSession},
				Metadata: map[string]string{"alias": alias},
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		retired, err := store.Create(beads.Bead{
			Type: session.BeadType, Labels: []string{session.LabelSession},
			Metadata: map[string]string{"alias": "crew/retired", "alias_history": "crew/former"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(retired.ID); err != nil {
			t.Fatal(err)
		}
	})
	t.Setenv("GC_CITY", city.cityPath)
	provider := city.cs.MailProvider("")
	for _, sender := range []string{"operator", "forge/worker", "crew/night"} {
		original, err := provider.Send(sender, "human", "Question", "Any news?")
		if err != nil {
			t.Fatal(err)
		}
		var out, errOut bytes.Buffer
		if rc := cmdMailReplyJSON([]string{original.ID, "An answer"}, "", "", false, true, &out, &errOut); rc != 0 {
			t.Fatalf("local reply to %s: exit=%d stderr=%q", sender, rc, &errOut)
		}
		inbox, err := provider.Inbox(sender)
		if err != nil || len(inbox) != 1 || inbox[0].ReplyTo != original.ID {
			t.Errorf("local inbox for %s: %+v, err=%v", sender, inbox, err)
		}
	}
	orphan, err := provider.Send("unknown", "human", "Question", "Any news?")
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	rc := cmdMailReplyJSON([]string{orphan.ID, "An answer"}, "", "", false, true, &out, &errOut)
	inbox, err := provider.Inbox("unknown")
	if rc == 0 || !strings.Contains(out.String(), "gc --context CITY mail send unknown") || len(inbox) != 0 || err != nil {
		t.Errorf("unknown sender: exit=%d inbox=%v err=%v output=%q", rc, inbox, err, &out)
	}
	// Exec mail and older unbound messages only supply the display address.
	alias := mail.Message{From: "crew/night"}
	resolver := mailReplyResolver{cfg: city.cs.Config(), sessions: city.cs.SessionsBeadStore().Store}
	if target, _, err := resolver.target(alias, false, io.Discard); target != nil || err != nil {
		t.Errorf("local session alias without stored sender binding: target=%v err=%v", target, err)
	}
	registry := clientcontext.File{Contexts: []clientcontext.Context{
		{Name: "forge", URL: "https://peer.invalid"},
		{Name: "crew", URL: "https://crew.invalid"},
	}}
	if err := registry.Save(DefaultPath()); err != nil {
		t.Fatal(err)
	}
	target, _, err := resolver.target(mail.Message{From: "forge/worker"}, false, io.Discard)
	if target != nil || err != nil {
		t.Errorf("configured local agent: target=%v err=%v", target, err)
	}
	target, _, err = resolver.target(mail.Message{From: "forge/ghost"}, false, io.Discard)
	if target != nil || err == nil || !strings.Contains(err.Error(), "gc --context forge mail send ghost") {
		t.Fatalf("ambiguous local rig and peer city: target=%v err=%v", target, err)
	}
	for _, address := range []string{alias.From, "crew/former", "crew/retired"} {
		if target, _, err := resolver.target(mail.Message{From: address}, false, io.Discard); target != nil || err == nil || !strings.Contains(err.Error(), "gc --context crew mail send ") {
			t.Errorf("ambiguous session address %q and peer city: target=%v err=%v", address, target, err)
		}
	}
}

func TestMailReplyToLocalSenderWithMatchingContext(t *testing.T) {
	for _, name := range []string{"forge", "peer"} {
		t.Run(name, func(t *testing.T) {
			clearRemoteMailIdentityEnv(t)
			t.Setenv("GC_MAIL", "")
			const rig = "\n[[rigs]]\nname = 'forge'\npath = 'forge'\n[[agent]]\nname = 'worker'\nscope = 'rig'\n"
			city := newCharCity(t, charCityBasic+rig, func(t *testing.T, store beads.Store) {
				_, err := store.Create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{"alias": "forge/lane"}})
				if err != nil {
					t.Fatal(err)
				}
			})
			t.Setenv("GC_CITY", city.cityPath)
			server := unexpectedMailServer(t)
			var errOut bytes.Buffer
			if rc := doContextAdd(clientcontext.Context{Name: name, City: "forge", URL: server.URL}, io.Discard, &errOut); rc != 0 {
				t.Fatal(errOut.String())
			}
			provider := city.cs.MailProvider("")
			for _, from := range []string{"forge/lane", "forge/worker", "forge/ghost"} {
				original, err := provider.Send(from, "human", "Question", "Any news?")
				if err != nil {
					t.Fatal(err)
				}
				stored, err := provider.Get(original.ID)
				if err != nil || (stored.FromSessionID != "") != (from == "forge/lane") {
					t.Fatalf("sender %q binding=%q err=%v", from, stored.FromSessionID, err)
				}
				for i, jsonOut := range []bool{false, true} {
					var out, stderr bytes.Buffer
					rc := cmdMailReplyJSON([]string{original.ID, "Answer"}, "", "", false, jsonOut, &out, &stderr)
					inbox, err := provider.Inbox(from)
					if err != nil {
						t.Fatal(err)
					}
					if from == "forge/ghost" {
						msg := out.String() + stderr.String()
						if rc != 1 || len(inbox) != 0 || !strings.Contains(msg, "matches both a local address namespace") || !strings.Contains(msg, "gc --context "+name+" mail send ghost") {
							t.Errorf("collision: rc=%d deliveries=%d diagnostic=%q", rc, len(inbox), msg)
						}
						continue
					}
					if rc != 0 || len(inbox) != i+1 || inbox[0].ReplyTo != original.ID {
						t.Errorf("%s json=%t: rc=%d inbox=%v stderr=%q", from, jsonOut, rc, inbox, &stderr)
					}
					if jsonOut {
						var result mailActionResult
						if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.OK || result.City != "" {
							t.Errorf("local JSON %q err=%v", &out, err)
						}
					} else if strings.Contains(out.String(), "(as ") {
						t.Errorf("unexpected remote confirmation %q", &out)
					}
				}
			}
		})
	}
}

func TestMailReplyRefusesUnresolvedOrigin(t *testing.T) {
	clearRemoteMailIdentityEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	seedLocalCity(t)
	registry := clientcontext.File{Contexts: []clientcontext.Context{
		{Name: "peer-one", City: "ridge", URL: "https://one.invalid"},
		{Name: "peer-two", City: "ridge", URL: "https://two.invalid"},
		{Name: "hollow", City: "elsewhere", URL: "https://three.invalid"},
	}}
	if err := registry.Save(DefaultPath()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ from, hint string }{
		{"", "gc --context CITY mail send AGENT"},
		{"/worker", "gc --context CITY mail send AGENT"},
		{"ridge/", "gc --context CITY mail send AGENT"},
		{"ridge/forge/worker", "gc --context CITY mail send forge/worker"},
		{"hollow/worker", "gc --context CITY mail send worker"},
	} {
		resolver := mailReplyResolver{cfg: &config.City{}}
		target, _, err := resolver.target(mail.Message{From: tc.from}, false, io.Discard)
		if target != nil || err == nil || !strings.Contains(err.Error(), tc.hint) {
			t.Errorf("sender %q: target=%v err=%v, want %q", tc.from, target, err, tc.hint)
		}
	}
}

func TestMailReplyWithUnreadableConfig(t *testing.T) {
	for _, file := range []string{"city.toml", "contexts.toml"} {
		t.Run(file, func(t *testing.T) {
			clearRemoteMailIdentityEnv(t)
			t.Setenv("GC_MAIL", "")
			const cfg = "\n[[rigs]]\nname = 'forge'\npath = 'forge'\n[[agent]]\nname = 'worker'\nscope = 'rig'\n[[agent]]\nname = 'alice'\n"
			city := newCharCity(t, charCityBasic+cfg, nil)
			t.Setenv("GC_CITY", city.cityPath)
			sessions := city.cs.SessionsBeadStore().Store
			addAlias := func(alias string) {
				t.Helper()
				_, err := sessions.Create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{"alias": alias}})
				if err != nil {
					t.Fatal(err)
				}
			}
			addAlias("forge/lane")
			provider := city.cs.MailProvider("")
			senders := []string{"alice", "forge/lane", "forge/worker", "forge/late", "ridge/worker"}
			originals := map[string]string{}
			for _, from := range senders {
				m, err := provider.Send(from, "human", "Question", "Any news?")
				if err != nil {
					t.Fatal(err)
				}
				originals[from] = m.ID
			}
			addAlias("forge/late")
			path := DefaultPath()
			broken := "[broken"
			if file == "city.toml" {
				broken = "[[agent]]\nname = 42\n"
				path = filepath.Join(city.cityPath, file)
			}
			if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, from := range senders {
				t.Run(from, func(t *testing.T) {
					var out, stderr bytes.Buffer
					id := originals[from]
					rc := cmdMailReply([]string{id, "Answer"}, "", "", false, &out, &stderr)
					inbox, err := provider.Inbox(from)
					if err != nil {
						t.Fatal(err)
					}
					wantRC, diagnostic := 0, ""
					switch {
					case file == "city.toml" && from == "alice":
						diagnostic = `recipient "alice" not checked`
					case file == "city.toml" && (from == "forge/worker" || from == "ridge/worker"):
						wantRC, diagnostic = 1, "cannot tell whether \""+from+"\" is local; use gc --context CITY mail send worker"
					case file == "contexts.toml" && from == "forge/late":
						diagnostic = "replied locally without checking contexts"
					case file == "contexts.toml" && from == "ridge/worker":
						wantRC, diagnostic = 1, "use gc --context CITY mail send worker after configuring the origin"
					}
					if rc != wantRC || (wantRC == 1 && len(inbox) != 0) || (wantRC == 0 && (len(inbox) != 1 || inbox[0].ReplyTo != id)) {
						t.Errorf("rc=%d want=%d inbox=%v stderr=%q", rc, wantRC, inbox, &stderr)
					}
					warnings := 0
					if wantRC == 0 && diagnostic != "" {
						warnings = 1
					}
					if !strings.Contains(stderr.String(), diagnostic) || strings.Count(stderr.String(), "gc mail reply: warning:") != warnings {
						t.Errorf("stderr=%q want %d warnings and %q", &stderr, warnings, diagnostic)
					}
				})
			}
			if file == "contexts.toml" {
				t.Setenv("GC_ALIAS", "absent")
				var out, stderr bytes.Buffer
				rc := cmdMailReply([]string{originals["forge/late"], "Answer"}, "", "", false, &out, &stderr)
				inbox, err := provider.Inbox("forge/late")
				if rc != 1 || err != nil || len(inbox) != 1 || strings.Contains(stderr.String(), "replied locally") {
					t.Errorf("unresolved reply sender: rc=%d inbox=%v err=%v stderr=%q", rc, inbox, err, &stderr)
				}
			}
		})
	}
}

func TestRemoteMailSubject(t *testing.T) {
	if got := remoteMailSubject("s", "b"); got != "s" {
		t.Errorf("explicit subject: %q", got)
	}
	if got := remoteMailSubject("", "first line\nsecond"); got != "first line" {
		t.Errorf("first-line derivation: %q", got)
	}
	long := strings.Repeat("x", 100)
	if got := remoteMailSubject("", long); len(got) != 80 || !strings.HasSuffix(got, "...") {
		t.Errorf("truncation: %d %q", len(got), got)
	}
	// A body that starts with blank lines still yields a non-empty subject
	// (the API requires minLength 1), and an all-blank body yields "" so the
	// caller refuses locally instead of round-tripping to a 422.
	if got := remoteMailSubject("", "\n\n  hello\nworld"); got != "hello" {
		t.Errorf("leading blank lines: %q", got)
	}
	if got := remoteMailSubject("  ", "\n \t\n"); got != "" {
		t.Errorf("all-blank must be empty, got %q", got)
	}
}

func unexpectedMailServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected remote mail request %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Local-only modes are refused before the wire is touched.
func TestCmdMailSendRemote_RefusesLocalOnlyModes(t *testing.T) {
	srv := unexpectedMailServer(t)
	cases := []struct {
		name        string
		notify, all bool
		args        []string
		want        string
	}{
		{"all", false, true, []string{"body"}, "--all"},
		{"no-recipient", false, false, nil, "missing recipient"},
		{"no-body", false, false, []string{"mayor"}, "usage"},
		{"blank-body", false, false, []string{"mayor", "\n\t "}, "usage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := cmdMailSendRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), tc.args, tc.notify, tc.all, "", "", "", "", false, &out, &errb)
			if code == 0 || !strings.Contains(errb.String(), tc.want) {
				t.Errorf("exit=%d stderr=%q (want %q)", code, errb.String(), tc.want)
			}
		})
	}
	// Remote replies still refuse --notify.
	var out, errb bytes.Buffer
	if code := cmdMailReplyRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mc-1"}, "", "b", true, false, &out, &errb); code == 0 || !strings.Contains(errb.String(), "--notify") {
		t.Errorf("reply --notify: exit=%d stderr=%q", code, errb.String())
	}
}

// A remote send POSTs /v0/city/{city}/mail with the CSRF header, the
// city-qualified sender, and a derived subject when -s is absent; it echoes the
// resolved target on stderr and renders the server's message.
func TestCmdMailSendRemote_PostsAndRenders(t *testing.T) {
	clearRemoteMailIdentityEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_ALIAS", "mayor")
	seedLocalCity(t)

	var gotPath, gotReq, gotMethod string
	var requests, grants []string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		grants = append(grants, r.Header.Get("X-GC-City-Write"))
		gotPath, gotReq, gotMethod = r.URL.Path, r.Header.Get("X-GC-Request"), r.Method
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(remoteMailMessageJSON))
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := doContextAdd(clientcontext.Context{Name: "peer", URL: srv.URL, City: "mc"}, &out, &errb); code != 0 {
		t.Fatalf("seed peer context: %s", &errb)
	}
	out.Reset()
	errb.Reset()
	code := run([]string{"--context", "peer", "mail", "send", "mayor", "round trip"}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/city/mc/mail" || gotReq == "" {
		t.Errorf("method=%q path=%q req=%q", gotMethod, gotPath, gotReq)
	}
	if len(requests) != 1 || requests[0] != "POST /v0/city/mc/mail" {
		t.Fatalf("remote mail send requests = %v, want exactly one POST", requests)
	}
	t.Logf("gc --context peer mail send requests: %v", requests)
	if gotBody["to"] != "mayor" || gotBody["from"] != "alpha/mayor" || gotBody["subject"] != "round trip" || gotBody["body"] != "round trip" {
		t.Errorf("body=%v", gotBody)
	}
	if !strings.Contains(out.String(), "Sent message mc-wisp-1 to mayor (as alpha/mayor)") {
		t.Errorf("stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "target:") || !strings.Contains(errb.String(), "mc @") {
		t.Errorf("remote send did not echo the resolved target: %q", errb.String())
	}

	// A city's registered key is for sending: with a grant_command the same send
	// makes exactly one request, the mail POST carrying the minted write grant,
	// and no read (no GET, no HEAD) that a hardened peer would refuse.
	t.Run("write-grant-source-makes-no-read", func(t *testing.T) {
		token := base64.RawURLEncoding.EncodeToString([]byte(`{"aud":"gc-city-write"}`)) + "." +
			base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, ed25519.SignatureSize))
		var out, errb bytes.Buffer
		if code := doContextAdd(clientcontext.Context{Name: "peer-signed", URL: srv.URL, City: "mc", GrantCommand: "printf '%s' '" + token + "'"}, &out, &errb); code != 0 {
			t.Fatalf("seed signed peer context: %s", &errb)
		}
		requests, grants = nil, nil
		out.Reset()
		errb.Reset()
		if code := run([]string{"--context", "peer-signed", "mail", "send", "mayor", "round trip"}, &out, &errb); code != 0 {
			t.Fatalf("exit %d; stderr=%q", code, errb.String())
		}
		t.Logf("gc --context peer-signed mail send requests: %v (write grant attached: %v)", requests, len(grants) == 1 && grants[0] == token)
		for _, req := range requests {
			if strings.HasPrefix(req, http.MethodGet+" ") || strings.HasPrefix(req, http.MethodHead+" ") {
				t.Errorf("remote mail send made a read: %s", req)
			}
		}
		if len(requests) != 1 || requests[0] != "POST /v0/city/mc/mail" || len(grants) != 1 || grants[0] != token {
			t.Fatalf("remote mail send requests = %v, want exactly one POST /v0/city/mc/mail carrying the write grant", requests)
		}
	})
}

// --to / -s / -m and --from are honored; --json emits the local mail.send shape
// and no human target echo.
func TestCmdMailSendRemote_FlagsAndJSON(t *testing.T) {
	clearRemoteMailIdentityEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(remoteMailMessageJSON))
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	code := cmdMailSendRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), nil, false, false, "citadel/mayor", "mayor", "hello", "round trip", true, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	if gotBody["to"] != "mayor" || gotBody["from"] != "citadel/mayor" || gotBody["subject"] != "hello" || gotBody["body"] != "round trip" {
		t.Errorf("body=%v", gotBody)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output not JSON: %v (%q)", err, out.String())
	}
	if got["ok"] != true || got["command"] != "mail.send" || got["id"] != "mc-wisp-1" {
		t.Errorf("json=%v", got)
	}
	if strings.Contains(errb.String(), "target:") {
		t.Errorf("json mode leaked the human target echo: %q", errb.String())
	}
}

type mailHandlerTransport struct{ handler http.Handler }

func (tr mailHandlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	tr.handler.ServeHTTP(rec, req)
	return rec.Result(), nil
}

func inProcessMailClient(t *testing.T, handler http.Handler) *api.Client {
	t.Helper()
	client, err := api.NewRemoteCityScopedClient("http://beta.test", "beta", api.RemoteOptions{RESTTransport: mailHandlerTransport{handler}})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// Exercise the real receiving controller with a separate ambient sender city.
func TestRemoteMailNotifyIdleRecipient(t *testing.T) {
	clearGCEnv(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	seedLocalCity(t)
	dir := t.TempDir()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "beta"}}
	cfg.Daemon.NudgeDispatcher = "supervisor"
	mgr := newSessionManagerWithConfig(dir, store, sp, cfg)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template: "worker", Title: "Worker", Command: "claude", WorkDir: dir, Provider: "claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatal(err)
	}
	sp.WaitForIdleErrors[info.SessionName] = nil
	state := &controllerState{cityName: "beta", cityPath: dir, cfg: cfg, sp: sp, cityBeadStore: store, cityMailProv: mail.NewFake(), eventProv: events.NewFake()}
	mux := api.NewSupervisorMux(&singleCityStateResolver{state}, nil, false, "test", "", time.Now())
	client := inProcessMailClient(t, mux.WithAllowedHosts([]string{"beta.test"}).Handler())
	var out, stderr bytes.Buffer
	target := &remoteTarget{BaseURL: "http://beta.test", CityName: "beta", Source: remoteSourceURLFlag}
	if code := cmdMailSendRemote(client, target, []string{info.ID, "update"}, true, false, "alpha/worker", "", "", "", true, &out, &stderr); code != 0 {
		t.Fatalf("exit %d: %s %s", code, &out, &stderr)
	}
	var result mailActionResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.Notified {
		t.Fatalf("result %s: %v", &out, err)
	}
	msg, err := state.cityMailProv.Get(result.ID)
	if err != nil || msg.From != "alpha/worker" {
		t.Fatalf("stored mail %+v, %v", msg, err)
	}
	var deliveries int
	for _, call := range sp.SnapshotCalls() {
		if call.Method == "NudgeNow" {
			deliveries++
			if !strings.Contains(call.Message, "[mail] You have new mail") || strings.Contains(call.Message, msg.From) {
				t.Errorf("unexpected runtime reminder %q", call.Message)
			}
		}
	}
	if deliveries != 1 {
		t.Errorf("live notifications = %d", deliveries)
	}

	// Hold the same live-delivery slot as a first send waiting for idle.
	// A second send receives its own durable, read-before-delivery reference.
	if _, occupied := state.liveMailNotifies.LoadOrStore(info.ID, struct{}{}); occupied {
		t.Fatal("live delivery slot leaked")
	}
	defer state.liveMailNotifies.Delete(info.ID)
	second, err := client.SendMail(api.MailSendRequest{To: info.ID, Subject: "another update", Notify: true})
	if err != nil || second.Notified == nil || !*second.Notified {
		t.Fatalf("overlapping send: %+v, %v", second, err)
	}
	pending, _, _, err := listQueuedNudges(dir, info.ID, time.Now())
	if err != nil || len(pending) != 1 || pending[0].Reference == nil || pending[0].Reference.ID != second.ID {
		t.Fatalf("queued reminder for %q: %+v, %v", second.ID, pending, err)
	}
	if got := sp.CountCalls("NudgeNow", info.SessionName); got != 1 {
		t.Errorf("overlapping send made a second live delivery: %d", got)
	}
}

func TestRemoteMailNotifyRespectsRecipientList(t *testing.T) {
	clearGCEnv(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	seedLocalCity(t)
	cfg := mailWakeTestCity()
	cfg.Workspace.Name = "beta"
	cfg.Daemon.NudgeDispatcher = "supervisor"
	cfg.Agents[0].MailWakeFrom = []string{"human", "city:alpha"}
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	state := &controllerState{
		cityName: "beta", cityPath: t.TempDir(), cfg: cfg, sp: sp,
		cityBeadStore: store, cityMailProv: mail.NewFake(), eventProv: events.NewFake(),
	}
	mux := api.NewSupervisorMux(&singleCityStateResolver{state}, nil, false, "test", "", time.Now()).WithAllowedHosts([]string{"beta.test"})
	client := inProcessMailClient(t, mux.Handler())
	result, err := client.SendMail(api.MailSendRequest{To: "reader", From: "alpha/worker", Subject: "update", Notify: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Notified == nil || *result.Notified {
		t.Fatalf("unlisted sender requested a turn: %+v", result)
	}
	if _, err := state.cityMailProv.Get(result.ID); err != nil {
		t.Fatalf("stored message: %v", err)
	}
	records, err := store.List(beads.ListQuery{Type: session.BeadType, TierMode: beads.TierBoth})
	if err != nil || len(records) != 0 {
		t.Fatalf("unlisted sender materialized a recipient: %v, %v", records, err)
	}

	// The request runs inside the receiving controller. Only its provider
	// knows that this recipient is already running.
	t.Setenv("GC_SESSION", "fake")
	mgr := newSessionManagerWithConfig(state.cityPath, store, sp, cfg)
	reader, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template: "reader", Title: "Reader", Command: "claude", WorkDir: state.cityPath, Provider: "claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Start(context.Background(), reader.ID, "", runtime.Config{WorkDir: state.cityPath}); err != nil {
		t.Fatal(err)
	}
	sp.WaitForIdleErrors[reader.SessionName] = nil

	// A request that passed no write gate is nobody, even when it writes a
	// listed local name.
	result, err = client.SendMail(api.MailSendRequest{To: reader.ID, From: "human", Subject: "follow-up", Notify: true})
	if err != nil {
		t.Fatal(err)
	}
	history, err := sessionFrontDoor(store).MailWakeHistory(reader.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Notified == nil || *result.Notified || len(history) != 0 {
		t.Fatalf("no key writing human: notified=%v, recipient wake history=%+v", result.Notified != nil && *result.Notified, history)
	}

	// The accepted sender is a peer city on the list, named by the key the
	// receiving write gate verified.
	keys := t.TempDir()
	if err := os.Chmod(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keys, "alpha.pub"), []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hardened, err := api.NewHardenedAuth(mux, supervisor.Section{Hardened: supervisor.HardenedListener{Port: 8447, TrustedKeysDir: keys}}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	answer := peerMailPost(t, hardened, priv, "alpha", reader.ID, "alpha/worker")
	history, err = sessionFrontDoor(store).MailWakeHistory(reader.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !answer.notified || len(history) != 1 || history[0].Sender != "city:alpha" {
		t.Fatalf("listed peer city: %+v, recipient wake history=%+v", answer, history)
	}
}

func TestRemoteMailNotifyMaterializesInReceivingCity(t *testing.T) {
	clearGCEnv(t)
	clearInheritedCityRoutingEnv(t)
	seedLocalCity(t)
	t.Setenv("GC_SESSION", "absent-provider")
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "beta"},
		Agents:        []config.Agent{{Name: "worker", Provider: "codex", StartCommand: "true"}},
		NamedSessions: []config.NamedSession{{Template: "worker"}},
	}
	target, err := resolveNudgeTargetInCity(dir, cfg, store, "worker", sp)
	if err != nil {
		t.Fatal(err)
	}
	if target.cityPath != dir || target.sessionID == "" || !sp.IsRunning(target.sessionName) {
		t.Fatalf("named recipient was not started on its city's provider: %q, %q", target.cityPath, target.sessionName)
	}
}

func TestCmdMailSendRemoteNotify(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		status                 int
		response               string
		wantAttempts, wantExit int
		wantNotified           bool
	}{
		{"accepted", 201, `{"id":"letter","notified":true}`, 1, 0, true},
		{"declined", 201, `{"id":"letter","notified":false}`, 1, 0, false},
		{"ignores field", 201, `{"id":"letter"}`, 1, 0, false},
		{"old peer", 422, `{"errors":[{"location":"body.notify","message":"unexpected property"}]}`, 2, 0, false},
		{"other validation error", 422, `{"errors":[{"location":"body.to","message":"unexpected property"}]}`, 1, 1, false},
		{"unknown recipient", 400, `{"detail":"unknown recipient"}`, 1, 1, false},
		{"server error", 500, `{"detail":"send failed"}`, 1, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts := 0
			client := inProcessMailClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts++
				var req struct {
					Notify            *bool
					To, From, Subject string
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if req.To != "worker" || req.From != "alpha/worker" || req.Subject != "update" {
					t.Errorf("wrong mail: %+v", req)
				}
				if attempts == 1 && (req.Notify == nil || !*req.Notify) || attempts > 1 && req.Notify != nil {
					t.Errorf("wrong notify field on attempt %d: %+v", attempts, req.Notify)
				}
				status, body := tc.status, tc.response
				if attempts == 2 {
					status, body = http.StatusCreated, `{"id":"letter"}`
				}
				contentType := "application/json"
				if status >= 400 {
					contentType = "application/problem+json"
				}
				w.Header().Set("Content-Type", contentType)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
			}))
			var out, stderr bytes.Buffer
			code := cmdMailSendRemote(client, &remoteTarget{BaseURL: "http://beta.test", CityName: "beta"}, []string{"worker", "update"}, true, false, "alpha/worker", "", "", "", true, &out, &stderr)
			if code != tc.wantExit || attempts != tc.wantAttempts {
				t.Fatalf("exit %d, attempts %d, stdout %s, stderr %s", code, attempts, &out, &stderr)
			}
			if code != 0 {
				return
			}
			var result struct {
				ID       string
				Notified *bool
			}
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.ID != "letter" || result.Notified == nil || *result.Notified != tc.wantNotified {
				t.Fatalf("result: %s (%v)", &out, err)
			}
			if tc.wantAttempts == 2 && !strings.Contains(stderr.String(), "too old") {
				t.Errorf("expected compatibility notice: %s", &stderr)
			}
		})
	}
}

// A server error surfaces (non-fallbackable) with a non-zero exit.
func TestCmdMailSendRemote_ServerErrorSurfaces(t *testing.T) {
	clearRemoteMailIdentityEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"title":"Unauthorized","status":401,"detail":"write grant required"}`))
	}))
	defer srv.Close()
	var out, errb bytes.Buffer
	code := cmdMailSendRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mayor", "x"}, false, false, "", "", "", "", false, &out, &errb)
	if code == 0 || !strings.Contains(errb.String(), "gc mail send:") {
		t.Errorf("exit=%d stderr=%q", code, errb.String())
	}
}

// A remote reply POSTs /v0/city/{city}/mail/{id}/reply with the qualified
// sender and renders the reply.
func TestCmdMailReplyRemote_Posts(t *testing.T) {
	clearRemoteMailIdentityEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_ALIAS", "mayor")
	seedLocalCity(t)
	var gotPath, gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"mc-wisp-2","from":"alpha/mayor","to":"jadegate/mayor","subject":"Re: hello","body":"ack","created_at":"2026-08-18T17:01:00Z","read":false,"reply_to":"mc-wisp-1"}`))
	}))
	defer srv.Close()
	var out, errb bytes.Buffer
	code := cmdMailReplyRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mc-wisp-1", "ack"}, "", "", false, false, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/city/mc/mail/mc-wisp-1/reply" {
		t.Errorf("method=%q path=%q", gotMethod, gotPath)
	}
	if gotBody["from"] != "alpha/mayor" || gotBody["body"] != "ack" {
		t.Errorf("body=%v", gotBody)
	}
	if !strings.Contains(out.String(), "Replied to mc-wisp-1 — sent message mc-wisp-2 to jadegate/mayor (as alpha/mayor)") {
		t.Errorf("stdout=%q", out.String())
	}
}

// A remote inbox GETs /v0/city/{city}/mail?agent=… — the explicit recipient,
// or this client's own qualified identity by default — and renders like a
// local inbox.
func TestCmdMailInboxRemote_ListsAndDefaultsRecipient(t *testing.T) {
	clearRemoteMailIdentityEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_ALIAS", "mayor")
	seedLocalCity(t)
	var gotAgents []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/city/mc/mail" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		gotAgents = append(gotAgents, r.URL.Query().Get("agent"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[` + remoteMailMessageJSON + `],"total":1}`))
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := cmdMailInboxRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mayor"}, false, &out, &errb); code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "mc-wisp-1") || !strings.Contains(out.String(), "alpha/mayor") {
		t.Errorf("stdout=%q", out.String())
	}
	out.Reset()
	errb.Reset()
	if code := cmdMailInboxRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), nil, true, &out, &errb); code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	if len(gotAgents) != 2 || gotAgents[0] != "mayor" || gotAgents[1] != "alpha/mayor" {
		t.Errorf("agent params=%v", gotAgents)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output not JSON: %v (%q)", err, out.String())
	}
	if got["recipient"] != "alpha/mayor" {
		t.Errorf("json recipient=%v", got["recipient"])
	}
}

// The remote inbox pages through the server's keyset pagination until the
// mailbox is exhausted, and a partial aggregate read is an error rather than a
// silently short list.
func TestCmdMailInboxRemote_PagesAndRefusesPartial(t *testing.T) {
	clearRemoteMailIdentityEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	var cursors []string
	partial := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := r.URL.Query().Get("cursor")
		cursors = append(cursors, c)
		w.Header().Set("Content-Type", "application/json")
		if partial {
			_, _ = w.Write([]byte(`{"items":[` + remoteMailMessageJSON + `],"total":1,"partial":true,"partial_errors":["rig alpha: provider timeout"]}`))
			return
		}
		switch c {
		case "":
			_, _ = w.Write([]byte(`{"items":[` + remoteMailMessageJSON + `],"total":3,"next_cursor":"p2"}`))
		case "p2":
			_, _ = w.Write([]byte(`{"items":[` + strings.Replace(remoteMailMessageJSON, "mc-wisp-1", "mc-wisp-2", 1) + `],"total":3,"next_cursor":"p3"}`))
		default:
			_, _ = w.Write([]byte(`{"items":[` + strings.Replace(remoteMailMessageJSON, "mc-wisp-1", "mc-wisp-3", 1) + `],"total":3}`))
		}
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	if code := cmdMailInboxRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mayor"}, false, &out, &errb); code != 0 {
		t.Fatalf("exit %d; stderr=%q", code, errb.String())
	}
	if strings.Join(cursors, ",") != ",p2,p3" {
		t.Errorf("cursors=%v (want first page, p2, p3)", cursors)
	}
	for _, id := range []string{"mc-wisp-1", "mc-wisp-2", "mc-wisp-3"} {
		if !strings.Contains(out.String(), id) {
			t.Errorf("stdout missing %s: %q", id, out.String())
		}
	}
	partial = true
	out.Reset()
	errb.Reset()
	if code := cmdMailInboxRemote(remoteTestClient(t, srv.URL), remoteTestTarget(srv.URL), []string{"mayor"}, false, &out, &errb); code == 0 || !strings.Contains(errb.String(), "partial") || !strings.Contains(errb.String(), "provider timeout") {
		t.Errorf("partial read must fail: exit=%d stderr=%q stdout=%q", code, errb.String(), out.String())
	}
	if strings.Contains(out.String(), "mc-wisp") {
		t.Errorf("partial read rendered messages: %q", out.String())
	}
}

// Dispatch matrix beyond the happy path: a remote failure surfaces non-zero
// with no local fallback; GC_NO_API plus --context is a loud conflict; and
// with no remote selector and no local city, the "no city" error is the local
// one (the remote arm never engages).
func TestCmdMail_ContextDispatchMatrix(t *testing.T) {
	clearRemoteMailIdentityEnv(t)
	t.Setenv("GC_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"title":"Internal","status":500,"detail":"boom"}`))
	}))
	defer srv.Close()
	var out, errb bytes.Buffer
	if code := doContextAdd(clientcontext.Context{Name: "peer", URL: srv.URL, City: "mc"}, &out, &errb); code != 0 {
		t.Fatalf("seed context: %q", errb.String())
	}
	prev := contextFlag
	t.Cleanup(func() { contextFlag = prev })

	// 1. Remote failure: non-zero, no "Sent message", no local fallback.
	contextFlag = "peer"
	out.Reset()
	errb.Reset()
	if code := cmdMailSendJSON([]string{"mayor", "hi"}, false, false, "", "", "", "", "", false, &out, &errb); code == 0 || strings.Contains(out.String(), "Sent message") || !strings.Contains(errb.String(), "gc mail send:") {
		t.Errorf("remote failure: exit=%d stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	// 2. GC_NO_API + --context is a conflict, surfaced before any wire call.
	t.Setenv("GC_NO_API", "1")
	out.Reset()
	errb.Reset()
	if code := cmdMailSendJSON([]string{"mayor", "hi"}, false, false, "", "", "", "", "", false, &out, &errb); code == 0 || !strings.Contains(errb.String(), "GC_NO_API") {
		t.Errorf("GC_NO_API conflict: exit=%d stderr=%q", code, errb.String())
	}
	t.Setenv("GC_NO_API", "")
	// 3. No remote selector, no local city: the local "not in a city" error,
	//    i.e. the remote arm stays out of the way.
	contextFlag = ""
	out.Reset()
	errb.Reset()
	if code := cmdMailSendJSON([]string{"mayor", "hi"}, false, false, "", "", "", "", "", false, &out, &errb); code == 0 || !strings.Contains(errb.String(), "city") {
		t.Errorf("no city: exit=%d stderr=%q", code, errb.String())
	}
	if strings.Contains(errb.String(), "target:") {
		t.Errorf("no remote selector must not echo a remote target: %q", errb.String())
	}
}
