package tmux

import "testing"

func TestNudgeDraftMatches(t *testing.T) {
	for _, tt := range []struct {
		name, draft, message string
		want                 bool
	}{
		{"exact", "one copy", "one copy", true},
		{"spaces", "one  copy", "one\tcopy", true},
		{"row boundary", "one\ncopy", "one copy", true},
		{"word split", "one co\npy", "one copy", true},
		{"blank rows", "one\n\ncopy", "one\ncopy", true},
		{"unicode whitespace", "one\u00a0copy", "one copy", true},
		{"different word", "one\nother", "one copy", false},
		{"different inline spacing", "ab cd", "a bcd", false},
		{"extra suffix", "one copy", "one copy extra", false},
		{"truncated message", "one copy extra", "one copy", false},
		{"empty", "", "", true},
		{"missing draft", "", "one copy", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := nudgeDraftMatches(tt.draft, tt.message); got != tt.want {
				t.Fatalf("matches(%q, %q)=%v, want %v", tt.draft, tt.message, got, tt.want)
			}
		})
	}
}

func TestNudgeComposer(t *testing.T) {
	for _, tt := range []struct {
		name, pane, prefix, want string
		observed                 bool
	}{
		{"empty", "❯ ", "❯ ", "", true},
		{"draft", "❯ keep one copy", "❯ ", "keep one copy", true},
		{"prompt in draft", "────\n❯ first line\n  ❯ \n────", "❯ ", "first line\n❯", true},
		{"boxed prompt in draft", "╭────╮\n│ ❯ first │\n│   ❯    │\n╰────╯", "❯ ", "first\n❯", true},
		{"literal box prompt in draft", "────\n❯ keep this draft\n  │❯ │\n────", "❯ ", "keep this draft\n│❯ │", true},
		{"literal heavy box prompt in draft", "────\n❯ keep this draft\n  ┃❯ ┃\n────", "❯ ", "keep this draft\n┃❯ ┃", true},
		{"indented box without composer", "  │❯ │\n────", "❯ ", "", false},
		{"heavy box clipped", "┃ ❯ ", "❯ ", "", false},
		{"border in draft", "────\n❯ first line\n  ────\n  last line\n────", "❯ ", "first line\n────\nlast line", true},
		{"rounded box in draft", "────\n❯ \n  ╭──╮\n  │draft│\n  ╰──╯\n────", "❯ ", "╭──╮\n│draft│\n╰──╯", true},
		{"rounded box before prompt in draft", "────\n❯ first\n  ╭──╮\n  ❯ \n  ╰──╯\n────", "❯ ", "first\n╭──╮\n❯\n╰──╯", true},
		{"boxed empty", feedbackSurveySessionFixture, "❯ ", "", true},
		{"boxed continuation", "╭────╮\n│ ❯    │\n│  keep me  │\n╰────╯", "❯ ", "keep me", true},
		{"multiline", "────\n❯ first\n  \n  second\n────\nstatus", "❯ ", "first\n\nsecond", true},
		{"suggestion", "────\n❯ \x1b[2mTry a suggestion\x1b[22m\n────", "❯ ", "", true},
		{"typed beside suggestion", "────\n❯ typed\x1b[2m suggestion\x1b[22m\n────", "❯ ", "typed", true},
		{"unframed ambiguous footer", "› \n\n  ? for shortcuts", "› ", "", false},
		{"unframed blank continuation", "› first\n  \n  last", "› ", "", false},
		{"unframed leading blanks", "› \n\n  keep this draft", "› ", "", false},
		{"unframed unknown boundary", "› first\nunknown layout", "› ", "", false},
		{"unframed trailing blank rows", "› first\n\n\n", "› ", "first", true},
		{"codex suggestion", "› \x1b[2mExplain this codebase\x1b[0m\n\n  gpt-5.5 low", "› ", "", true},
		{"codex footer", "› payload\n\n  gpt-5.5 low · /tmp/probe", "› ", "payload", true},
		{"codex live footer", "› payload\n\n  gpt-6-astra default · ~/city/.worktrees/gascity/lane-implementation-worker-codex-gascity\n\n", "› ", "payload", true},
		{"codex empty footer", "› \n\n  gpt-6-astra xhigh · ~/city/lane", "› ", "", true},
		{"codex multiline footer", "› first\n  \n  last\n\n  gpt-5.5 low · /tmp/probe", "› ", "first\n\nlast", true},
		{"codex literal footer in draft", "› first\n  \n  gpt-5.5 low · /tmp/probe\n  last\n\n  gpt-5.5 low · /tmp/probe", "› ", "first\n\ngpt-5.5 low · /tmp/probe\nlast", true},
		{"codex unknown footer", "› payload\n\n  gpt-5.5 unknown · /tmp/probe", "› ", "", false},
		{"codex footer missing cwd", "› payload\n\n  gpt-5.5 low", "› ", "", false},
		{"codex footer followed by unknown row", "› payload\n\n  gpt-5.5 low · /tmp/probe\n  unknown", "› ", "", false},
		{"codex footer with unindented continuation", "› first\nunknown\n\n  gpt-5.5 low · /tmp/probe", "› ", "", false},
		{"opaque paste", "› [Pasted Content 2048 chars]\n\n  ? for shortcuts", "› ", "", false},
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
