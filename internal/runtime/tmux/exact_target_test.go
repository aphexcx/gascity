package tmux

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestExactSessionTargetCommands(t *testing.T) {
	for _, target := range []string{"abc", "=abc", "%42", "@42", "$42", "abc:^.0", "abc:2", "=abc:", "=abc:2.0"} {
		t.Run(target, func(t *testing.T) {
			executor := &fakeExecutor{}
			tm := NewTmux()
			tm.exec = executor
			wantTarget := target
			if target == "abc" || target == "abc:^.0" || target == "abc:2" {
				wantTarget = "=" + target
			}
			wantSessionTarget := wantTarget
			if target == "abc" || target == "=abc" {
				wantTarget = "=abc:"
			}
			if err := tm.SendKeysRaw(target, "Enter"); err != nil {
				t.Fatal(err)
			}
			want := []string{"-u", "send-keys", "-t", wantTarget, "Enter"}
			if !slices.Equal(executor.calls[0], want) {
				t.Errorf("send args = %q, want %q", executor.calls[0], want)
			}
			if err := tm.KillSession(target); err != nil {
				t.Fatal(err)
			}
			want = []string{"-u", "kill-session", "-t", wantSessionTarget}
			if !slices.Equal(executor.calls[1], want) {
				t.Errorf("kill args = %q, want %q", executor.calls[1], want)
			}
		})
	}
	t.Run("missing environment session", func(t *testing.T) {
		for _, command := range []string{"set-environment", "show-environment"} {
			err := wrapError(errors.New("exit status 1"), "no such session: abc", []string{command})
			if !errors.Is(err, ErrSessionNotFound) {
				t.Errorf("%s error = %v, want ErrSessionNotFound", command, err)
			}
		}
	})
	t.Run("window option target", func(t *testing.T) {
		executor := &fakeExecutor{}
		tm := NewTmux()
		tm.exec = executor
		ops := newTmuxStartOps(tm, "", 0, runtime.Config{}, true)
		if err := ops.disableMouseAndActivity("abc"); err != nil {
			t.Fatal(err)
		}
		want := []string{"-u", "set-option", "-wt", "=abc:", "monitor-activity", "off"}
		if len(executor.calls) != 2 || !slices.Equal(executor.calls[1], want) {
			t.Errorf("option calls = %q, want window target %q", executor.calls, want)
		}
	})
	t.Run("hook log keeps session identity", func(t *testing.T) {
		executor := &fakeExecutor{}
		tm := NewTmux()
		tm.exec = executor
		if err := tm.SetPaneDiedHook("abc", "worker"); err != nil {
			t.Fatal(err)
		}
		call := executor.calls[0]
		if call[3] != "=abc:" || !strings.Contains(call[len(call)-1], "--session 'abc'") {
			t.Errorf("hook call = %q, want exact target and unqualified log identity", call)
		}
	})
	t.Run("missing pane or window is not session absence", func(t *testing.T) {
		for _, kind := range []string{"pane", "window"} {
			want := ErrPaneNotFound
			if kind == "window" {
				want = ErrWindowNotFound
			}
			for _, wording := range []string{"no such ", "can't find "} {
				err := wrapError(errors.New("exit status 1"), wording+kind+": =abc:99.99", []string{"set-option"})
				if !errors.Is(err, want) || errors.Is(err, ErrSessionNotFound) {
					t.Errorf("%s%s error = %v, want %s absence only", wording, kind, err, kind)
				}
			}
		}
	})
}
