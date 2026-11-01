//go:build integration

package tmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/testutil"
)

// TestNudgeRecoversStagedDraftSwallowedByPasteIngest reproduces the staged-draft
// stall against a REAL, isolated tmux server (its own -L socket, killed on
// cleanup). A codex-shaped fake TUI collapses the pasted nudge into
// "[Pasted Content N chars]" and eats the first few submits, the way a loaded
// codex TUI still ingesting a large paste does. Before the fix every submit in
// the ~2.4s confirm window was eaten, NudgeSession gave up unconfirmed, and the
// draft stayed staged forever. The fake TUI logs every submit it takes, so each
// case can assert both the verdict and that the message was taken exactly once.
func TestNudgeRecoversStagedDraftSwallowedByPasteIngest(t *testing.T) {
	if os.Getenv("GC_TMUX_INTEGRATION") != "1" {
		t.Skip("set GC_TMUX_INTEGRATION=1 to run this real-tmux reproduction (spins a throwaway tmux server)")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 required for the fake TUI")
	}
	script, err := filepath.Abs(filepath.Join("testdata", "nudge_faketui_codex.py"))
	if err != nil {
		t.Fatalf("resolving fake TUI: %v", err)
	}

	cleanup := func(t *testing.T, tm *Tmux) {
		t.Helper()
		t.Logf("private tmux socket: %s", tm.cfg.SocketName)
		t.Cleanup(func() {
			if err := tm.KillServer(); err != nil {
				t.Errorf("stopping private tmux server: %v", err)
				return
			}
			if _, err := tm.run("list-sessions"); !errors.Is(err, ErrNoServer) {
				t.Errorf("private tmux server survived cleanup: %v", err)
				return
			}
			t.Logf("tmux -L %s list-sessions: no server running", tm.cfg.SocketName)
			if err := os.Remove(namedSocketPath(tm.cfg.SocketName)); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("removing private tmux socket: %v", err)
			}
		})
	}

	start := func(t *testing.T, socket string, swallow int, busyDelay time.Duration) (*Tmux, string) {
		t.Helper()
		logPath := filepath.Join(t.TempDir(), "tui.log")
		tm := NewTmuxWithConfig(Config{
			SocketName:        socket,
			NudgeReadyTimeout: 10 * time.Second,
			NudgeLockTimeout:  30 * time.Second,
		})
		_, _ = tm.run("kill-server")
		cleanup(t, tm)
		cmd := fmt.Sprintf("python3 %s %s %d %.3f 30", shellQuote(script), shellQuote(logPath), swallow, busyDelay.Seconds())
		if _, err := tm.run("new-session", "-d", "-s", "draft", "-x", "120", "-y", "30", cmd); err != nil {
			t.Skipf("cannot create tmux session (tmux unavailable?): %v", err)
		}
		// GC_PROVIDER=codex routes NudgeSession through the verified submit
		// path, codex's Escape+Enter submit sequence, and its draft marker.
		if err := tm.SetEnvironment("draft", "GC_PROVIDER", "codex"); err != nil {
			t.Fatalf("SetEnvironment: %v", err)
		}
		// Let the TUI paint its first frame before any keystroke.
		poll := time.NewTicker(100 * time.Millisecond)
		defer poll.Stop()
		deadline := time.After(5 * time.Second)
		for {
			if out, err := tm.CapturePane("draft", 40); err == nil && strings.Contains(out, "fake-codex-tui") {
				return tm, logPath
			}
			select {
			case <-poll.C:
			case <-deadline:
				t.Fatal("fake TUI never painted")
				return nil, ""
			}
		}
	}

	count := func(t *testing.T, logPath, event string) int {
		t.Helper()
		raw, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("reading TUI log: %v", err)
		}
		return strings.Count(string(raw), "\t"+event+"\t")
	}

	largeNudge := "gc-staged-draft-probe " + strings.Repeat("work the claimed bead ", 60)

	t.Run("submits eaten past the confirm window: recovered, submitted once", func(t *testing.T) {
		const swallow = submitEnterMaxSends + 2
		tm, logPath := start(t, "gcstageddraftbusy", swallow, 100*time.Millisecond)

		if err := tm.NudgeSession("draft", largeNudge); err != nil {
			t.Fatalf("NudgeSession = %v, want nil (recovery should land the staged draft)", err)
		}
		if got := count(t, logPath, "SUBMIT"); got != 1 {
			t.Fatalf("TUI submits = %d, want exactly 1", got)
		}
		if got := count(t, logPath, "SWALLOWED"); got != swallow {
			t.Fatalf("TUI swallowed submits = %d, want %d", got, swallow)
		}
	})

	t.Run("draft clears but busy never renders: delivered-unobserved, submitted once", func(t *testing.T) {
		tm, logPath := start(t, "gcstageddraftclear", submitEnterMaxSends+1, 60*time.Second)

		began := time.Now()
		err := tm.NudgeSession("draft", largeNudge)
		t.Logf("NudgeSession returned after %v: %v", time.Since(began), err)
		if !errors.Is(err, ErrNudgeSubmitDeliveredUnobserved) {
			t.Fatalf("err = %v, want ErrNudgeSubmitDeliveredUnobserved", err)
		}
		if got := count(t, logPath, "SUBMIT"); got != 1 {
			t.Fatalf("TUI submits = %d, want exactly 1", got)
		}
	})

	// CONTROL: the same eaten submits, but the draft is short enough to show
	// inline, so no staged-draft marker is on screen. Recovery must not fire:
	// exactly the ordinary window's submits reach the pane and the verdict
	// stays unconfirmed, as before the fix.
	t.Run("no staged-draft marker: no extra submits", func(t *testing.T) {
		tm, logPath := start(t, "gcstageddraftnone", submitEnterMaxSends+2, 100*time.Millisecond)

		err := tm.NudgeSession("draft", "gc-staged-draft-short-probe")
		if !errors.Is(err, ErrNudgeSubmitUnconfirmed) {
			t.Fatalf("err = %v, want ErrNudgeSubmitUnconfirmed", err)
		}
		if got := count(t, logPath, "SWALLOWED"); got != submitEnterMaxSends {
			t.Fatalf("TUI saw %d submits, want exactly the ordinary window's %d", got, submitEnterMaxSends)
		}
		if got := count(t, logPath, "SUBMIT"); got != 0 {
			t.Fatalf("TUI submits = %d, want 0", got)
		}
	})

	for _, ownership := range []struct {
		edited, removed, global, switched, collapsed bool
		timing                                       string
	}{
		{},
		{edited: true},
		{edited: true, global: true},
		{removed: true},
		{edited: true, switched: true},
		{timing: "late-confirmed"},
		{timing: "before-confirmation"},
		{timing: "before-observation"},
		{edited: true, switched: true, timing: "blocking"},
		{collapsed: true},
		{collapsed: true, removed: true},
	} {
		edited := ownership.edited
		t.Run(fmt.Sprintf("wrapped Claude draft at 80 columns: edited=%t hook-removed=%t global=%t switched=%t collapsed=%t timing=%s", edited, ownership.removed, ownership.global, ownership.switched, ownership.collapsed, ownership.timing), func(t *testing.T) {
			claudeScript, err := filepath.Abs(filepath.Join("testdata", "nudge_faketui.py"))
			if err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(t.TempDir(), "tui.log")
			tm := NewTmuxWithConfig(Config{
				SocketName:        privateSocketName("wd"),
				NudgeReadyTimeout: 10 * time.Second,
				NudgeLockTimeout:  30 * time.Second,
			})
			cleanup(t, tm)
			mode := "wrap-retry"
			if ownership.collapsed {
				mode = "collapsed-retry"
			}
			command := fmt.Sprintf("python3 %s %s 0.1 30 %s %d", shellQuote(claudeScript), shellQuote(logPath), mode, submitEnterMaxSends)
			if _, err := tm.run("new-session", "-d", "-s", "wrapped", "-x", "80", "-y", "30", command); err != nil {
				t.Fatalf("creating private 80-column pane: %v", err)
			}
			if err := tm.SetEnvironment("wrapped", "GC_PROVIDER", "claude"); err != nil {
				t.Fatal(err)
			}
			if edited {
				args := []string{"set-hook", "-t", "wrapped"}
				if ownership.global {
					args = []string{"set-hook", "-g"}
				}
				event := "client-attached"
				if ownership.switched {
					event = "client-session-changed"
				}
				command := "set-option -t wrapped @gc-test-user-hook ran"
				if ownership.timing == "blocking" {
					command += " ; run-shell 'sleep 10'"
				}
				if _, err := tm.run(append(args, event, command)...); err != nil {
					t.Fatal(err)
				}
			}
			poll := time.NewTicker(50 * time.Millisecond)
			defer poll.Stop()
			deadline := time.After(testutil.ExecRaceTimeout)
			for {
				if pane, err := tm.CapturePane("wrapped", 40); err == nil && strings.Contains(pane, "fake-claude-tui") {
					break
				}
				select {
				case <-poll.C:
				case <-deadline:
					t.Fatal("wrapped Claude TUI never painted")
				}
			}
			width, err := tm.run("display-message", "-p", "-t", "wrapped", "#{pane_width}")
			if err != nil || strings.TrimSpace(width) != "80" {
				t.Fatalf("pane width=%q err=%v, want 80", width, err)
			}
			// Include an unbroken token so a wrap inside a word is covered too.
			message := "long founder line " + strings.Repeat("abcdefghij", 20) + " with final words"
			if edited {
				message = strings.Repeat("a", 76) + " b"
				if ownership.timing == "blocking" {
					// The hard-wrap matcher trims both spaces at column 78,
					// accepting the edited 76-cell row followed by a newline.
					// That makes stale provenance observable during the hook.
					message = strings.Repeat("a", 76) + "  b"
				}
			} else if ownership.collapsed {
				message = strings.Repeat("founder line ", 80) + "end"
			}
			if err := tm.NudgeSession("wrapped", message); !errors.Is(err, ErrNudgeSubmitUnconfirmed) {
				t.Fatalf("initial submit=%v, want an unconfirmed preserved draft", err)
			}
			pane, err := tm.CapturePane("wrapped", 40)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("80-column pane after unconfirmed submit:\n%s", pane)
			draft, observed := nudgeComposer(pane, "❯ ")
			if ownership.collapsed {
				if !observed || draft != "[Pasted text #1]" {
					t.Fatalf("draft=%q observed=%v; expected collapsed paste", draft, observed)
				}
			} else if !observed || !strings.Contains(draft, "\n") || draft == message {
				t.Fatalf("draft=%q observed=%v; expected real wrapped rows", draft, observed)
			}
			stored, err := tm.run("show-options", "-p", "-qv", "-t", "wrapped", "@gc-nudge-draft-payload")
			if err != nil || strings.TrimSpace(stored) != runtime.NudgePayloadDigest(message) {
				t.Fatalf("stored payload digest=%q err=%v, want full-paste provenance", stored, err)
			}
			attachAndEdit := func() {
				attachedSession := "wrapped"
				if ownership.switched {
					attachedSession = "other"
					if _, err := tm.run("new-session", "-d", "-s", attachedSession); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				cmd := exec.CommandContext(ctx, "script", hiddenAttachScriptArgs(goruntime.GOOS, []string{"-L", tm.cfg.SocketName, "attach-session", "-t", attachedSession})...)
				cmd.Env = append(cmd.Environ(), "TERM=xterm-256color")
				cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
				cmd.WaitDelay = testutil.ExecRaceTimeout
				stdin, err := cmd.StdinPipe()
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				if err := cmd.Start(); err != nil {
					_ = stdin.Close()
					cancel()
					t.Fatal(err)
				}
				done := make(chan struct{})
				go func() { _ = cmd.Wait(); close(done) }()
				t.Cleanup(func() { _ = stdin.Close(); cancel(); <-done })
				deadline := time.After(testutil.ExecRaceTimeout)
				for !tm.IsSessionAttached(attachedSession) {
					select {
					case <-poll.C:
					case <-deadline:
						t.Fatal("human client never attached")
					}
				}
				client, err := tm.run("list-clients", "-F", "#{client_name}")
				if err != nil {
					t.Fatal(err)
				}
				if ownership.switched {
					if _, err := tm.run("switch-client", "-c", strings.TrimSpace(client), "-t", "wrapped"); err != nil {
						t.Fatal(err)
					}
				}
				var editKeys [][]string
				if edited {
					editKeys = [][]string{{"C-u"}, {"-l", strings.Repeat("a", 76)}, {"C-v"}, {"-l", "b"}}
				}
				for _, keys := range editKeys {
					if _, err := tm.run(append([]string{"send-keys", "-t", "wrapped"}, keys...)...); err != nil {
						t.Fatal(err)
					}
				}
				deadline = time.After(testutil.ExecRaceTimeout)
				for edited {
					pane, err := tm.CapturePane("wrapped", 40)
					got, ok := nudgeComposer(pane, "❯ ")
					if err == nil && ok && got == strings.Repeat("a", 76)+"\nb" {
						break
					}
					select {
					case <-poll.C:
					case <-deadline:
						t.Fatalf("edited draft never rendered: %q, %v", got, err)
					}
				}
				if ownership.switched {
					if _, err := tm.run("switch-client", "-c", strings.TrimSpace(client), "-t", attachedSession); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := tm.run("detach-client", "-s", attachedSession); err != nil {
					t.Fatal(err)
				}
				select {
				case <-done:
				case <-time.After(testutil.ExecRaceTimeout):
					t.Fatal("human client never detached")
				}
				if edited {
					if value, err := tm.run("show-options", "-qv", "-t", "wrapped", "@gc-test-user-hook"); err != nil || strings.TrimSpace(value) != "ran" {
						t.Fatalf("existing attach hook did not run: %q, %v", value, err)
					}
				}
			}
			if edited {
				attachAndEdit()
			}
			if ownership.removed {
				if _, err := tm.run("set-hook", "-u", "-t", "wrapped", "client-attached"); err != nil {
					t.Fatal(err)
				}
			}
			// The pane owns the evidence, so retry works with a new transport too.
			tm = NewTmuxWithConfig(tm.cfg)
			if ownership.timing == "late-confirmed" || ownership.timing == "before-confirmation" || ownership.timing == "before-observation" {
				real := tm.exec
				attached := false
				entered := false
				tm.exec = &fakeExecutor{fn: func(args []string) (string, error) {
					capture := slices.Contains(args, "capture-pane") || (slices.Contains(args, "if-shell") && strings.HasPrefix(args[len(args)-2], "capture-pane "))
					send := slices.Contains(args, "if-shell") && strings.HasPrefix(args[len(args)-2], "send-keys ")
					if !attached && ((ownership.timing == "before-confirmation" && send) || (ownership.timing == "before-observation" && capture && entered)) {
						attached = true
						attachAndEdit()
					}
					out, err := real.execute(args)
					entered = entered || send
					if !attached && ownership.timing == "late-confirmed" && capture && strings.Contains(out, "esc to interrupt") {
						// The observation is established; attach before returning it
						// to the confirmation loop and its deferred finalizer.
						attached = true
						attachAndEdit()
					}
					return out, err
				}}
				receipts := collectReceipts(tm)
				delivery, err := tm.nudgeSessionDelivery("wrapped", message)
				want, submits := runtime.NudgeSubmitConfirmed, 1
				if ownership.timing == "before-confirmation" {
					want, submits = runtime.NudgeSubmitOwnershipLost, 0
				} else if ownership.timing == "before-observation" {
					want = runtime.NudgeSubmitOwnershipLost
				}
				boundaryErr := delivery.UnconfirmedSubmitError("wrapped")
				terminalLoss := errors.Is(boundaryErr, runtime.ErrNudgeDraftOwnershipLost)
				if !attached || err != nil || delivery.Submit != want || !delivery.Landed() || terminalLoss != (want == runtime.NudgeSubmitOwnershipLost) || errors.Is(boundaryErr, ErrNudgeSubmitUnconfirmed) {
					t.Fatalf("attached=%t delivery=%+v err=%v; want terminal %s without replay", attached, delivery, err, want)
				}
				if got := count(t, logPath, "SUBMIT"); got != submits || len(receipts()) != 1 || receipts()[0].Submitted != (want == runtime.NudgeSubmitConfirmed) {
					t.Fatalf("submits=%d receipts=%v; want %d submits", got, receipts(), submits)
				}
				if submits == 0 {
					pane, err := tm.CapturePane("wrapped", 40)
					got, ok := nudgeComposer(pane, "❯ ")
					if err != nil || !ok || got != draft {
						t.Fatalf("ownership loss changed the draft: %q, %v", got, err)
					}
				}
				return
			}
			if edited || ownership.removed {
				receipts := collectReceipts(tm)
				if err := tm.NudgeSession("wrapped", message); !errors.Is(err, ErrNudgeDraftDeferred) {
					t.Fatalf("attach/edit/detach retry=%v, want deferred ownership", err)
				}
				if got := count(t, logPath, "SUBMIT"); got != 0 || len(receipts()) != 0 {
					t.Fatalf("edited draft submitted %d times with %d receipts", got, len(receipts()))
				}
				return
			}
			if err := tm.NudgeSession("wrapped", message); err != nil {
				t.Fatalf("wrapped draft retry: %v", err)
			}
			stored, err = tm.run("show-options", "-p", "-qv", "-t", "wrapped", "@gc-nudge-draft-payload")
			if err != nil || strings.TrimSpace(stored) != "" {
				t.Fatalf("stored payload digest=%q err=%v, want cleared provenance after submit", stored, err)
			}
			raw, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(raw), "\tSUBMIT\t") != 1 || !strings.Contains(string(raw), "\tSUBMIT\t"+message+"\n") {
				t.Fatalf("want one exact payload, with no second paste:\n%s", raw)
			}
		})
	}
}
