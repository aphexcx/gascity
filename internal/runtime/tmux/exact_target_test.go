package tmux

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/stretchr/testify/require"
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
	t.Run("ambiguous absence requires an exact session probe", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			args   []string
			result error
		}{
			{"window option", []string{"set-option", "-wt", "=abc:99", "remain-on-exit", "on"}, ErrWindowNotFound},
			{"pane option", []string{"set-option", "-p", "-t", "=abc:^.99", "@test", "value"}, ErrPaneNotFound},
			{"if-shell", []string{"if-shell", "-F", "-t", "=abc:", "1", "display-message ok"}, ErrPaneNotFound},
			{"quiet if-shell", []string{"if-shell", "-F", "-t", "=abc:", "0", "display-message ok"}, nil},
			{"display", []string{"display-message", "-p", "-t", "=abc:", "#{pane_id}"}, nil},
			{"quiet option", []string{"show-options", "-p", "-qv", "-t", "=abc:", "@unset"}, nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				for _, probeErr := range []error{nil, ErrSessionNotFound, ErrNoServer, ErrPaneNotFound, errors.New("probe failed")} {
					executor := &fakeExecutor{errs: []error{tc.result, probeErr}}
					tm := NewTmux()
					tm.exec = executor
					_, err := tm.run(tc.args...)
					require.Len(t, executor.calls, 2)
					require.Equal(t, []string{"-u", "has-session", "-t", "=abc"}, executor.calls[1])
					if probeErr == nil {
						require.Equal(t, tc.result, err)
					} else {
						require.ErrorIs(t, err, probeErr)
					}
					if tc.result != nil {
						require.ErrorIs(t, err, tc.result)
					}
					require.Equal(t, errors.Is(probeErr, ErrSessionNotFound), errors.Is(err, ErrSessionNotFound))
				}
			})
		}
	})
	t.Run("unambiguous results and opaque IDs need no probe", func(t *testing.T) {
		for _, tc := range []struct {
			args []string
			out  string
			err  error
		}{
			{[]string{"display-message", "-p", "-t", "=abc:", "#{pane_dead}"}, "0", nil},
			{[]string{"display-message", "-p", "-t", "%42", "#{pane_id}"}, "", nil},
			{[]string{"set-option", "-p", "-t", "%42", "@test", "value"}, "", ErrPaneNotFound},
			{[]string{"set-option", "-wt", "@42", "remain-on-exit", "on"}, "", ErrWindowNotFound},
			{[]string{"send-keys", "-t", "=abc:", "x"}, "", ErrSessionNotFound},
			{[]string{"send-keys", "-t", "=abc:", "x"}, "", errors.New("invalid key")},
			{[]string{"show-options", "-gqv", "@unset"}, "", nil},
		} {
			executor := &fakeExecutor{out: tc.out, err: tc.err}
			tm := NewTmux()
			tm.exec = executor
			out, err := tm.run(tc.args...)
			require.Equal(t, tc.out, out)
			require.Equal(t, tc.err, err)
			require.Len(t, executor.calls, 1)
		}
	})
}
