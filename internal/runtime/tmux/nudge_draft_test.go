package tmux

import "testing"

func TestNudgeComposer(t *testing.T) {
	for _, tt := range []struct {
		name, pane, prefix, want string
		observed                 bool
	}{
		{"empty", "❯ ", "❯ ", "", true},
		{"draft", "❯ keep one copy", "❯ ", "keep one copy", true},
		{"prompt in draft", "────\n❯ first line\n  ❯ \n────", "❯ ", "first line\n❯", true},
		{"boxed prompt in draft", "╭────╮\n│ ❯ first │\n│   ❯    │\n╰────╯", "❯ ", "first\n❯", true},
		{"border in draft", "────\n❯ first line\n  ────\n  last line\n────", "❯ ", "first line\n────\nlast line", true},
		{"rounded box in draft", "────\n❯ \n  ╭──╮\n  │draft│\n  ╰──╯\n────", "❯ ", "╭──╮\n│draft│\n╰──╯", true},
		{"rounded box before prompt in draft", "────\n❯ first\n  ╭──╮\n  ❯ \n  ╰──╯\n────", "❯ ", "first\n╭──╮\n❯\n╰──╯", true},
		{"boxed empty", feedbackSurveySessionFixture, "❯ ", "", true},
		{"boxed continuation", "╭────╮\n│ ❯    │\n│  keep me  │\n╰────╯", "❯ ", "keep me", true},
		{"multiline", "────\n❯ first\n  \n  second\n────\nstatus", "❯ ", "first\n\nsecond", true},
		{"suggestion", "────\n❯ \x1b[2mTry a suggestion\x1b[22m\n────", "❯ ", "", true},
		{"typed beside suggestion", "────\n❯ typed\x1b[2m suggestion\x1b[22m\n────", "❯ ", "typed", true},
		{"codex footer", "› \n\n  ? for shortcuts", "› ", "", true},
		{"codex suggestion", "› \x1b[2mExplain this codebase\x1b[0m\n\n  gpt-5.5 low", "› ", "", true},
		{"opaque paste", "› [Pasted Content 2048 chars]\n\n  ? for shortcuts", "› ", "[Pasted Content 2048 chars]", true},
		{"transcript", "❯ prior text\nanswer\n❯ ", "❯ ", "", true},
		{"missing prompt", "working", "❯ ", "", false},
		{"incomplete box", "│ ❯ \nunknown layout", "❯ ", "", false},
		{"clipped box", "│ ❯ ", "❯ ", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, observed := nudgeComposer(tt.pane, tt.prefix)
			if got != tt.want || observed != tt.observed {
				t.Fatalf("composer=(%q, %v), want (%q, %v)", got, observed, tt.want, tt.observed)
			}
		})
	}
}
