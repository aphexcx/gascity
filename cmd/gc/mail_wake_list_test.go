package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestMailWakePoolSettings(t *testing.T) {
	a := config.Agent{Name: "reader", MailWakeFrom: []string{"demo/source", "human"}, MailWakeStart: true}
	worker := deepCopyAgent(&a, "reader-1", "demo")
	if !worker.MailWakeStart || !slices.Equal(worker.MailWakeFrom, a.MailWakeFrom) {
		t.Fatalf("pool worker settings = %v, %v", worker.MailWakeFrom, worker.MailWakeStart)
	}
	worker.MailWakeFrom[0] = "changed"
	if a.MailWakeFrom[0] == "changed" {
		t.Fatal("pool worker shares sender list with template")
	}
}

func TestMailWakeSendReloadsSettings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_CITY", dir)
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_MAIL", "beads")
	t.Setenv("GC_SESSION_ID", "another-session")
	startSessionResetTestController(t, dir)
	store := openNudgeBeadStore(dir)
	send := func(notify bool, dedup string) mailActionResult {
		t.Helper()
		var out, stderr bytes.Buffer
		code := cmdMailSendJSON([]string{"reader"}, notify, false, "human", "", "Update", "Body", dedup, true, &out, &stderr)
		if code != 0 {
			t.Fatalf("send failed (%d): %s", code, stderr.String())
		}
		var result mailActionResult
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	writeConfig := func(list string) {
		t.Helper()
		content := fmt.Sprintf("[workspace]\nname = \"alpha\"\n[[agent]]\nname = \"reader\"\nprovider = \"codex\"\nstart_command = \"echo\"\nmail_wake_from = [%s]\nmail_wake_start = true\n[[named_session]]\ntemplate = \"reader\"\n", list)
		if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig("")
	if send(false, "").Notified {
		t.Fatal("empty list requested a turn")
	}
	writeConfig(`"human"`)
	if !send(false, "update").Notified {
		t.Fatal("send after config rewrite did not request the named session")
	}
	cfg, err := loadCityConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := resolveSessionIDWithConfig(dir, cfg, store.Store, "reader")
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Get(id)
	if err != nil || b.Metadata["state"] != "start-pending" || b.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("new named session: %+v (%v)", b.Metadata, err)
	}
	front := sessionFrontDoor(store.Store)
	history, err := front.MailWakeHistory(id)
	if err != nil || len(history) != 1 || history[0].Sender != "human" {
		t.Fatalf("new named session history = %v, %v", history, err)
	}
	if dup := send(false, "update"); !dup.AlreadyDone || dup.Notified {
		t.Fatalf("duplicate=%+v", dup)
	}
	if !send(true, "").Notified {
		t.Fatal("flagged mail did not request reminder")
	}
	after, err := front.MailWakeHistory(id)
	if err != nil || !slices.Equal(after, history) {
		t.Fatalf("duplicate or flag spent a list wake: %v, %v", after, err)
	}
}

func TestMailWakeFlagPrecedence(t *testing.T) {
	for _, flag := range []bool{true, false} {
		for _, listed := range []bool{true, false} {
			for _, spent := range []bool{true, false} {
				flagCalls, listCalls := 0, 0
				var explicit nudgeFunc
				if flag {
					explicit = func(string, string) error { flagCalls++; return nil }
				}
				list := func(mail.Message) bool { listCalls++; return listed && !spent }
				var diagnostic bytes.Buffer
				got := notifyStoredMail(mail.Message{ID: "m", To: "reader"}, explicit, list, "gc mail send", &diagnostic)
				if got != (flag || listed && !spent) || (flagCalls == 1) != flag || (listCalls == 1) == flag || diagnostic.Len() != 0 {
					t.Fatalf("flag=%t listed=%t spent=%t: notified=%t explicit=%d list=%d stderr=%q", flag, listed, spent, got, flagCalls, listCalls, diagnostic.String())
				}
			}
		}
	}
}

