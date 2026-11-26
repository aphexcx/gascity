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
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
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

func TestMailWakeOnlyLocalCommands(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_CITY", dir)
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_MAIL", "beads")
	cfg := `[workspace]
name = "alpha"
[session]
park_alert_to = "reader"
[[agent]]
name = "gc"
[[agent]]
name = "nudge-dispatcher"
[[agent]]
name = "source"
[[agent]]
name = "reader"
mail_wake_from = ["gc", "nudge-dispatcher", "source"]
mail_wake_start = true
[[named_session]]
template = "reader"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	startSessionResetTestController(t, dir)
	store := openNudgeBeadStore(dir).Store
	ids := make(map[string]string)
	for _, name := range []string{"reader", "gc", "nudge-dispatcher", "source"} {
		b, err := store.Create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{
			"state": "asleep", "sleep_reason": "idle", "session_name": name, "alias": name, "template": name,
			"configured_named_session": "true", "configured_named_identity": name,
		}})
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = b.ID
	}
	mp := beadmail.New(store)
	records := func() map[string]beads.Bead {
		t.Helper()
		result, err := store.List(beads.ListQuery{Type: session.BeadType, TierMode: beads.TierBoth})
		if err != nil {
			t.Fatal(err)
		}
		byID := make(map[string]beads.Bead, len(result))
		for _, record := range result {
			byID[record.ID] = record
		}
		return byID
	}
	before := records()
	var stderr bytes.Buffer
	if err := sendNudgeDeadLetterMail(dir, queuedNudge{ID: "reminder"}, &stderr, false); err != nil {
		t.Fatal(err)
	}
	policy := workStartFailurePolicy{alertTo: "reader", mail: mp}
	policy.sendParkMail(store, "work", "reader", 1, "failure", "stamp", &stderr)
	if _, ok := createHandoffMail(store, store, events.Discard, "source", "reader", nil, "Update", nil, &stderr); !ok {
		t.Fatalf("handoff: %s", stderr.String())
	}
	messages, err := store.List(beads.ListQuery{Type: "message", TierMode: beads.TierBoth})
	if err != nil || len(messages) != 3 || stderr.Len() != 0 {
		t.Fatalf("messages=%d err=%v stderr=%q", len(messages), err, stderr.String())
	}
	if !reflect.DeepEqual(before, records()) {
		t.Fatal("in-process mail changed a session record")
	}
	var broadcast bytes.Buffer
	code := cmdMailSendJSON(nil, false, true, "source", "", "Broadcast", "Update", "", true, &broadcast, &stderr)
	if code != 0 || !reflect.DeepEqual(before, records()) {
		t.Fatalf("broadcast changed a session or failed: code=%d stdout=%q stderr=%q", code, broadcast.String(), stderr.String())
	}
	pending, _, _, err := listQueuedNudges(dir, "reader", time.Now())
	if err != nil || len(pending) != 0 {
		t.Fatalf("in-process mail queued reminders: %v, %v", pending, err)
	}
	original, err := mp.Send("reader", "source", "Question", "Please reply")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_SESSION_ID", ids["source"])
	var out bytes.Buffer
	code = cmdMailReplyJSON([]string{original.ID}, "Answer", "Reply", false, true, &out, &stderr)
	var result mailActionResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || code != 0 || !result.Notified {
		t.Fatalf("local reply: code=%d result=%s stderr=%q err=%v", code, out.String(), stderr.String(), err)
	}
	history, err := sessionFrontDoor(store).MailWakeHistory(ids["reader"])
	if err != nil || len(history) != 1 || history[0].Sender != ids["source"] {
		t.Fatalf("reply wake history=%v err=%v", history, err)
	}
}

func TestMailWakeReplyAfterRecipientRename(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reuse=%t", reuse), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("GC_CITY", dir)
			t.Setenv("GC_SESSION", "fake")
			t.Setenv("GC_BEADS", "file")
			t.Setenv("GC_MAIL", "beads")
			t.Setenv("GC_SESSION_ID", "")
			t.Setenv("GC_ALIAS", "")
			t.Setenv("GC_AGENT", "")
			const cfg = "[workspace]\nname = \"alpha\"\n[[agent]]\nname = \"reader\"\nmail_wake_from = [\"human\"]\nmail_wake_start = true\n[[named_session]]\ntemplate = \"reader\"\n"
			if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(cfg), 0o644); err != nil {
				t.Fatal(err)
			}
			startSessionResetTestController(t, dir)
			store := openNudgeBeadStore(dir).Store
			target, err := store.Create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{
				"state": "asleep", "sleep_reason": "idle", "session_name": "reader", "alias": "first-name", "template": "reader",
				"configured_named_session": "true", "configured_named_identity": "reader",
			}})
			if err != nil {
				t.Fatal(err)
			}
			mp := beadmail.New(store)
			original, err := mp.Send("first-name", "human", "Question", "Please reply")
			if err != nil || original.FromSessionID != target.ID {
				t.Fatalf("original sender=%q err=%v", original.FromSessionID, err)
			}
			if err := store.SetMetadata(target.ID, "alias", "second-name"); err != nil {
				t.Fatal(err)
			}
			var replacement beads.Bead
			if reuse {
				replacement, err = store.Create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{"alias": "first-name", "session_name": "another", "template": "reader"}})
				if err != nil {
					t.Fatal(err)
				}
			}
			var out, diagnostic bytes.Buffer
			code := cmdMailReplyJSON([]string{original.ID}, "Answer", "Reply", false, true, &out, &diagnostic)
			var result mailActionResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil || code != 0 {
				t.Fatalf("code=%d err=%v stderr=%q", code, err, diagnostic.String())
			}
			reply, err := store.Get(result.ID)
			if err != nil || reply.Assignee != target.ID {
				t.Fatalf("stored reply route=%q err=%v", reply.Assignee, err)
			}
			history, err := sessionFrontDoor(store).MailWakeHistory(target.ID)
			if !result.Notified || err != nil || len(history) != 1 {
				t.Errorf("reply notified=%t history=%v err=%v stderr=%q", result.Notified, history, err, diagnostic.String())
			}
			if reuse {
				after, err := store.Get(replacement.ID)
				if err != nil || !reflect.DeepEqual(after, replacement) {
					t.Errorf("new alias holder changed: %v", err)
				}
			}
		})
	}
}

type mailReadOnIdle struct {
	*runtime.Fake
	read func()
}

func (p *mailReadOnIdle) WaitForIdle(ctx context.Context, name string, delay time.Duration) error {
	if err := p.Fake.WaitForIdle(ctx, name, delay); err != nil {
		return err
	}
	p.read()
	return nil
}

func TestMailWakeReadDuringLiveWait(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_CITY", dir)
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_MAIL", "beads")
	const city = `
[workspace]
name = "alpha"
[providers.claude]
base = "builtin:claude"
[[agent]]
name = "reader"
provider = "claude"
mail_wake_from = ["human"]
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(city), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := openNudgeBeadStore(dir).Store
	mp := beadmail.New(store)
	fake := &mailReadOnIdle{Fake: runtime.NewFake()}
	read := false
	fake.read = func() {
		rows, err := store.List(beads.ListQuery{Type: "message", TierMode: beads.TierBoth})
		if err != nil || len(rows) != 1 {
			t.Fatalf("mail at idle: count=%d err=%v", len(rows), err)
		}
		if err := mp.MarkRead(rows[0].ID); err != nil {
			t.Fatal(err)
		}
		read = true
	}
	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return fake, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })
	mgr := newSessionManagerWithConfig(dir, store, fake, cfg)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "reader", Title: "reader", Command: "echo", WorkDir: dir, Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatal(err)
	}
	fake.WaitForIdleErrors[info.SessionName] = nil
	var out, diagnostic bytes.Buffer
	code := cmdMailSendJSON([]string{info.ID}, false, false, "human", "", "Update", "Body", "", true, &out, &diagnostic)
	if code != 0 || !read {
		t.Fatalf("code=%d read=%t stderr=%q", code, read, diagnostic.String())
	}
	for _, call := range fake.Calls {
		if call.Method == "NudgeNow" {
			t.Error("submitted a reminder after its mail was read during the idle wait")
		}
	}
	history, err := sessionFrontDoor(store).MailWakeHistory(info.ID)
	if err != nil || len(history) != 1 {
		t.Errorf("reservation lost on withdrawal: history=%v err=%v", history, err)
	}
	pending, _, _, err := listQueuedNudges(dir, info.SessionName, time.Now())
	if err != nil || len(pending) != 0 {
		t.Errorf("queued a withdrawn reminder: pending=%v err=%v", pending, err)
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

func TestMailWakeBlocksPreserveState(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")
	for _, tc := range []struct{ key, value string }{
		{"held_until", "later"},
		{"quarantined_until", "later"},
		{"wait_hold", "true"},
		{"sleep_intent", "idle-stop-pending"},
		{"state", "suspended"},
		{"state", "drained"},
		{"state", "archived"},
		{"state", "closing"},
		{"sleep_reason", "user-hold"},
		{"sleep_reason", "wait-hold"},
		{"sleep_reason", "quarantine"},
		{"sleep_reason", "context-churn"},
		{"sleep_reason", "rate_limit"},
		{"sleep_reason", "drained"},
		{"agent", "suspended"},
		{"wait", "open"},
	} {
		for _, running := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s=%s/running=%t", tc.key, tc.value, running), func(t *testing.T) {
				dir := t.TempDir()
				store := beads.NewMemStore()
				cfg := &config.City{
					Agents:        []config.Agent{{Name: "reader", Provider: "fake", MailWakeFrom: []string{"human"}, MailWakeStart: true, Suspended: tc.key == "agent"}},
					NamedSessions: []config.NamedSession{{Template: "reader"}},
				}
				meta := map[string]string{
					"state": "asleep", "template": "reader", "alias": "reader", "session_name": "reader",
					"configured_named_session": "true", "configured_named_identity": "reader",
				}
				if tc.key != "agent" && tc.key != "wait" {
					meta[tc.key] = tc.value
				}
				before, err := store.Create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: meta})
				if err != nil {
					t.Fatal(err)
				}
				if tc.key == "wait" {
					_, err := store.Create(beads.Bead{Type: session.WaitBeadType, Labels: []string{session.WaitBeadLabel, "session:" + before.ID}, Metadata: map[string]string{"session_id": before.ID, "state": "pending"}})
					if err != nil {
						t.Fatal(err)
					}
				}
				sp := runtime.NewFake()
				if running {
					if err := sp.Start(context.Background(), "reader", runtime.Config{}); err != nil {
						t.Fatal(err)
					}
				}
				build := buildSessionProviderByName
				buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
					return sp, nil
				}
				t.Cleanup(func() { buildSessionProviderByName = build })
				var stderr bytes.Buffer
				wake := newLocalMailWake(dir, cfg, store, store, beadmail.New(store), "human", "", "gc mail send", &stderr, time.Now)
				if wake(mail.Message{To: before.ID, ID: "message"}) || strings.Count(stderr.String(), "\n") != 1 {
					t.Errorf("blocked mail wake: stderr=%q", stderr.String())
				}
				after, err := store.Get(before.ID)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Errorf("blocked session changed: after=%v err=%v", after, err)
				}
				items, err := store.List(beads.ListQuery{Label: nudgeBeadLabel, TierMode: beads.TierBoth})
				if err != nil || len(items) != 0 {
					t.Errorf("blocked session reminders=%v err=%v", items, err)
				}
			})
		}
	}
}

