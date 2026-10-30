package runtime

import "testing"

func TestStartupMenuReason(t *testing.T) {
	for _, tt := range []struct{ name, pane, want string }{
		{"hooks", codexHookReviewDialogFixture(), "Hooks need review"},
		{"directory", "Do you trust the contents of this directory?\n› 1. Yes, continue\n  2. No, exit\nPress enter to confirm", "Do you trust the contents of this directory?"},
		{"ready", "OpenAI Codex\n› Fix the parser", ""},
		{"discussion", "The old pane said Hooks need review. It is fixed now.\n› ", ""},
		{"cleared menu in scrollback", codexHookReviewDialogFixture() + "\nOpenAI Codex\n› Fix the parser", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := StartupMenuReason(tt.pane); got != tt.want {
				t.Fatalf("reason=%q want=%q", got, tt.want)
			}
		})
	}
}