func TestMailWakeExplicitRequestReachesController(t *testing.T) {
	when := time.Unix(10000, 0).UTC()
	metadata := map[string]string{"state": "asleep", "sleep_reason": "idle"}
	for key, value := range session.RequestExplicitWakePatch("explicit", when) {
		metadata[key] = value
	}
	view := session.ProjectLifecycle(session.LifecycleInputFromMetadata("open", metadata))
	result := ComputeAwakeSet(AwakeInput{
		Agents:        []AwakeAgent{{QualifiedName: "reader"}},
		NamedSessions: []AwakeNamedSession{{Identity: "reader", Template: "reader", Mode: "on_demand"}},
		SessionBeads:  []AwakeSessionBead{{ID: "r", SessionName: "reader", Template: "reader", State: string(view.BaseState), NamedIdentity: "reader", ExplicitWake: view.HasWakeCause(session.WakeCauseExplicit), IdleSince: when.Add(-time.Hour)}},
		Now:           when,
	})
	assertAwake(t, result, "reader")
	assertReason(t, result, "reader", "explicit-wake")
}

func TestMailWakeStates(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, tc := range []struct {
		name    string
		facts   mailWakeFacts
		off, on mailWakeAction
	}{
		{"human", mailWakeFacts{human: true}, mailWakeNone, mailWakeNone},
		{"closed", mailWakeFacts{block: "closed", running: true}, mailWakeNone, mailWakeNone},
		{"held", mailWakeFacts{block: "held", ordinary: true, named: true, managed: true}, mailWakeNone, mailWakeNone},
		{"running", mailWakeFacts{recipientID: "r", running: true}, mailWakeRemind, mailWakeRemind},
		{"unnamed", mailWakeFacts{recipientID: "r", ordinary: true, managed: true}, mailWakeNone, mailWakeNone},
		{"unmanaged", mailWakeFacts{recipientID: "r", ordinary: true, named: true}, mailWakeNone, mailWakeNone},
		{"other sleep", mailWakeFacts{recipientID: "r", named: true, managed: true}, mailWakeNone, mailWakeNone},
		{"ordinary", mailWakeFacts{recipientID: "r", ordinary: true, named: true, managed: true}, mailWakeNone, mailWakeStart},
		{"missing", mailWakeFacts{named: true, managed: true}, mailWakeNone, mailWakeCreate},
	} {
		for _, start := range []bool{false, true} {
			a := config.Agent{MailWakeFrom: []string{"source"}, MailWakeStart: start}
			got, reason := decideMailWake(a, mailWakeSender{"source", "s"}, tc.facts, nil, nil, now)
			want := tc.off
			if start {
				want = tc.on
			}
			if got != want || (reason != "") != (want == mailWakeNone && !tc.facts.human) {
				t.Errorf("%s start=%t: action=%v reason=%q, want %v", tc.name, start, got, reason, want)
			}
		}
	}
}