func TestMailWakeLoop(t *testing.T) {
	for _, size := range []int{2, 3} {
		agents := []string{"first", "second", "third"}[:size]
		now := time.Now().UTC()
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
		var lastID string
		send := func(from, to string) (bool, string) {
			t.Helper()
			m, err := mp.Send(ids[from], ids[to], "Update", "Body")
			if err != nil {
				t.Fatal(err)
			}
			if m.FromSessionID != ids[from] {
				t.Fatalf("returned mail sender session = %q, want %q", m.FromSessionID, ids[from])
			}
			lastID = m.ID
			var diagnostic bytes.Buffer
			wake := newLocalMailWake(dir, cfg, store, store, mp, from, ids[from], "gc mail send", &diagnostic, func() time.Time { return now })
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
			last, err := mp.Get(lastID)
			if err != nil || last.Read {
				t.Fatalf("suppressed hop did not leave unread mail: read=%t err=%v", last.Read, err)
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
			if err := withNudgeQueueState(dir, func(queue *nudgeQueueState) error {
				if len(queue.Pending)+len(queue.InFlight) != 0 {
					t.Errorf("ring stopped with %d pending and %d in-flight reminders", len(queue.Pending), len(queue.InFlight))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
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
			now = now.Add(16 * time.Minute)
			recipient, err := sessionFrontDoor(store).Get(ids[agents[1]])
			if err != nil {
				t.Fatal(err)
			}
			oldPoller := startNudgePoller
			startNudgePoller = func(string, string, string) error { return nil }
			t.Cleanup(func() { startNudgePoller = oldPoller })
			seedCLIStorageRoutes(t, dir, &storageRoutes{stores: map[coordclass.Class]beads.Store{coordclass.ClassNudges: store}, binding: "infra"})
			fake.NudgeErrors = map[string]error{recipient.SessionName: errors.New("live reminder unavailable")}
			for range 3 {
				if asked, diagnostic := send(agents[0], agents[1]); !asked {
					t.Fatalf("busy recipient: %q", diagnostic)
				}
			}
			queueKey := resolveNudgeTargetFromSessionInfo(dir, cfg, recipient).agentKey()
			pending, _, _, err := listQueuedNudges(dir, queueKey, now)
			if err != nil || len(pending) != 3 {
				t.Fatalf("pending mail reminders=%v err=%v", pending, err)
			}
			for _, item := range pending {
				if item.Reference == nil {
					t.Fatal("queued reminder missing mail reference")
				}
				if err := mp.MarkRead(item.Reference.ID); err != nil {
					t.Fatal(err)
				}
				reason, blocked, err := blockedQueuedMailNudgeReason(mp, item)
				if err != nil || !blocked || reason != "mail-already-read" {
					t.Fatalf("withdraw read mail: blocked=%v reason=%q err=%v", blocked, reason, err)
				}
			}
			if err := terminalizeBlockedQueuedNudges(dir, map[string][]queuedNudge{"mail-already-read": pending}); err != nil {
				t.Fatal(err)
			}
			if asked, diagnostic := send(agents[0], agents[1]); asked || !strings.Contains(diagnostic, "budget") {
				t.Fatalf("withdrawn reminders lost budget: asked=%t diagnostic=%q", asked, diagnostic)
			}
			after, err := store.Get(recipient.ID)
			if err != nil || after.Metadata["wake_request"] != "" {
				t.Fatalf("queued live reminders requested a start: %v, %v", after.Metadata, err)
			}
			now = now.Add(16 * time.Minute)
			fake.NudgeErrors[recipient.SessionName] = runtime.ErrNudgeDraftOwnershipLost
			if asked, diagnostic := send(agents[0], agents[1]); asked || !strings.Contains(diagnostic, "delivery") {
				t.Fatalf("failed delivery: asked=%t diagnostic=%q", asked, diagnostic)
			}
			history, err := sessionFrontDoor(store).MailWakeHistory(recipient.ID)
			if err != nil || len(history) != 1 || history[0].Sender != ids[agents[0]] {
				t.Fatalf("missing reservation after delivery failed: %v, %v", history, err)
			}
		}
	}
}

type mailWakeHistoryFailure struct {
	beads.Store
}

func (s mailWakeHistoryFailure) SetMetadata(id, key, value string) error {
	if key == "mail_wake_log" {
		return errors.New("history unavailable")
	}
	return s.Store.SetMetadata(id, key, value)
}

func TestMailWakeHistoryWriteFailure(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_BEADS", "file")
	for _, existing := range []bool{true, false} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			dir := t.TempDir()
			startSessionResetTestController(t, dir)
			store := openNudgeBeadStore(dir).Store
			sessions := mailWakeHistoryFailure{store}
			cfg := &config.City{
				Workspace:     config.Workspace{Name: "alpha"},
				Agents:        []config.Agent{{Name: "reader", Provider: "codex", StartCommand: "echo", MailWakeStart: true, MailWakeFrom: []string{"human"}}},
				NamedSessions: []config.NamedSession{{Template: "reader"}},
			}
			if existing {
				_, err := store.Create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{
					"state": "asleep", "sleep_reason": "idle", "template": "reader", "session_name": "reader", "alias": "reader",
					"configured_named_session": "true", "configured_named_identity": "reader", "configured_named_mode": "on_demand",
				}})
				if err != nil {
					t.Fatal(err)
				}
			}
			mp := beadmail.New(store)
			var stdout, stderr bytes.Buffer
			wake := newLocalMailWake(dir, cfg, store, sessions, mp, "human", "", "gc mail send", &stderr, time.Now)
			code := doMailSendWithWake(mp, events.Discard, nil, "human", []string{"reader", "Update"}, nil, "", true, &stdout, &stderr, wake)
			var result mailActionResult
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if code != 0 || result.Notified == existing || stderr.String() != "gc mail send: reader: history-write\n" {
				t.Fatalf("code=%d notified=%t stderr=%q", code, result.Notified, stderr.String())
			}
			if _, err := mp.Get(result.ID); err != nil {
				t.Fatalf("stored mail: %v", err)
			}
			records, err := store.List(beads.ListQuery{Type: session.BeadType, TierMode: beads.TierBoth})
			if err != nil || len(records) != 1 {
				t.Fatalf("session records=%v err=%v", records, err)
			}
			meta := records[0].Metadata
			if meta["mail_wake_log"] != "" || meta["wake_request"] != "" || (meta["pending_create_claim"] == "true") == existing {
				t.Errorf("session after failed history write: %v", meta)
			}
			pending, _, _, err := listQueuedNudges(dir, "reader", time.Now())
			if err != nil || len(pending) != 0 {
				t.Fatalf("queue=%v err=%v", pending, err)
			}
		})
	}
}

