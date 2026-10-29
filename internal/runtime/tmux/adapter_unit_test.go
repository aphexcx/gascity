package tmux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

func TestBuildLaunchCommandUnsetsColorKillersForInteractiveExecutables(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		command  string
		want     string
	}{
		{name: "claude", provider: "claude", command: "claude", want: "env -u CI -u NO_COLOR claude"},
		{name: "claude alias", provider: "qlandia/claude", command: "claude", want: "env -u CI -u NO_COLOR claude"},
		{name: "claude without provider", command: "claude", want: "env -u CI -u NO_COLOR claude"},
		{name: "codex", provider: "codex", command: "codex", want: "env -u CI -u NO_COLOR codex"},
		{name: "kiro command", provider: "claude", command: "kiro-cli", want: "kiro-cli"},
		{name: "omp", provider: "omp", command: "omp", want: "omp"},
		{name: "custom", provider: "custom", command: "custom-agent", want: "custom-agent"},
		{name: "custom codex", provider: "custom-codex", command: "custom-codex", want: "custom-codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := buildLaunchCommand("worker", runtime.Config{Command: tc.command, ProviderName: tc.provider})
			if err != nil {
				t.Fatalf("buildLaunchCommand: %v", err)
			}
			if got != tc.want {
				t.Fatalf("command = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildLaunchCommandColorWrapsLongPromptCommand(t *testing.T) {
	got, promptFile, err := buildLaunchCommand("worker", runtime.Config{
		Command:      "/opt/bin/claude",
		ProviderName: "kiro",
		WorkDir:      t.TempDir(),
		PromptSuffix: strings.Repeat("prompt ", maxInlinePromptLen),
	})
	if err != nil {
		t.Fatalf("buildLaunchCommand: %v", err)
	}
	if promptFile == "" {
		t.Fatal("long prompt did not create a prompt file")
	}
	if !strings.HasPrefix(got, "env -u CI -u NO_COLOR sh -c ") {
		t.Fatalf("command = %q, want env wrapper around final sh -c command", got)
	}
}

func TestProviderAttachRefusesDeadPane(t *testing.T) {
	fe := &fakeExecutor{
		outs: []string{"", "1"},
	}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe

	err := p.Attach("runner")
	if err == nil {
		t.Fatal("Attach = nil, want dead pane error")
	}
	if !strings.Contains(err.Error(), "dead pane") {
		t.Fatalf("Attach error = %v, want dead pane context", err)
	}
	for _, call := range fe.calls {
		if strings.Contains(strings.Join(call, " "), "attach-session") {
			t.Fatalf("Attach attempted tmux attach-session for dead pane: %v", fe.calls)
		}
	}
}

func TestProviderAttachMissingSessionWrapsRuntimeSentinel(t *testing.T) {
	fe := &fakeExecutor{
		err: ErrSessionNotFound,
	}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe

	err := p.Attach("runner")
	if !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Attach error = %v, want runtime.ErrSessionNotFound", err)
	}
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Attach error = %v, want tmux ErrSessionNotFound", err)
	}
	for _, call := range fe.calls {
		if strings.Contains(strings.Join(call, " "), "attach-session") {
			t.Fatalf("Attach attempted tmux attach-session for missing session: %v", fe.calls)
		}
	}
}

func TestProviderListRunningReportsPartialOnNoServer(t *testing.T) {
	fe := &fakeExecutor{err: ErrNoServer}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe

	names, err := p.ListRunning("")
	if names != nil {
		t.Fatalf("ListRunning names = %v, want nil on unreachable server", names)
	}
	if !runtime.IsPartialListError(err) {
		t.Fatalf("ListRunning err = %v, want runtime.PartialListError so reconciler guards defer", err)
	}
	if !errors.Is(err, ErrNoServer) {
		t.Fatalf("ListRunning err = %v, want wrapped ErrNoServer cause", err)
	}
}

func TestProviderListRunningPropagatesNonServerError(t *testing.T) {
	sentinel := errors.New("tmux exploded")
	fe := &fakeExecutor{err: sentinel}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe

	names, err := p.ListRunning("")
	if names != nil {
		t.Fatalf("ListRunning names = %v, want nil on error", names)
	}
	if runtime.IsPartialListError(err) {
		t.Fatalf("ListRunning err = %v, want a plain error (not partial) for a real tmux failure", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("ListRunning err = %v, want the underlying tmux error", err)
	}
}

// TestListSessionsAbsorbsNoServer pins the tmux-internal contract that the
// change deliberately preserves: ListSessions still reports an unreachable
// server as an empty result so FindSessionByWorkDir and CleanupOrphanedSessions
// keep treating "server down" as "no sessions". Only Provider.ListRunning
// surfaces the outage as a PartialListError.
func TestListSessionsAbsorbsNoServer(t *testing.T) {
	fe := &fakeExecutor{err: ErrNoServer}
	tm := NewTmux()
	tm.exec = fe

	names, err := tm.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions err = %v, want nil (no server absorbed)", err)
	}
	if names != nil {
		t.Fatalf("ListSessions names = %v, want nil", names)
	}
}

func TestProviderAttachReportsHasSessionError(t *testing.T) {
	fe := &fakeExecutor{
		err: errors.New("tmux unavailable"),
	}
	p := NewProviderWithConfig(Config{SocketName: "x"})
	p.tm.exec = fe

	err := p.Attach("runner")
	if err == nil {
		t.Fatal("Attach = nil, want has-session error")
	}
	if !strings.Contains(err.Error(), "checking tmux session before attach") {
		t.Fatalf("Attach error = %v, want checking context", err)
	}
	for _, call := range fe.calls {
		if strings.Contains(strings.Join(call, " "), "attach-session") {
			t.Fatalf("Attach attempted tmux attach-session after has-session error: %v", fe.calls)
		}
	}
}

// Native transcript writes keep a quiet pane active without borrowing another
// conversation's writes from the same working directory.
func TestProviderActivityIncludesExactClaudeTranscript(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "work")
	project := filepath.Join(root, "projects", sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(filepath.Join(project, "own", "subagents"), 0o755); err != nil {
		t.Fatal(err)
	}
	pane := time.Unix(1700000000, 0)
	main := pane.Add(time.Minute)
	child := main.Add(time.Minute)
	write := func(path string, stamp time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(project, "own.jsonl"), main)
	write(filepath.Join(project, "foreign.jsonl"), child.Add(time.Hour))
	write(filepath.Join(project, "old.jsonl"), pane.Add(-time.Minute))
	t.Run("pane query preserves newest window and discounts nudge echo", func(t *testing.T) {
		p := NewProviderWithConfig(Config{SocketName: "activity-test"})
		for _, poked := range []bool{false, true} {
			p.tm.exec = &fakeExecutor{out: fmt.Sprintf("%d\tcodex\n%d\tcodex", pane.Unix(), main.Unix())}
			want := main
			if poked {
				p.tm.recordPokeAt("worker", pane, main)
				want = pane
			}
			if got, err := p.GetLastActivity("worker"); err != nil || !got.Equal(want) {
				t.Fatalf("poked=%t: activity = %v, %v; want %v", poked, got, err, want)
			}
		}
	})
	for _, tc := range []struct {
		name, provider, key string
		want                time.Time
		unknown             bool
	}{
		{"claude", "claude", "own", main, false},
		{"opus", "claude-opus", "own", main, false},
		{"pane newer", "claude", "old", pane, false},
		{"missing key", "claude", "", time.Time{}, true},
		{"invalid key", "claude", "../other", time.Time{}, true},
		{"missing transcript", "claude", "missing", pane, false},
		{"non claude", "codex", "own", pane, false},
		{"no provider", "", "", pane, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewProviderWithConfig(Config{SocketName: "activity-test"})
			p.tm.exec = &fakeExecutor{outs: []string{
				fmt.Sprintf("%d\t%s", pane.Unix(), tc.provider),
				fmt.Sprintf("GC_PROVIDER=%s\nGC_SESSION_KEY=%s\nGC_DIR=%s\nCLAUDE_CONFIG_DIR=%s", tc.provider, tc.key, workDir, root),
			}}
			got, err := p.GetLastActivity("worker")
			if tc.unknown {
				if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !got.IsZero() {
					t.Fatalf("activity = %v, %v; want unknown", got, err)
				}
			} else if err != nil || !got.Equal(tc.want) {
				t.Fatalf("activity = %v, %v; want %v", got, err, tc.want)
			}
			if (tc.provider == "codex" || tc.provider == "") && len(p.tm.exec.(*fakeExecutor).calls) != 1 {
				t.Fatal("non-Claude activity performed an extra tmux query")
			}
		})
	}
	t.Run("first transcript write", func(t *testing.T) {
		p := NewProviderWithConfig(Config{SocketName: "activity-test"})
		activity := func() (time.Time, error) {
			p.tm.exec = &fakeExecutor{outs: []string{
				fmt.Sprintf("%d\tclaude", pane.Unix()),
				fmt.Sprintf("GC_PROVIDER=claude\nGC_SESSION_KEY=fresh\nGC_DIR=%s\nCLAUDE_CONFIG_DIR=%s", workDir, root),
			}}
			return p.GetLastActivity("worker")
		}
		if got, err := activity(); err != nil || !got.Equal(pane) {
			t.Fatalf("before first write = %v, %v; want pane activity", got, err)
		}
		write(filepath.Join(project, "fresh.jsonl"), main)
		if got, err := activity(); err != nil || !got.Equal(main) {
			t.Fatalf("after first write = %v, %v; want transcript activity", got, err)
		}
	})
	t.Run("unreadable transcript path", func(t *testing.T) {
		broken := filepath.Join(root, "not-a-directory")
		write(broken, main)
		p := NewProviderWithConfig(Config{SocketName: "activity-test"})
		p.tm.exec = &fakeExecutor{outs: []string{
			fmt.Sprintf("%d\tclaude", pane.Unix()),
			fmt.Sprintf("GC_PROVIDER=claude\nGC_SESSION_KEY=own\nGC_DIR=%s\nCLAUDE_CONFIG_DIR=%s", workDir, broken),
		}}
		if got, err := p.GetLastActivity("worker"); !errors.Is(err, runtime.ErrRuntimeUnavailable) || !got.IsZero() {
			t.Fatalf("unreadable transcript activity = %v, %v; want unknown", got, err)
		}
	})
	t.Run("production wrapper preserves uncertainty", func(t *testing.T) {
		p := NewSeamBackedWithConfig(Config{SocketName: "activity-test"}).(*seamBackedProvider)
		p.tm.exec = &fakeExecutor{outs: []string{fmt.Sprintf("%d\tclaude", pane.Unix()), "GC_PROVIDER=claude"}}
		got, err := p.GetLastActivity("worker")
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !got.IsZero() {
			t.Fatalf("wrapped activity = %v, %v; want unknown", got, err)
		}
	})
	write(filepath.Join(project, "own", "subagents", "agent-child.jsonl"), child)
	p := NewProviderWithConfig(Config{SocketName: "activity-test"})
	p.tm.exec = &fakeExecutor{outs: []string{fmt.Sprintf("%d\tclaude", pane.Unix()), fmt.Sprintf("GC_PROVIDER=claude\nGC_SESSION_KEY=own\nGC_DIR=%s\nCLAUDE_CONFIG_DIR=%s", workDir, root)}}
	got, err := p.GetLastActivity("worker")
	if err != nil || !got.Equal(child) {
		t.Fatalf("child activity = %v, %v; want %v", got, err, child)
	}
	// An unreadable subagent directory must not silently become stale activity.
	if err := os.MkdirAll(filepath.Join(project, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(project, "broken.jsonl"), main)
	write(filepath.Join(project, "broken", "subagents"), main)
	_, err = claudeTranscriptActivity(map[string]string{"GC_SESSION_KEY": "broken", "GC_DIR": workDir, "CLAUDE_CONFIG_DIR": root})
	if err == nil {
		t.Fatal("unreadable subagent directory was treated as quiet")
	}
}

func TestTmuxClientDoesNotInheritConversationIdentity(t *testing.T) {
	got := tmuxClientEnv([]string{"GC_SESSION_KEY=old-conversation", "PATH=/bin"})
	if len(got) != 1 || got[0] != "PATH=/bin" {
		t.Fatalf("client environment = %v", got)
	}
}
