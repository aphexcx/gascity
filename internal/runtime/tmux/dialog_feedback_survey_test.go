package tmux

import (
	"bytes"
	"log"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
)

// feedbackSurveySessionFixture and feedbackSurveyMemoryFixture are
// byte-accurate captures of Claude Code's post-turn feedback survey
// (ga-zg7fjq), kept in sync with the equivalent fixtures in
// internal/runtime/dialog_test.go.
const feedbackSurveySessionFixture = `⏺ Done — pushed the branch and replied on the PR.

● How is Claude doing this session? (optional)
  1: Bad    2: Fine   3: Good   0: Dismiss

╭──────────────────────────────────────────────────────────╮
│ ❯                                                        │
╰──────────────────────────────────────────────────────────╯
  ⏵⏵ bypass permissions on (shift+tab to cycle)`

const feedbackSurveyMemoryFixture = `⏺ Reading the config.

● Claude recalled a memory:

  The user prefers tabs over spaces.

  How was Claude's recollection? (optional)
  1: Bad    2: Fine   3: Good   4: Unsure  0: Dismiss

╭──────────────────────────────────────────────────────────╮
│ ❯                                                        │
╰──────────────────────────────────────────────────────────╯
  ⏵⏵ bypass permissions on (shift+tab to cycle)`

func TestFeedbackSurveyDismissalPreservesDraft(t *testing.T) {
	for _, initialDraft := range []string{"", "preserve this draft"} {
		t.Run(initialDraft, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				draft := initialDraft
				survey := true
				var keys []string
				var logs bytes.Buffer
				originalLog := log.Writer()
				log.SetOutput(&logs)
				t.Cleanup(func() { log.SetOutput(originalLog) })
				tm := NewTmux()
				tm.exec = &fakeExecutor{fn: func(args []string) (string, error) {
					switch {
					case slices.Contains(args, "capture-pane"):
						if !survey {
							return "❯ " + draft, nil
						}
						return strings.Replace(feedbackSurveySessionFixture, "│ ❯ ", "│ ❯ "+draft, 1), nil
					case slices.Contains(args, "send-keys"):
						key := args[len(args)-1]
						keys = append(keys, key)
						if key == "0" {
							draft += "0"
						} else if key == "Enter" && draft == "0" {
							survey, draft = false, ""
						}
					}
					return "", nil
				}}
				tm.DismissFeedbackSurveyModalIfPresent(t.Name())
				if draft != initialDraft {
					t.Fatalf("draft=%q, want byte-identical %q; keys=%q", draft, initialDraft, keys)
				}
				if initialDraft == "" {
					if survey || !slices.Equal(keys, []string{"0", "Enter"}) {
						t.Fatalf("empty composer: survey=%v keys=%q; want dismissed", survey, keys)
					}
				} else if len(keys) != 0 || !strings.Contains(logs.String(), "skip") {
					t.Fatalf("drafted composer: keys=%q log=%q; want logged skip", keys, logs.String())
				}
			})
		})
	}
}

// TestFeedbackSurveyParkedPaneReadsIdle documents the ga-zg7fjq root cause: a
// pane parked on Claude Code's feedback-survey modal shows neither a busy
// indicator nor a filled composer, so it satisfies WaitForIdle's existing
// idle check exactly like a genuinely idle prompt would. That's why
// NudgeSession's only dismissal hook (inside WaitForIdle's err != nil
// branch) never ran for this modal -- WaitForIdle returns nil before that
// branch is ever reached. This pins the pre-fix idle-detection behavior; it
// must keep passing after the fix, since broadening busy detection to cover
// the survey is explicitly out of scope (a survey-parked pane genuinely is
// idle -- the fix dismisses it as a nudge-delivery step instead).
func TestFeedbackSurveyParkedPaneReadsIdle(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
	}{
		{"session feedback variant", feedbackSurveySessionFixture},
		{"memory recollection variant", feedbackSurveyMemoryFixture},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lines := strings.Split(tt.content, "\n")
			if paneContainsBusyIndicator(lines) {
				t.Fatalf("paneContainsBusyIndicator = true, want false (a survey-parked pane must read idle, not busy)")
			}
			found := false
			for _, line := range lines {
				if matchesPromptPrefix(line, DefaultReadyPromptPrefix) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("no line matched the ready-prompt prefix %q; want the boxed composer line to match so the pane reads idle:\n%s", DefaultReadyPromptPrefix, tt.content)
			}
		})
	}
}