func TestMailWakeHistoryLifetime(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_BEADS", "file")
	now := time.Unix(10000, 0).UTC()
	store := openNudgeBeadStore(dir).Store
	create := func() string {
		t.Helper()
		b, err := store.Create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{"template": "reader", "state": "active"}})
		if err != nil {
			t.Fatal(err)
		}
		return b.ID
	}
	id := create()
	front := sessionFrontDoor(store)
	entry := session.MailWakeEntry{Sender: "sender-session", At: now}
	if err := front.RecordMailWake(id, nil, entry); err != nil {
		t.Fatal(err)
	}
	if err := front.ApplyPatch(id, session.PreWakePatch(session.PreWakePatchInput{Now: now, FreshWake: true})); err != nil {
		t.Fatal(err)
	}
	reopened := sessionFrontDoor(openNudgeBeadStore(dir).Store)
	history, err := reopened.MailWakeHistory(id)
	if err != nil || !slices.Equal(history, []session.MailWakeEntry{entry}) {
		t.Fatalf("history after fresh wake and reopen = %v, %v", history, err)
	}
	history, err = reopened.MailWakeHistory(create())
	if err != nil || len(history) != 0 {
		t.Fatalf("new session history = %v, %v", history, err)
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
			callback := newLocalMailWake("", tc.cfg, store, store, nil, "human", "", command, &stderr, time.Now)
			if callback != nil || counter.reads != 0 || stderr.Len() != 0 {
				t.Fatalf("%s installed a wake or consulted session state with the list unavailable", command)
			}
		}
	}
}