func TestMailWakeLoop(t *testing.T) {
	for _, size := range []int{2, 3} {
		agents := []string{"first", "second", "third"}[:size]
		now := time.Unix(10000, 0)
		dir := t.TempDir()
		t.Setenv("GC_SESSION", "fake")
		t.Setenv("GC_BEADS", "file")
		store := beads.NewMemStore()
		cfg := &config.City{Workspace: config.Workspace{Name: "alpha"}}
		mp := beadmail.New(store)
		fake := runtime.NewFake()
		oldBuild := buildSessionProviderByName
		buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
			return fake, nil
		}
		t.Cleanup(func() { buildSessionProviderByName = oldBuild })
		ids := make(map[string]string)
		for i, name := range agents {
			cfg.Agents = append(cfg.Agents, config.Agent{Name: name, Provider: "claude", MailWakeFrom: []string{agents[(i+size-1)%size]}})
			mgr := newSessionManagerWithConfig(dir, store, fake, cfg)
			info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: name, Title: name, Command: "echo", WorkDir: dir, Provider: "claude"})
			if err != nil {
				t.Fatal(err)
			}
			if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
				t.Fatal(err)
			}
			fake.WaitForIdleErrors[info.SessionName] = nil
			ids[name] = info.ID
		}
		send := func(from, to string) (bool, string) {
			t.Helper()
			m, err := mp.Send(ids[from], ids[to], "Update", "Body")
			if err != nil {
				t.Fatal(err)
			}
			var diagnostic bytes.Buffer
			wake := newLocalMailWake(dir, cfg, store, store, from, ids[from], "gc mail send", &diagnostic, func() time.Time { return now })
			result := notifyStoredMail(m, nil, wake, "gc mail send", &diagnostic)
			return result, diagnostic.String()
		}
		for window := range 2 {
			sent, asked := 0, 0
			for i := 0; i < 16; i++ {
				from, to := agents[i%size], agents[(i+1)%size]
				sent++
				notified, diagnostic := send(from, to)
				if !notified {
					want := "budget"
					if size == 2 {
						want = "return-hop"
					}
					if !strings.Contains(diagnostic, want) {
						t.Errorf("size %d suppressed with %q, want %q", size, diagnostic, want)
					}
					break
				}
				asked++
			}
			want := 9
			if size == 2 {
				want = 1
			}
			if sent != want+1 || asked != want {
				t.Errorf("size %d, window %d: sent=%d asked=%d", size, window, sent, asked)
			}
			msgs, err := store.List(beads.ListQuery{Type: "message", TierMode: beads.TierBoth})
			if err != nil || len(msgs) != (window+1)*(want+1) {
				t.Fatalf("stored %d messages, err=%v", len(msgs), err)
			}
			live := 0
			for _, call := range fake.Calls {
				if call.Method == "NudgeNow" {
					live++
				}
			}
			if live != (window+1)*want {
				t.Errorf("delivered %d reminders, want %d", live, (window+1)*want)
			}
			now = now.Add(16 * time.Minute)
		}
		if size == 2 {
			for range 100 {
				send(agents[0], agents[1])
				send(agents[1], agents[0])
			}
			msgs, err := store.List(beads.ListQuery{Type: "message", TierMode: beads.TierBoth})
			if err != nil || len(msgs) != 204 {
				t.Errorf("flood stored %d total messages, err=%v", len(msgs), err)
			}
			for _, id := range ids {
				history, err := sessionFrontDoor(store).MailWakeHistory(id)
				if err != nil || len(history) > 3 {
					t.Errorf("flood history for %s = %v, %v", id, history, err)
				}
			}
		}
	}
}

type mailWakeReadCounter struct {
	beads.Store
	reads int
}

func (s *mailWakeReadCounter) Get(string) (beads.Bead, error) {
	s.reads++
	return beads.Bead{}, errors.New("unexpected session lookup")
}

func (s *mailWakeReadCounter) List(beads.ListQuery) ([]beads.Bead, error) {
	s.reads++
	return nil, errors.New("unexpected session query")
}

func TestMailWakeDisabled(t *testing.T) {
	withList := &config.City{Agents: []config.Agent{{Name: "reader", MailWakeFrom: []string{"human"}}}}
	for _, command := range []string{"gc mail send", "gc mail reply"} {
		for _, tc := range []struct {
			cfg   *config.City
			store bool
		}{{&config.City{}, true}, {withList, false}, {nil, true}} {
			counter := &mailWakeReadCounter{Store: beads.NewMemStore()}
			var store beads.Store
			if tc.store {
				store = counter
			}
			var stderr bytes.Buffer
			callback := newLocalMailWake("", tc.cfg, store, store, "human", "", command, &stderr, time.Now)
			if callback != nil || counter.reads != 0 || stderr.Len() != 0 {
				t.Fatalf("%s installed a wake or consulted session state with the list unavailable", command)
			}
		}
	}
}

