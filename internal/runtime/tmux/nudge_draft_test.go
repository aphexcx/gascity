package tmux

import (
	"slices"
	"strings"
	"testing"
)

func TestNudgeDraftOwnershipHookPrecedesUserHooks(t *testing.T) {
	const invalidate = `set-option -p -t "%0" @gc-nudge-draft-payload ''`
	const user = `run-shell "sleep 10"`
	for _, global := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "global"}[global], func(t *testing.T) {
			tm := NewTmux()
			installed := make(map[string]string)
			tm.exec = &fakeExecutor{fn: func(args []string) (string, error) {
				switch {
				case slices.Contains(args, "display-message"):
					return "%0", nil
				case slices.Contains(args, "show-hooks"):
					event := args[len(args)-1]
					if command, ok := installed[event]; ok && !slices.Contains(args, "-g") {
						return event + "[0] " + command, nil
					}
					if slices.Contains(args, "-g") == global {
						return event + "[0] " + user, nil
					}
				case slices.Contains(args, "set-hook"):
					event, command := args[len(args)-2], args[len(args)-1]
					if slices.Contains(args, "-a") {
						command = user + " ; " + command
					}
					installed[event] = command
				}
				return "", nil
			}}
			if _, err := tm.nudgeDraftOwnershipHook("session", "%0", true); err != nil {
				t.Fatal(err)
			}
			for event, command := range installed {
				if command != invalidate+" ; "+user {
					t.Errorf("%s = %q; ownership must clear before the blocking user hook", event, command)
				}
			}
			if _, err := tm.nudgeDraftOwnershipHook("session", "%0", false); err != nil {
				t.Fatalf("checking installed hook: %v", err)
			}
			installed["client-attached"] = user + " ; " + invalidate
			if _, err := tm.nudgeDraftOwnershipHook("session", "%0", false); err == nil {
				t.Fatal("a clear behind a blocking user command must not establish ownership")
			}
		})
	}
}

func TestNudgeDraftMatches(t *testing.T) {
	t.Run("live Claude 2.1.316 word wrap at 80 columns", func(t *testing.T) {
		// Byte-for-byte composer rows from the private, detached live probe.
		const pane = "────────────────────────────────────────────────────────────────────────────────\n" +
			"❯ The founder expects this complete message to arrive exactly once even when\n" +
			"  the composer wraps whole words onto another row. Keep every word intact and\n" +
			"  preserve the original spaces while the transport retries only the submit\n" +
			"  key.\n────────────────────────────────────────────────────────────────────────────────"
		const message = "The founder expects this complete message to arrive exactly once even when the composer wraps whole words onto another row. Keep every word intact and preserve the original spaces while the transport retries only the submit key."
		draft, observed := nudgeComposer(pane, "❯ ")
		if !observed || !nudgeDraftMatches(draft, message, 78) {
			t.Fatalf("live wrapped draft must match with provenance: %q, observed=%t", draft, observed)
		}
		if nudgeDraftMatches(draft, message, 0) {
			t.Fatal("word-wrap matching must still require provenance")
		}
		if !nudgeDraftMatches(strings.Repeat("a", 76)+"\na b", strings.Repeat("a", 77)+" b", 78) {
			t.Fatal("live Claude long-word rendering must match with provenance")
		}
	})
	for _, tt := range []struct {
		name, draft, message string
		want                 bool
	}{
		{"exact", "one copy", "one copy", true},
		{"spaces", "one  copy", "one\tcopy", false},
		{"quoted spaces", `print("a  b")`, `print("a b")`, false},
		{"owned word-wrap boundary", "one\ncopy", "one copy", true},
		{"identical multiline", "one\n\ncopy", "one\n\ncopy", true},
		{"explicit newline spaces", "first\nsecond", "first  \nsecond", false},
		{"wrapped explicit newline spaces", strings.Repeat("a", 78) + "\nfirst\nsecond", strings.Repeat("a", 78) + "first  \nsecond", false},
		{"full row explicit newline spaces", strings.Repeat("a", 77) + "\nb", strings.Repeat("a", 77) + " \nb", false},
		{"owned row splits a word", "one co\npy", "one copy", true},
		{"ownership disambiguates inserted newlines", "I am now\nhere", "I am nowhere", true},
		{"full row splits a word", strings.Repeat("a", 78) + "\nb", strings.Repeat("a", 78) + "b", true},
		{"wide full row splits a word", strings.Repeat("界", 39) + "\nend", strings.Repeat("界", 39) + "end", true},
		{"wrap trims trailing row space", strings.Repeat("a", 77) + "\n b", strings.Repeat("a", 77) + "  b", true},
		{"wrapped quoted spaces differ", strings.Repeat("a", 78) + "\n" + `print("a  b")`, strings.Repeat("a", 78) + `print("a b")`, false},
		{"blank rows", "one\n\ncopy", "one\ncopy", false},
		{"unicode whitespace", "one\u00a0copy", "one copy", false},
		{"different word", "one\nother", "one copy", false},
		{"different inline spacing", "ab cd", "a bcd", false},
		{"extra suffix", "one copy", "one copy extra", false},
		{"truncated message", "one copy extra", "one copy", false},
		{"empty", "", "", true},
		{"missing draft", "", "one copy", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := nudgeDraftMatches(tt.draft, tt.message, 78); got != tt.want {
				t.Fatalf("matches(%q, %q)=%v, want %v", tt.draft, tt.message, got, tt.want)
			}
		})
	}
	if nudgeDraftMatches(strings.Repeat("a", 78)+"\nb", strings.Repeat("a", 78)+"b", 0) {
		t.Fatal("unknown pane width must not join a word across rows")
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
