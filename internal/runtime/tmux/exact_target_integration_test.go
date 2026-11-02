//go:build integration

package tmux

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/test/tmuxtest"
	"github.com/stretchr/testify/require"
)

// A missing session must never resolve to another session with the same prefix.
func TestExactSessionTargetsRealTmux(t *testing.T) {
	for _, operation := range []string{"kill missing", "stop missing", "read environment", "write environment", "send keys", "stop present"} {
		t.Run(operation, func(t *testing.T) {
			guard := tmuxtest.NewGuard(t)
			cfg := DefaultConfig()
			cfg.SocketName = guard.SocketName()
			p := NewProviderWithConfig(cfg)
			tm := p.Tmux()
			t.Logf("private socket: %s", cfg.SocketName)
			if err := tm.NewSessionWithCommandAndEnv("abc-foreign", t.TempDir(), "exec cat", map[string]string{"GC_EXACT_TARGET": "foreign"}); err != nil {
				t.Fatal(err)
			}
			if present, err := tm.HasSession("abc"); err != nil || present {
				t.Fatalf("fixture abc present = %v, error = %v", present, err)
			}
			switch operation {
			case "kill missing":
				if err := tm.KillSession("abc"); !errors.Is(err, ErrSessionNotFound) {
					t.Errorf("KillSession(absent) = %v, want ErrSessionNotFound", err)
				}
			case "stop missing":
				// Provider.Stop retains its idempotent missing-session contract.
				if err := p.Stop("abc"); err != nil {
					t.Errorf("Stop(absent) = %v, want nil", err)
				}
			case "read environment":
				if env, err := p.GetAllEnvironment("abc"); !errors.Is(err, ErrSessionNotFound) || len(env) != 0 {
					t.Errorf("GetAllEnvironment(absent) returned %d variables, error = %v; want ErrSessionNotFound", len(env), err)
				}
			case "write environment":
				if err := tm.SetEnvironment("abc", "GC_EXACT_TARGET", "changed"); !errors.Is(err, ErrSessionNotFound) {
					t.Errorf("SetEnvironment(absent) = %v, want ErrSessionNotFound", err)
				}
				if got, err := tm.GetEnvironment("abc-foreign", "GC_EXACT_TARGET"); err != nil || got != "foreign" {
					t.Errorf("foreign environment = %q, %v; want foreign", got, err)
				}
			case "send keys":
				if err := tm.SendKeysRaw("abc", "C-c"); !errors.Is(err, ErrSessionNotFound) {
					t.Errorf("SendKeysRaw(absent) = %v, want ErrSessionNotFound", err)
				}
				if err := p.SendKeys("abc", "C-c"); err != nil {
					t.Errorf("SendKeys(absent) = %v, want best-effort nil", err)
				}
			case "stop present":
				if err := tm.NewSessionWithCommand("abc", t.TempDir(), "exec cat"); err != nil {
					t.Fatal(err)
				}
				if err := p.Stop("abc"); err != nil {
					t.Errorf("Stop(present) = %v", err)
				}
				if present, err := tm.HasSession("abc"); err != nil || present {
					t.Errorf("stopped abc present = %v, error = %v", present, err)
				}
			}
			if present, err := tm.HasSession("abc-foreign"); err != nil || !present {
				t.Errorf("foreign session survived = %v, error = %v", present, err)
			}
			if dead, err := tm.IsPaneDead("abc-foreign"); err != nil || dead {
				t.Errorf("foreign process dead = %v, error = %v", dead, err)
			}
		})
	}

	// A successful return alone is insufficient: SendKeys and Interrupt are
	// best-effort on absence. Observe delivery to the live pane as well.
	for _, operation := range []string{
		"SendKeysRaw", "SendKeys", "provider SendKeys", "Interrupt", "Peek",
		"NudgeSession", "SetRemainOnExit", "window options", "SetPaneDiedHook",
		"SetAutoRespawnHook", "RespawnPane", "GetSessionCreatedUnix", "IsAttached",
		"paste-buffer", "nudge draft hooks", "capture variants", "missing components",
	} {
		t.Run("live/"+operation, func(t *testing.T) {
			guard := tmuxtest.NewGuard(t)
			cfg := DefaultConfig()
			cfg.SocketName = guard.SocketName()
			p := NewProviderWithConfig(cfg)
			tm := p.Tmux()
			t.Logf("private socket: %s", cfg.SocketName)
			for _, name := range []string{"abc", "abc-foreign"} {
				require.NoError(t, tm.NewSessionWithCommandAndEnv(name, t.TempDir(),
					"printf 'READY\\n'; exec cat", map[string]string{"GC_PROVIDER": "opencode"}))
			}
			// Fixture observations deliberately use a literal qualified target,
			// independent of the production target formatter under test.
			raw := func(args ...string) string {
				t.Helper()
				out, err := tm.run(args...)
				require.NoError(t, err)
				return out
			}
			awaitText := func(target, text string) {
				t.Helper()
				require.Eventually(t, func() bool {
					out, err := tm.run("capture-pane", "-p", "-t", target)
					return err == nil && strings.Contains(out, text)
				}, 3*time.Second, 10*time.Millisecond, "text %q on %s", text, target)
			}
			awaitText("=abc:", "READY")
			awaitText("=abc-foreign:", "READY")
			foreignSnapshot := func() string {
				return raw("capture-pane", "-p", "-t", "=abc-foreign:") +
					raw("show-options", "-t", "=abc-foreign:") +
					raw("show-options", "-w", "-t", "=abc-foreign:") +
					raw("show-options", "-p", "-t", "=abc-foreign:") +
					raw("show-hooks", "-t", "=abc-foreign:") +
					raw("display-message", "-p", "-t", "=abc-foreign:", "#{pane_pid}|#{pane_dead}|#{session_attached}")
			}
			foreignBefore := foreignSnapshot()
			t.Cleanup(func() {
				present, err := tm.HasSession("abc-foreign")
				require.NoError(t, err)
				require.True(t, present, "foreign session survived")
				require.Equal(t, foreignBefore, foreignSnapshot(), "foreign session was untouched")
			})
			switch operation {
			case "SendKeysRaw":
				require.NoError(t, tm.SendKeysRaw("abc", "RAW_MARKER"))
				awaitText("=abc:", "RAW_MARKER")
			case "SendKeys":
				require.NoError(t, tm.SendKeys("abc", "SEND_MARKER"))
				awaitText("=abc:", "SEND_MARKER")
			case "provider SendKeys":
				require.NoError(t, p.SendKeys("abc", "PROVIDER_MARKER"))
				awaitText("=abc:", "PROVIDER_MARKER")
			case "Interrupt":
				raw("set-option", "-t", "=abc:", "remain-on-exit", "on")
				require.NoError(t, p.Interrupt("abc"))
				require.Eventually(t, func() bool {
					out, err := tm.run("display-message", "-p", "-t", "=abc:", "#{pane_dead}")
					return err == nil && out == "1"
				}, 3*time.Second, 10*time.Millisecond, "Interrupt reached the live process")
			case "Peek":
				out, err := p.Peek("abc", 10)
				require.NoError(t, err)
				require.Contains(t, out, "READY")
			case "NudgeSession":
				require.NoError(t, tm.NudgeSession("abc", "NUDGE_MARKER"))
				awaitText("=abc:", "NUDGE_MARKER")
			case "SetRemainOnExit":
				require.NoError(t, tm.SetRemainOnExit("abc", true))
				require.Equal(t, "on", raw("show-options", "-wv", "-t", "=abc:", "remain-on-exit"))
			case "window options":
				raw("set-option", "-t", "=abc:", "mouse", "on")
				raw("set-option", "-wt", "=abc:", "monitor-activity", "on")
				ops := newTmuxStartOps(tm, "", 0, runtime.Config{}, true)
				require.NoError(t, ops.disableMouseAndActivity("abc"))
				require.Equal(t, "off", raw("show-options", "-v", "-t", "=abc:", "mouse"))
				require.Equal(t, "off", raw("show-options", "-wv", "-t", "=abc:", "monitor-activity"))
			case "SetPaneDiedHook":
				require.NoError(t, tm.SetPaneDiedHook("abc", "worker"))
				require.Contains(t, raw("show-hooks", "-w", "-t", "=abc:", "pane-died"), "--session")
			case "SetAutoRespawnHook":
				require.NoError(t, tm.SetAutoRespawnHook("abc"))
				require.Contains(t, raw("show-hooks", "-w", "-t", "=abc:", "pane-died"), "=abc:")
			case "RespawnPane":
				require.NoError(t, tm.RespawnPane("abc", "printf 'RESPAWN_MARKER\\n'; exec cat"))
				awaitText("=abc:", "RESPAWN_MARKER")
			case "GetSessionCreatedUnix":
				created, err := tm.GetSessionCreatedUnix("abc")
				require.NoError(t, err)
				require.Positive(t, created)
			case "IsAttached":
				t.Cleanup(func() { tm.CloseHiddenAttachClient("abc") })
				require.NoError(t, tm.ensureHiddenAttachedClient("abc"))
				require.True(t, p.IsAttached("abc"), "real attached client is visible")
			case "paste-buffer":
				_, err := tm.pasteLiteralText("abc", "PASTE_MARKER")
				require.NoError(t, err)
				awaitText("=abc:", "PASTE_MARKER")
			case "nudge draft hooks":
				pane, err := tm.nudgeDraftOwnershipHook("abc", "abc", true)
				require.NoError(t, err)
				require.NoError(t, tm.stageNudgeDraftProvenance(pane, "DRAFT_MARKER"))
				require.NoError(t, tm.confirmNudgeDraftProvenance(pane, "DRAFT_MARKER"))
				owned, err := tm.sendNudgeDraftSubmitSequence("abc", "DRAFT_MARKER", []string{"x"})
				require.NoError(t, err)
				require.True(t, owned)
				awaitText("=abc:", "x")
				_, err = tm.captureNudgeDraftSubmit("abc", "DRAFT_MARKER")
				require.NoError(t, err)
				_, err = tm.nudgeDraftOwnershipHook("abc", "abc", false)
				require.NoError(t, err)
			case "capture variants":
				for _, capture := range []func(string, int) (string, error){tm.CapturePane, tm.CapturePaneJoined, tm.CapturePaneWithAttributes} {
					out, err := capture("abc", 10)
					require.NoError(t, err)
					require.Contains(t, out, "READY")
				}
				out, err := tm.CapturePaneAll("abc")
				require.NoError(t, err)
				require.Contains(t, out, "READY")
			case "missing components":
				_, err := tm.run("set-option", "-wt", "=abc:99999", "remain-on-exit", "on")
				require.ErrorIs(t, err, ErrWindowNotFound)
				require.NotErrorIs(t, err, ErrSessionNotFound)
				_, err = tm.run("set-option", "-p", "-t", "=abc:^.99999", "@missing", "value")
				require.ErrorIs(t, err, ErrPaneNotFound)
				require.NotErrorIs(t, err, ErrSessionNotFound)
				require.ErrorIs(t, tm.SendKeysRaw("abc:^.99999", "x"), ErrPaneNotFound)
				require.ErrorIs(t, p.SendKeys("abc:^.99999", "x"), ErrPaneNotFound)
				require.ErrorIs(t, p.Interrupt("abc:^.99999"), ErrPaneNotFound)
			}
		})
	}
}
