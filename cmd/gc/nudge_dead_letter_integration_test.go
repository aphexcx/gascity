//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/gastownhall/gascity/internal/worker"
	"github.com/gastownhall/gascity/test/tmuxtest"
)

func TestNudgeDeadLetterReplayTmux(t *testing.T) {
	if os.Getenv("GC_TMUX_INTEGRATION") != "1" {
		t.Skip("set GC_TMUX_INTEGRATION=1 for the private-socket dead-letter/replay proof")
	}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_MAIL", "")
	guard := tmuxtest.NewGuard(t)
	dir := t.TempDir()
	t.Setenv("GC_CITY", dir)
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"nudge-proof\"\n[beads]\nprovider = \"file\"\n[session]\npark_alert_to = \"operator\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := openCityStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(store) })
	script, err := filepath.Abs("testdata/nudge_dead_letter_tui.py")
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "tui.log")
	tmuxConfig := tmux.Config{SocketName: guard.SocketName(), NudgeReadyTimeout: 10 * time.Second, NudgeLockTimeout: 10 * time.Second}
	client := tmux.NewTmuxWithConfig(tmuxConfig)
	if err := client.NewSessionWithCommand("worker", dir, fmt.Sprintf("python3 %s %s", shellquote.Quote(script), shellquote.Quote(logPath))); err != nil {
		t.Fatal(err)
	}
	if err := client.SetEnvironment("worker", "GC_PROVIDER", "codex"); err != nil {
		t.Fatal(err)
	}
	capture := func() string {
		t.Helper()
		out, err := client.CapturePane("worker", 40)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	waitNudgeProofPane(t, capture, "fake-codex-tui")
	if err := client.SendKeysRaw("worker", strings.Repeat("opaque draft ", 30)); err != nil {
		t.Fatal(err)
	}
	waitNudgeProofPane(t, capture, "[Pasted Content")

	provider := tmux.NewProviderWithConfig(tmuxConfig)
	target := nudgeTarget{cityPath: dir, agent: config.Agent{Name: "worker"}, resolved: &config.ResolvedProvider{Name: "codex"}, sessionName: "worker"}
	item := newQueuedNudge("worker", "recover this founder line", time.Now())
	if err := enqueueQueuedNudgeWithStore(dir, beads.NudgesStore{Store: store}, item); err != nil {
		t.Fatal(err)
	}
	for range defaultQueuedNudgeMaxAttempts {
		landed, err := tryDeliverQueuedNudgesByPoller(target, store, store, provider, 0, worker.LiveObservation{Running: true})
		if landed || !errors.Is(err, runtime.ErrNudgeDraftUnowned) {
			t.Fatalf("unowned draft: landed=%t err=%v", landed, err)
		}
		// Advance only the persisted retry deadline; this proof owns the
		// real transport boundary, while unit tests own backoff timing.
		if err := nudgequeue.WithState(dir, func(s *nudgequeue.State) error {
			for i := range s.Pending {
				s.Pending[i].DeliverAfter = time.Now().Add(-time.Second)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	mailbox := beadmail.New(store)
	letters, err := mailbox.Inbox("operator")
	if err != nil || len(letters) != 1 || !strings.Contains(letters[0].Body, "gc nudge replay "+item.ID) {
		t.Fatalf("dead-letter mail: %+v err=%v", letters, err)
	}
	row := doctor.NewNudgeDeadLettersCheck().Run(&doctor.CheckContext{CityPath: dir})
	if row.Status != doctor.StatusWarning || !strings.Contains(strings.Join(row.Details, "\n"), item.ID) {
		t.Fatalf("doctor row: %+v", row)
	}
	t.Logf("private socket=%s; mail=%q; doctor=%s %v", guard.SocketName(), letters[0].Subject, row.Message, row.Details)
	var doctorOut, doctorErr bytes.Buffer
	doctorCmd := newDoctorCmd(&doctorOut, &doctorErr)
	doctorCmd.SetArgs([]string{"--check", "nudge-dead-letters", "--verbose"})
	if err := doctorCmd.Execute(); err != nil || !strings.Contains(doctorOut.String(), item.ID) {
		t.Fatalf("gc doctor: %v stdout=%s stderr=%s", err, &doctorOut, &doctorErr)
	}
	t.Logf("gc doctor --check nudge-dead-letters --verbose:\n%s", &doctorOut)

	if err := client.SendKeysRaw("worker", "C-u"); err != nil {
		t.Fatal(err)
	}
	waitNudgeProofPane(t, capture, "Explain this codebase")
	var stdout, stderr bytes.Buffer
	cmd := newNudgeCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"replay", item.ID})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gc nudge replay: %v: %s", err, &stderr)
	}
	replayID := strings.TrimSpace(stdout.String())
	if landed, err := tryDeliverQueuedNudgesByPoller(target, store, store, provider, 0, worker.LiveObservation{Running: true}); err != nil || !landed {
		t.Fatalf("replay delivery: landed=%t err=%v", landed, err)
	}
	if landed, err := tryDeliverQueuedNudgesByPoller(target, store, store, provider, 0, worker.LiveObservation{Running: true}); err != nil || landed {
		t.Fatalf("second dispatch duplicated replay: landed=%t err=%v", landed, err)
	}
	state, err := nudgequeue.LoadState(dir)
	if err != nil || len(state.Pending)+len(state.InFlight) != 0 || len(state.Dead) != 1 || state.Dead[0].ReplayID != replayID {
		t.Fatalf("post-replay queue: %+v err=%v", state, err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil || strings.Count(string(log), "SUBMIT\t") != 1 || !strings.Contains(string(log), item.Message) {
		t.Fatalf("expected exactly one submit: %s err=%v", log, err)
	}
	letters, err = mailbox.Inbox("operator")
	if err != nil || len(letters) != 1 {
		t.Fatalf("repeat notification: %+v err=%v", letters, err)
	}
	t.Logf("gc nudge replay %s => %s; TUI accepted exactly once; original retained dead", item.ID, replayID)
	if err := client.KillSession("worker"); err != nil {
		t.Fatal(err)
	}
	if guard.HasSession("worker") {
		t.Fatal("private tmux session survived cleanup")
	}
}

// The fake TUI exposes only terminal frames. Share one bounded black-box waiter
// for its initial, drafted and cleared states; no timing sleeps decide success.
func waitNudgeProofPane(t *testing.T, capture func() string, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		pane := capture()
		if strings.Contains(pane, want) {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("pane never contained %q: %q", want, pane)
		}
	}
}