func TestMailWakeGenericRigSender(t *testing.T) {
	dir := t.TempDir()
	const text = `
[workspace]
name = "alpha"
[[rigs]]
name = "demo"
path = "."
[[agent]]
name = "source"
scope = "rig"
[[agent]]
name = "reader"
scope = "rig"
mail_wake_from = ["source"]
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{"template": "demo/source"}})
	if err != nil {
		t.Fatal(err)
	}
	p := localMailWakePolicy{cfg: cfg, front: sessionFrontDoor(store)}
	reader, ok := p.agent(session.Info{Template: "demo/reader"})
	if !ok {
		t.Fatal("missing rig reader")
	}
	identity := p.sender("demo/source", b.ID, "")
	action, _ := decideMailWake(reader, identity, mailWakeFacts{running: true}, nil, nil, time.Now())
	if action != mailWakeRemind {
		t.Fatalf("configured senders %v missed resolved sender %+v", reader.MailWakeFrom, identity)
	}
}

func TestMailWakeSenderIdentity(t *testing.T) {
	t.Setenv("GC_SESSION", "fake")
	store := beads.NewMemStore()
	cfg := &config.City{Agents: []config.Agent{{Name: "source"}, {Name: "stranger"}, {Name: "reader", MailWakeFrom: []string{"source", "human"}}}}
	cfg.Agents[0].MaxActiveSessions = intPtr(2)
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
	poolWorker := makeRecord("source-2", "author-two")
	other := makeRecord("stranger", "source")
	reader := makeRecord("reader", "inbox")
	if err := store.SetMetadata(listed, "alias", "renamed-author"); err != nil {
		t.Fatal(err)
	}
	policy := localMailWakePolicy{cfg: cfg, front: sessionFrontDoor(store)}
	now := time.Now().UTC()
	entry := session.MailWakeEntry{Sender: listed, At: now}
	full := []session.MailWakeEntry{entry, entry, entry}
	for id, want := range map[string]mailWakeAction{listed: mailWakeNone, poolWorker: mailWakeRemind} {
		sender := policy.sender("renamed-author", id, "")
		got, _ := decideMailWake(cfg.Agents[2], sender, mailWakeFacts{recipientID: reader, running: true}, full, nil, now)
		if got != want {
			t.Errorf("session %s after alias change: action=%v want=%v", id, got, want)
		}
	}
	for _, tc := range []struct {
		name, sender, resolved, stored string
		listed                         bool
	}{
		{"resolver", "author", listed, "", true},
		{"mail stamp", "author", "", listed, true},
		{"pool worker", "author-two", poolWorker, "", true},
		{"human", "human", "", "", true},
		{"alias collision", "source", other, "", false},
		{"unresolved", "source", "", "", false},
		{"reserved", "controller", "", listed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			wake := newLocalMailWake(t.TempDir(), cfg, store, store, beadmail.New(store), tc.sender, tc.resolved, "gc mail send", &stderr, time.Now)
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
		{"relocated queue", "asleep", "idle", "", "", true, true},
		{"unmanaged", "asleep", "idle", "", "", false, false},
		{"hold", "asleep", "idle", "future", "", true, false},
		{"draining", "draining", "", "", "", true, false},
		{"closed record", "closed", "", "", "", true, false},
		{"unknown sleep", "asleep", "unknown", "", "", true, false},
		{"city stopped", "asleep", "city-stop", "", "", true, false},
		{"damaged history", "asleep", "idle", "", "{", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store := openNudgeBeadStore(dir)
			var relocated beads.Store
			if tc.name == "relocated queue" {
				relocated = beads.NewMemStore()
				seedCLIStorageRoutes(t, dir, &storageRoutes{stores: map[coordclass.Class]beads.Store{coordclass.ClassNudges: relocated}, binding: "infra"})
			}
			cfg := &config.City{Workspace: config.Workspace{Name: "alpha"}, Agents: []config.Agent{{Name: "reader", Provider: "codex", MailWakeStart: true, MailWakeFrom: []string{"human"}}}, NamedSessions: []config.NamedSession{{Template: "reader"}}}
			b, err := store.Create(beads.Bead{Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{
				"state": tc.state, "sleep_reason": tc.reason, "held_until": tc.hold, "configured_named_session": "true", "configured_named_identity": "reader", "configured_named_mode": "on_demand", "template": "reader", "session_name": "reader", "alias": "reader", "wake_attempts": "2", "churn_count": "3", "wake_refused_event_at": "prior", "mail_wake_log": tc.history,
			}})
			if err != nil {
				t.Fatal(err)
			}
			recipient := b.ID
			if tc.name == "closed record" {
				if err := store.Close(b.ID); err != nil {
					t.Fatal(err)
				}
				b, err = store.Get(b.ID)
				if err != nil {
					t.Fatal(err)
				}
				recipient = "reader"
			}
			pokes := 0
			managed, poke := nudgeCityUsesManagedReconciler, nudgePokeController
			nudgeCityUsesManagedReconciler = func(string) bool { return tc.managed }
			nudgePokeController = func(string) error { pokes++; return nil }
			t.Cleanup(func() { nudgeCityUsesManagedReconciler, nudgePokeController = managed, poke })
			var diagnostic bytes.Buffer
			wake := newLocalMailWake(dir, cfg, store.Store, store.Store, beadmail.New(store.Store), "human", "", "gc mail send", &diagnostic, func() time.Time { return now })
			got := notifyStoredMail(mail.Message{ID: "message", From: "human", To: recipient}, nil, wake, "gc mail send", &diagnostic)
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
			if tc.name == "closed record" {
				active, err := store.List(beads.ListQuery{Status: "open", Type: session.BeadType, TierMode: beads.TierBoth})
				if err != nil || len(active) != 0 || b.Revision != after.Revision || b.Status != after.Status {
					t.Fatalf("closed record changed or new record created: open=%v after=%v err=%v", active, after, err)
				}
			}
			pending, _, _, err := listQueuedNudges(dir, "reader", now)
			if err != nil || (len(pending) == 1) != tc.wantWake {
				t.Errorf("queue=%v err=%v", pending, err)
			}
			if relocated != nil {
				items, err := relocated.List(beads.ListQuery{Label: nudgeBeadLabel, TierMode: beads.TierBoth})
				if err != nil || len(items) != 1 {
					t.Errorf("reminder beads on the nudges binding = %v, %v", items, err)
				}
			}
		})
	}
}
