package runtime

import (
	"strings"
	"testing"
)

func TestRenderPeek(t *testing.T) {
	// Measured frame/prompt colors (2026-10-30) and the recorded SGR 2
	// suggestion (2026-10-29); text is synthetic, not a live transcript.
	frame := "\x1b[38;5;244m────────────────\x1b[39m\n"
	prompt := "\x1b[38;5;246m❯\u00a0\x1b[39m"
	box := func(text string) string { return frame + prompt + text + "\n" + frame }
	for _, tc := range []struct {
		name, input, suggestion, kept string
	}{
		{"suggestion", box("\x1b[2mTry the next step\x1b[0m"), "Try the next step", "❯\u00a0"},
		{"cursor placeholder", box("\x1b[7mT\x1b[0;2mry the next step\x1b[0m"), "Try the next step", "❯\u00a0"},
		{"cursor on draft", box("\x1b[7mR\x1b[27mun the checks"), "", "Run the checks"},
		{"unicode cursor placeholder", box("\x1b[7mÉ\x1b[0;2mcrivez un message\x1b[0m"), "Écrivez un message", "❯\u00a0"},
		{"wrapped cursor placeholder", box("\x1b[7mT\x1b[0;2mry\n  another task\x1b[0m"), "Try\n  another task", "❯\u00a0"},
		{"typed continuation", box("\x1b[7mT\x1b[0;2mry\x1b[0m\n  actual input"), "ry", "T\n  actual input"},
		{"inverse typed suffix", box("\x1b[7mT\x1b[27myped\x1b[2m completion\x1b[0m"), "completion", "Typed"},
		{"color is not inverse", box("\x1b[38;5;7mT\x1b[0;2myped completion\x1b[0m"), "yped completion", "❯\u00a0T"},
		{"unframed cursor", prompt + "\x1b[7mT\x1b[0;2mext\x1b[0m", "", "Text"},
		{"live capture", frame + "\x1b[39m❯\u00a0\x1b[2mkeep going\x1b[0m\n" + frame, "keep going", "❯\u00a0"},
		{"typed", box("Run the checks"), "", "Run the checks"},
		{"status", "\x1b[2mWorking\x1b[0m\n" + box("typed"), "", "Working"},
		{"unframed", prompt + "\x1b[2mordinary output\x1b[0m\n", "", "ordinary output"},
		{"grey typed", box("\x1b[38;5;246mordinary grey\x1b[39m"), "", "ordinary grey"},
		{"true color", box("\x1b[38;2;2;2;2mtyped RGB\x1b[39m"), "", "typed RGB"},
		{"background color", box("\x1b[48;5;2mtyped indexed\x1b[49m"), "", "typed indexed"},
		{"reset faint", box("\x1b[2mguess\x1b[22m typed"), "guess", " typed"},
		{"combined", box("\x1b[0;2;38;5;246mguess\x1b[m"), "guess", "❯\u00a0"},
		{"colon color", box("\x1b[38:2::2:2:2mtyped\x1b[m"), "", "typed"},
		{"wrapped", box("\x1b[2mfirst line\n  second line\x1b[0m"), "first line\n  second line", "❯\u00a0"},
		{"historical box", box("\x1b[2mold\x1b[0m") + "answer\n" + box("typed"), "", "old"},
		{"blank faint", box("\x1b[2m  \x1b[0m"), "", "❯\u00a0  "},
		{"empty", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := RenderPeek(tc.input)
			if got.Suggestion != tc.suggestion {
				t.Fatalf("suggestion = %q, want %q", got.Suggestion, tc.suggestion)
			}
			if !strings.Contains(got.Output, tc.kept) {
				t.Fatalf("output = %q, lost %q", got.Output, tc.kept)
			}
			if strings.Contains(got.Output, "\x1b") || strings.Contains(got.Text, "\x1b") {
				t.Fatalf("SGR survived: %+v", got)
			}
			if strings.Count(got.Output, "\n") != strings.Count(tc.input, "\n") {
				t.Fatalf("line count changed: %+v", got)
			}
			if tc.suggestion != "" {
				if !strings.Contains(got.Text, "[suggestion] ") {
					t.Fatalf("missing tag: %q", got.Text)
				}
				if strings.Contains(got.Output, strings.Split(tc.suggestion, "\n")[0]) {
					t.Fatalf("suggestion remained in output: %q", got.Output)
				}
			} else if strings.Contains(got.Text, "[suggestion]") {
				t.Fatalf("false suggestion: %q", got.Text)
			}
		})
	}
}
