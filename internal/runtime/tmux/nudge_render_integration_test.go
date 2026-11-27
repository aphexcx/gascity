//go:build integration

package tmux

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/stretchr/testify/require"
)

type renderTrace struct {
	realExecutor
	t       *testing.T
	start   time.Time
	message string
	seen    bool
	enters  int
}

func (e *renderTrace) executeCtx(ctx context.Context, args []string) (string, error) {
	out, err := e.realExecutor.executeCtx(ctx, args)
	if slices.Contains(args, "capture-pane") && slices.Contains(args, "-J") {
		draft, observed := nudgeComposer(out, DefaultReadyPromptPrefix)
		if observed && !e.seen && (draft == e.message || collapsedNudgeDraftMatches(draft, e.message)) {
			e.seen = true
			e.t.Logf("composer at %s: %q", time.Since(e.start).Round(time.Millisecond), draft)
		}
	}
	if slices.Contains(args, "send-keys") && slices.Contains(args, "Enter") ||
		slices.Contains(args, "if-shell") && strings.Contains(strings.Join(args, " "), " Enter") {
		e.enters++
		if !e.seen {
			e.t.Error("Enter before the paste rendered")
		}
		e.t.Logf("Enter at %s: %q err=%v", time.Since(e.start).Round(time.Millisecond), out, err)
	}
	return out, err
}

// This opt-in test uses an authenticated Claude Code installation. Its
// UserPromptSubmit hook witnesses the turn independently of tmux's receipt.
func TestNudgeDuringClaudeSessionStart(t *testing.T) {
	project := os.Getenv("GC_TEST_LIVE_CLAUDE_DIR")
	if project == "" {
		t.Skip("set GC_TEST_LIVE_CLAUDE_DIR to a trusted, empty scratch project to run with local Claude Code")
	}
	message := "Reply with OK." + strings.Repeat("\nThis is synthetic test input.", 21)
	delays := []time.Duration{700 * time.Millisecond, 3300 * time.Millisecond, 6 * time.Second}
	held := 0
	for _, delay := range delays {
		t.Run(delay.String(), func(t *testing.T) {
			dir := t.TempDir()
			const settings = `{"hooks":{
				"SessionStart":[{"hooks":[{"type":"command","command":"cd \"$GC_TEST_HOOK_DIR\" && touch hook-started && sleep 18 && touch hook-finished && printf '%s' '{\"hookSpecificOutput\":{\"hookEventName\":\"SessionStart\",\"additionalContext\":\"Synthetic startup complete.\"}}'"}]}],
				"UserPromptSubmit":[{"hooks":[{"type":"command","command":"cd \"$GC_TEST_HOOK_DIR\" && cat > turn.tmp && mv turn.tmp turn.json"}]}]
			}}`
			if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := DefaultConfig()
			cfg.SocketName = privateSocketName("render")
			tm := NewTmuxWithConfig(cfg)
			const session = "scratch"
			const command = `exec env -u NO_COLOR claude --model haiku --tools '' --strict-mcp-config --setting-sources '' --settings "$GC_TEST_HOOK_DIR/settings.json"`
			start := time.Now()
			trace := &renderTrace{t: t, start: start, message: message}
			tm.exec = trace
			if _, err := tm.run("new-session", "-d", "-s", session, "-c", project, "-x", "139", "-y", "57",
				"-e", "GC_PROVIDER=claude", "-e", "GC_TEST_HOOK_DIR="+dir, command); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := tm.KillServer(); err != nil {
					t.Errorf("stopping private socket %s: %v", cfg.SocketName, err)
				}
			})
			snapshot := func(stage string) {
				t.Helper()
				content, err := tm.CapturePane(session, 57)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("%s at %s\n%s", stage, time.Since(start).Round(time.Millisecond), strings.TrimSpace(content))
			}
			// Begin the two-poll idle check on the first prompt frame. This
			// keeps its polling interval from delaying the earliest sample.
			require.Eventually(t, func() bool {
				ready, err := tm.snapshotPaneIdleWithPrefix(session, DefaultReadyPromptPrefix)
				return err == nil && ready
			}, 30*time.Second, 10*time.Millisecond)
			if err := tm.WaitForIdle(t.Context(), session, 30*time.Second); err != nil {
				snapshot("prompt wait ended")
				t.Fatal(err)
			}
			d, err := tm.nudgeSessionDeliveryWith(session, message, func(target, text string, timeout time.Duration) (int, error) {
				// Schedule the input at the specified process age independently
				// of the time the provider's readiness and ownership checks took.
				select {
				case <-time.After(time.Until(start.Add(delay))):
				case <-t.Context().Done():
					return 0, t.Context().Err()
				}
				if _, err := os.Stat(filepath.Join(dir, "hook-finished")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("startup hook already finished or could not be checked at paste: %v", err)
				}
				n, err := tm.sendKeysLiteralWithRetry(target, text, timeout)
				age := time.Since(start)
				t.Logf("pasted %d bytes at %s", n, age.Round(time.Millisecond))
				if age > delay+250*time.Millisecond {
					t.Fatalf("missed the %s paste window: %s", delay, age)
				}
				snapshot("after paste")
				return n, err
			})
			snapshot("delivery returned")
			t.Logf("submit=%s Enters=%d err=%v", d.Submit, trace.enters, err)
			if !d.Delivered || d.Bytes != len(message) {
				t.Fatalf("incomplete delivery: %+v err=%v", d, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "hook-started")); err != nil {
				t.Fatalf("SessionStart hook: %v", err)
			}
			if !trace.seen {
				if err != nil || d.Submit != runtime.NudgeSubmitUnconfirmed || trace.enters != 0 {
					t.Fatalf("unseen paste: delivery=%+v err=%v Enters=%d", d, err, trace.enters)
				}
				mark, err := tm.run("show-options", "-p", "-qv", "-t", exactPaneTarget(session), nudgeDraftPayloadOption)
				if err != nil || mark != runtime.NudgePayloadDigest(message) {
					t.Fatalf("unseen paste lost ownership: mark=%q err=%v", mark, err)
				}
				t.Log("ownership retained for the late paste")
				held++
				return
			}
			if d.Submit != runtime.NudgeSubmitConfirmed || trace.enters == 0 || (err != nil && !errors.Is(err, ErrNudgeSubmitDeliveredUnobserved)) {
				t.Fatalf("rendered paste: delivery=%+v err=%v Enters=%d", d, err, trace.enters)
			}
			turnFile := filepath.Join(dir, "turn.json")
			require.Eventually(t, func() bool {
				_, err := os.Stat(turnFile)
				return !errors.Is(err, os.ErrNotExist)
			}, 30*time.Second, 200*time.Millisecond, "UserPromptSubmit hook")
			raw, err := os.ReadFile(turnFile)
			require.NoError(t, err)
			var turn struct{ Prompt string }
			require.NoError(t, json.Unmarshal(raw, &turn))
			// Claude can wrap a multiline paste in a text block in the hook.
			if strings.Count(turn.Prompt, message) != 1 {
				t.Fatal("UserPromptSubmit did not contain the complete paste exactly once")
			}
			t.Logf("UserPromptSubmit witnessed the complete paste at %s", time.Since(start).Round(time.Millisecond))
		})
	}
	if held == len(delays) {
		t.Fatal("no live sample witnessed a rendered paste start a turn")
	}
}
