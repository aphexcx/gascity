package tmux

import (
	"fmt"
	"slices"
	"testing"
)

func TestProviderPeekPreservesAttributes(t *testing.T) {
	for _, lines := range []int{40, 0, -1} {
		t.Run(fmt.Sprint(lines), func(t *testing.T) {
			capture := "\x1b[2msuggestion\x1b[0m"
			exec := &fakeExecutor{out: capture}
			p := &Provider{tm: &Tmux{cfg: DefaultConfig(), exec: exec}}
			got, err := p.Peek("worker", lines)
			if err != nil || got != capture {
				t.Fatalf("Peek = %q, %v", got, err)
			}
			start := "-"
			if lines > 0 {
				start = fmt.Sprintf("-%d", lines)
			}
			want := []string{"-u", "capture-pane", "-p", "-e", "-t", "worker", "-S", start}
			if len(exec.calls) != 1 || !slices.Equal(exec.calls[0], want) {
				t.Fatalf("capture calls = %v, want %v", exec.calls, want)
			}
		})
	}
}
