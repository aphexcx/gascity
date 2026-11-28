//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
	"github.com/gastownhall/gascity/test/tmuxtest"
	"github.com/stretchr/testify/require"
)

// This opt-in test observes submissions independently through Claude Code's
// UserPromptSubmit hook. Each case owns its city, hook files and tmux server.
func TestLateClaudePasteQueue(t *testing.T) {
	project := os.Getenv("GC_TEST_LIVE_CLAUDE_DIR")
	if project == "" {
		t.Skip("set GC_TEST_LIVE_CLAUDE_DIR to a trusted, empty scratch project")
	}
	// TestMain installs provider stubs on PATH. An explicit binary keeps
	// this opt-in check on the real client without changing other tests.
	require.True(t, filepath.IsAbs(os.Getenv("GC_TEST_LIVE_CLAUDE_BIN")), "set GC_TEST_LIVE_CLAUDE_BIN to the installed Claude Code executable")
	for _, attached := range []bool{false, true} {
		t.Run(fmt.Sprintf("attach=%t", attached), func(t *testing.T) {
			runLateClaudePasteQueue(t, project, attached)
		})
	}
}

func runLateClaudePasteQueue(t *testing.T, project string, attach bool) {
	dir := t.TempDir()
	t.Setenv("GC_CITY", dir)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_MAIL", "")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname='hold-test'\n[beads]\nprovider='file'\n[session]\npark_alert_to='operator'\n"), 0o600))
	store, err := openCityStoreAt(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = closeBeadStoreHandle(store) })
	const name = "scratch"
	sessionKey, err := session.GenerateSessionKey()
	require.NoError(t, err)
	project, err = filepath.EvalSymlinks(project)
	require.NoError(t, err)
	b, err := store.Create(beads.Bead{
		Type: session.BeadType, Labels: []string{session.LabelSession},
		Metadata: map[string]string{"session_name": name, "session_key": sessionKey, "work_dir": project, "template": name, "provider": "claude", "continuation_epoch": "1"},
	})
	require.NoError(t, err)
	target := nudgeTarget{cityPath: dir, agent: config.Agent{Name: name}, resolved: &config.ResolvedProvider{Name: "claude"}, sessionName: name, sessionID: b.ID, continuationEpoch: "1"}
	status := func(stage string) nudgeStatusJSON {
		t.Helper()
		var out, diag bytes.Buffer
		require.Equal(t, 0, cmdNudgeStatus([]string{b.ID}, true, &out, &diag), diag.String())
		var result nudgeStatusJSON
		require.NoError(t, json.Unmarshal(out.Bytes(), &result))
		t.Logf("queue %s: %s", stage, &out)
		return result
	}
	guard := tmuxtest.NewGuard(t)
	cfg := tmux.DefaultConfig()
	cfg.SocketName = guard.SocketName()
	client, provider := tmux.NewTmuxWithConfig(cfg), tmux.NewProviderWithConfig(cfg)
	settings := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"cd \"$GC_TEST_HOOK_DIR\" && touch started && sleep 18 && touch finished"}]}],"UserPromptSubmit":[{"hooks":[{"type":"command","command":"cd \"$GC_TEST_HOOK_DIR\" && jq -c '.prompt' >> turns.jsonl"}]}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0o600))
	// tmux resumes a stopped direct child. Keep a shell as the pane process
	// so the test can pause its foreground Claude child until the first receipt.
	command := `env -u NO_COLOR "$GC_TEST_LIVE_CLAUDE_BIN" --session-id "$GC_SESSION_KEY" --model haiku --tools '' --strict-mcp-config --setting-sources '' --settings "$GC_TEST_HOOK_DIR/settings.json"; result=$?; exit "$result"`
	run := func(args ...string) string {
		t.Helper()
		out := runExternalOutput(t, dir, "tmux", append([]string{"-L", cfg.SocketName}, args...)...)
		return strings.TrimSpace(string(out))
	}
	start := time.Now()
	run("new-session", "-d", "-s", name, "-c", project, "-x", "139", "-y", "57",
		"-e", "GC_PROVIDER=claude", "-e", "GC_DIR="+project, "-e", "GC_SESSION_KEY="+sessionKey, "-e", "HOME="+os.Getenv("HOME"),
		"-e", "CLAUDE_CONFIG_DIR="+os.Getenv("CLAUDE_CONFIG_DIR"), "-e", "GC_TEST_HOOK_DIR="+dir, command)
	t.Cleanup(func() { require.NoError(t, client.KillSession(name)) })
	capture := func(stage string) string {
		t.Helper()
		pane, err := client.CapturePane(name, 57)
		require.NoError(t, err)
		t.Logf("pane %s at %s\n%s", stage, time.Since(start).Round(time.Millisecond), strings.TrimSpace(pane))
		return pane
	}
	defer capture("final")
	waitUntil := func(at time.Time) {
		t.Helper()
		select {
		case <-time.After(time.Until(at)):
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
	first := "Reply with the single word OK.\n" + strings.Repeat("Synthetic startup reminder.\n", 20) + "End of first reminder."
	require.NoError(t, client.WaitForIdle(t.Context(), name, 20*time.Second))
	waitUntil(start.Add(3150 * time.Millisecond))
	require.FileExists(t, filepath.Join(dir, "started"))
	require.NoFileExists(t, filepath.Join(dir, "finished"))
	// The hook can render early on a warm client. Pause only this private
	// session's foreground process to put the render outside the first call.
	parent, err := client.GetPanePID(name)
	require.NoError(t, err)
	tty := run("display-message", "-p", "-t", name, "#{pane_tty}")
	processes := runExternalOutput(t, dir, "ps", "-t", tty, "-o", "pid=,ppid=")
	var children []string
	for line := range strings.SplitSeq(string(processes), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[1] == parent {
			children = append(children, fields[0])
		}
	}
	require.Len(t, children, 1, "expected the scratch pane shell's one foreground child")
	pid, err := strconv.Atoi(children[0])
	require.NoError(t, err)
	require.Greater(t, pid, 1)
	scratch, err := os.FindProcess(pid)
	require.NoError(t, err)
	require.NoError(t, scratch.Signal(syscall.SIGSTOP))
	paused := time.Now()
	defer func() {
		if !paused.IsZero() {
			_ = scratch.Signal(syscall.SIGCONT)
		}
	}()
	t.Logf("controlled render paused at %s (+%s)", paused.UTC().Format(time.RFC3339Nano), paused.Sub(start))
	firstAt := time.Now()
	d, err := provider.NudgeNowDelivered(name, runtime.TextContent(first))
	require.NoError(t, scratch.Signal(syscall.SIGCONT))
	resumed := time.Now()
	paused = time.Time{}
	t.Logf("controlled render resumed at %s (+%s)", resumed.UTC().Format(time.RFC3339Nano), resumed.Sub(start))
	t.Logf("first call at %s, returned at %s, submit=%s", firstAt.Sub(start), time.Since(start), d.Submit)
	require.NoError(t, err)
	require.Equal(t, runtime.NudgeSubmitUnconfirmed, d.Submit, "the first paste must exercise the late-render path")
	require.Equal(t, runtime.NudgePayloadDigest(first), run("show-options", "-p", "-qv", "-t", name, "@gc-nudge-draft-payload"))
	require.True(t, strings.HasSuffix(run("show-options", "-p", "-qv", "-t", name, "@gc-nudge-draft-shape"), ":"))
	capture("first-unconfirmed")
	if attach {
		// A control-mode client genuinely attaches and detaches without
		// sending a key to the composer.
		run("-C", "attach-session", "-t", name, ";", "detach-client")
		require.Eventually(t, func() bool {
			return run("show-options", "-p", "-qv", "-t", name, "@gc-nudge-draft-payload") == ""
		}, 5*time.Second, 50*time.Millisecond)
	}
	waitUntil(firstAt.Add(20 * time.Second))
	require.Eventually(t, func() bool {
		pane, err := client.CapturePane(name, 57)
		return err == nil && strings.Contains(pane, "[Pasted text #")
	}, 30*time.Second, 100*time.Millisecond)
	capture("late-placeholder")
	t.Logf("second delivery begins at %s (+%s)", time.Now().UTC().Format(time.RFC3339Nano), time.Since(start))
	var messages []string
	for i := 1; i <= 5; i++ {
		message := fmt.Sprintf("Synthetic follow-up %d. Reply OK.\n", i) + strings.Repeat("More detail.\n", i)
		messages = append(messages, message)
		item := newQueuedNudgeWithOptions(name, message, "test", time.Now(), queuedNudgeOptions{SessionID: b.ID, SessionName: name, ContinuationEpoch: "1"})
		require.NoError(t, enqueueQueuedNudgeWithStore(dir, beads.NudgesStore{Store: store}, item))
		if !attach {
			status(fmt.Sprintf("before-follow-up-%d", i))
			landed, err := tryDeliverQueuedNudgesByPoller(target, store, store, provider, 0, worker.LiveObservation{Running: true})
			require.NoError(t, err)
			require.True(t, landed)
		}
	}
	status("before-dispatch")
	mailbox := beadmail.New(store)
	var nudgeErr error
	require.Eventually(t, func() bool {
		_, nudgeErr = tryDeliverQueuedNudgesByPoller(target, store, store, provider, 0, worker.LiveObservation{Running: true})
		current, err := readNudgeStatus(dir, target, time.Now(), clock.Real{})
		require.NoError(t, err)
		require.Empty(t, current.Dead, "last delivery error: %v", nudgeErr)
		if attach {
			return current.DraftHold != nil && !current.DraftHold.NotifiedAt.IsZero()
		}
		return len(current.Pending)+len(current.InFlight) == 0
	}, 3*time.Minute, time.Second, "last delivery: %v", nudgeErr)
	final := status("after-dispatch")
	letters, err := mailbox.Inbox("operator")
	require.NoError(t, err)
	t.Logf("final mailbox contains %d notices", len(letters))
	turns, err := os.ReadFile(filepath.Join(dir, "turns.jsonl"))
	if attach {
		require.True(t, errors.Is(err, os.ErrNotExist) || (err == nil && len(turns) == 0))
		require.Contains(t, capture("held"), "[Pasted text #")
		require.Equal(t, nudgeStatusCounts{Pending: 5}, final.Counts)
		require.Len(t, letters, 1)
		require.Contains(t, letters[0].Subject, "NUDGE HOLD")
		return
	}
	require.NoError(t, err)
	require.LessOrEqual(t, len(letters), 1)
	decoder := json.NewDecoder(bytes.NewReader(turns))
	var prompts []string
	for {
		var prompt string
		if err := decoder.Decode(&prompt); errors.Is(err, io.EOF) {
			break
		} else {
			require.NoError(t, err)
		}
		prompts = append(prompts, prompt)
	}
	require.NotEmpty(t, prompts)
	require.Contains(t, prompts[0], first)
	for _, text := range append([]string{first}, messages...) {
		require.Equal(t, 1, strings.Count(strings.Join(prompts, "\n"), strings.TrimSpace(text)))
	}
}