func TestMailWakeSenderIdentity(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")
	store := beads.NewMemStore()
	cfg := &config.City{Agents: []config.Agent{{Name: "source"}, {Name: "stranger"}, {Name: "reader", MailWakeFrom: []string{"source", "human"}}}}
	makeRecord := func(template, alias string) string {
		b, err := store.Create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{
			"template": template, "alias": alias, "state": "asleep", "session_name": alias + "-session",
		}})
		if err != nil {
			t.Fatal(err)
		}
		return b.ID
	}
	listed := makeRecord("source", "author")
	other := makeRecord("stranger", "source")
	reader := makeRecord("reader", "inbox")
	for _, tc := range []struct {
		name, sender, resolved, stored string
		listed                         bool
	}{
		{"resolver", "author", listed, "", true},
		{"mail stamp", "author", "", listed, true},
		{"human", "human", "", "", true},
		{"alias collision", "source", other, "", false},
		{"unresolved", "source", "", "", false},
		{"reserved", "controller", "", listed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			wake := newLocalMailWake(t.TempDir(), cfg, store, store, tc.sender, tc.resolved, "gc mail send", &stderr, time.Now)
			var notified bool
			if wake != nil {
				notified = wake(mail.Message{From: tc.sender, FromSessionID: tc.stored, To: reader, ID: "message"})
			}
			if notified || (stderr.Len() > 0) != tc.listed || strings.Count(stderr.String(), "\n") > 1 {
				t.Fatalf("start disabled: notified=%t stderr=%q, expected diagnostic=%t", notified, stderr.String(), tc.listed)
			}
		})
	}
}

func TestMailWakeExistingNamedSession(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	now := time.Unix(10000, 0).UTC()
	for _, tc := range []struct {
		name, state, reason, hold, history string
		managed, wantWake                  bool
	}{
		{"idle", "asleep", "idle", "", "", true, true},
		{"unmanaged", "asleep", "idle", "", "", false, false},
		{"hold", "asleep", "idle", "future", "", true, false},
		{"draining", "draining", "", "", "", true, false},
		{"unknown sleep", "asleep", "unknown", "", "", true, false},
		{"damaged history", "asleep", "idle", "", "{", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store := openNudgeBeadStore(dir)
			cfg := &config.City{Workspace: config.Workspace{Name: "alpha"}, Agents: []config.Agent{{Name: "reader", Provider: "codex", MailWakeStart: true, MailWakeFrom: []string{"human"}}}, NamedSessions: []config.NamedSession{{Template: "reader"}}}
			b, err := store.Create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{
				"state": tc.state, "sleep_reason": tc.reason, "held_until": tc.hold, "configured_named_session": "true", "configured_named_identity": "reader", "configured_named_mode": "on_demand", "template": "reader", "session_name": "reader", "alias": "reader", "wake_attempts": "2", "churn_count": "3", "wake_refused_event_at": "prior", "mail_wake_log": tc.history,
			}})
			if err != nil {
				t.Fatal(err)
			}
			pokes := 0
			managed, poke := nudgeCityUsesManagedReconciler, nudgePokeController
			nudgeCityUsesManagedReconciler = func(string) bool { return tc.managed }
			nudgePokeController = func(string) error { pokes++; return nil }
			t.Cleanup(func() { nudgeCityUsesManagedReconciler, nudgePokeController = managed, poke })
			var diagnostic bytes.Buffer
			wake := newLocalMailWake(dir, cfg, store.Store, store.Store, "human", "", "gc mail send", &diagnostic, func() time.Time { return now })
			got := notifyStoredMail(mail.Message{ID: "message", From: "human", To: b.ID}, nil, wake, "gc mail send", &diagnostic)
			if got != tc.wantWake || (pokes == 1) != tc.wantWake || strings.Count(diagnostic.String(), "\n") != map[bool]int{true: 0, false: 1}[tc.wantWake] {
				t.Errorf("notified=%t pokes=%d stderr=%q", got, pokes, diagnostic.String())
			}
			after, err := store.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantWake {
				if after.Metadata["wake_request"] != "explicit" || after.Metadata["wake_requested_at"] != now.Format(time.RFC3339) || after.Metadata["mail_wake_log"] == "" {
					t.Errorf("missing wake or history: %v", after.Metadata)
				}
				delete(after.Metadata, "wake_request")
				delete(after.Metadata, "wake_requested_at")
				after.Metadata["mail_wake_log"] = tc.history
			}
			if !maps.Equal(b.Metadata, after.Metadata) {
				t.Errorf("other metadata changed: before %v after %v", b.Metadata, after.Metadata)
			}
			pending, _, _, err := listQueuedNudges(dir, "reader", now)
			if err != nil || (len(pending) == 1) != tc.wantWake {
				t.Errorf("queue=%v err=%v", pending, err)
			}
		})
	}
}
