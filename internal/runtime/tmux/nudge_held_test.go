package tmux

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestNudgeRecoversEarlierOpaquePaste(t *testing.T) {
	for _, scenario := range []string{"recover", "suggestion", "busy", "young", "disabled", "attached", "human identical"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				t.Cleanup(func() { sessionNudgeLocks.Delete(t.Name()) })
				tm := NewTmux()
				prior := runtime.NudgePayloadDigest("one\ntwo\nthree")
				options := map[string]string{
					nudgeDraftPayloadOption: prior,
					"@gc-nudge-draft-shape": fmt.Sprintf("%s:%d:2", prior, time.Now().Add(-time.Minute).UnixMilli()),
				}
				draft, busy := "[Pasted text #12 +2 lines]", false
				if scenario == "busy" {
					busy = true
				}
				if scenario == "young" {
					options[nudgeDraftShapeOption] = fmt.Sprintf("%s:%d:2", prior, time.Now().UnixMilli())
				}
				if scenario == "disabled" {
					tm.cfg.DisableOpaquePasteRecovery = true
				}
				if scenario == "human identical" {
					draft = "next nudge"
					options[nudgeDraftPayloadOption] = ""
				}
				pastes, submits := 0, 0
				tm.exec = &fakeExecutor{fn: func(args []string) (string, error) {
					switch {
					case slices.Contains(args, "show-environment") && args[len(args)-1] == "GC_PROVIDER":
						return "GC_PROVIDER=claude", nil
					case slices.Contains(args, "capture-pane") || (slices.Contains(args, "if-shell") && strings.HasPrefix(args[len(args)-2], "capture-pane ")):
						pane := "────\n❯ " + draft + "\n────"
						if scenario == "suggestion" && draft == "" {
							pane = "────\n❯ \x1b[2mTry a suggestion\x1b[22m\n────"
							if !slices.Contains(args, "-e") && !strings.Contains(strings.Join(args, " "), "capture-pane -p -e ") {
								pane = "────\n❯ Try a suggestion\n────"
							}
						}
						if busy {
							pane = "esc to interrupt\n" + pane
						}
						return pane, nil
					case slices.Contains(args, "display-message") && slices.Contains(args, "#{pane_id}"):
						return "%0", nil
					case slices.Contains(args, "display-message") && slices.Contains(args, "#{session_attached}"):
						if scenario == "attached" {
							return "1", nil
						}
						return "0", nil
					case slices.Contains(args, "show-hooks"):
						return args[len(args)-1] + `[0] set-option -p -t "%0" @gc-nudge-draft-payload ''`, nil
					case slices.Contains(args, "show-options"):
						return options[args[len(args)-1]], nil
					case slices.Contains(args, "set-option"):
						options[args[len(args)-2]] = args[len(args)-1]
					case slices.Contains(args, "send-keys") || (slices.Contains(args, "if-shell") && strings.HasPrefix(args[len(args)-2], "send-keys ")):
						submits++
						draft, busy = "", true
					}
					return "", nil
				}}
				delivery, err := tm.nudgeSessionDeliveryWith(t.Name(), "next nudge", func(_, text string, _ time.Duration) (int, error) {
					if draft != "" {
						t.Fatalf("new paste overwrote held draft %q", draft)
					}
					pastes++
					draft = text
					return len(text), nil
				})
				if scenario != "recover" && scenario != "suggestion" {
					if !errors.Is(err, ErrNudgeDraftDeferred) || delivery.Landed() || delivery.Held == nil || pastes != 0 || submits != 0 {
						t.Fatalf("unsafe recovery: delivery=%+v err=%v pastes=%d submits=%d", delivery, err, pastes, submits)
					}
					want := "unowned_draft"
					if scenario == "busy" || scenario == "young" {
						want = "own_paste"
					}
					if delivery.Held.State != want {
						t.Fatalf("held=%+v, want %s", delivery.Held, want)
					}
				} else if err != nil || !delivery.Landed() || delivery.Bytes != len("next nudge") || pastes != 1 || submits != 2 {
					t.Fatalf("delivery=%+v err=%v pastes=%d submits=%d; want recovery then exactly one new delivery", delivery, err, pastes, submits)
				}
			})
		})
	}
}

func TestOwnedPasteShape(t *testing.T) {
	digest := runtime.NudgePayloadDigest("one\ntwo\nthree")
	shape := digest + ":1700000000000:2"
	for _, tc := range []struct {
		name, stored, shape, draft string
		want                       bool
	}{
		{"owned", digest, shape, "[Pasted text #12 +2 lines]", true},
		{"wrong count", digest, shape, "[Pasted text #12 +3 lines]", false},
		{"no provenance", "", shape, "[Pasted text #12 +2 lines]", false},
		{"pending", "pending:" + digest, shape, "[Pasted text #12 +2 lines]", false},
		{"different digest", runtime.NudgePayloadDigest("another"), shape, "[Pasted text #12 +2 lines]", false},
		{"legacy no shape", digest, "", "[Pasted text #12 +2 lines]", false},
		{"edited", digest, shape, "[Pasted text #12 +2 lines] human", false},
		{"ambiguous", digest, shape, "[Pasted text #12 +2 lines]\n[Pasted text #13]", false},
		{"bad timestamp", digest, digest + ":bad:2", "[Pasted text #12 +2 lines]", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got := ownedPasteShape(tc.stored, tc.shape, tc.draft)
			if got != tc.want {
				t.Fatalf("match=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestNudgeHeldAgeAndStateChangeLogging(t *testing.T) {
	tm := NewTmux()
	stored := ""
	tm.exec = &fakeExecutor{fn: func(args []string) (string, error) {
		if slices.Contains(args, "show-options") {
			return stored, nil
		}
		if slices.Contains(args, "set-option") {
			stored = args[len(args)-1]
		}
		return "", nil
	}}
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previous)
	now := time.Unix(1700000000, 123)
	first, err := tm.nudgeHeldReceipt("pane", "human draft", "unowned_draft", time.Time{}, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := tm.nudgeHeldReceipt("pane", "human draft", "unowned_draft", time.Time{}, now.Add(8*time.Second))
	if err != nil || !second.Since.Equal(first.Since) || second.AgeMS != 8000 {
		t.Fatalf("held age reset: first=%+v second=%+v err=%v", first, second, err)
	}
	if strings.Count(logs.String(), "delivery held") != 1 {
		t.Fatalf("repeated hold logged twice: %s", logs.String())
	}
	_, err = tm.nudgeHeldReceipt("pane", "changed draft", "unowned_draft", time.Time{}, now.Add(9*time.Second))
	if err != nil || strings.Count(logs.String(), "delivery held") != 2 {
		t.Fatalf("state change not logged: %s err=%v", logs.String(), err)
	}
}

func TestNudgeHeldEvidenceSurvivesAgeStorageFailure(t *testing.T) {
	for _, operation := range []string{"show-options", "set-option"} {
		t.Run(operation, func(t *testing.T) {
			tm := NewTmux()
			failure := errors.New("tmux option unavailable")
			tm.exec = &fakeExecutor{fn: func(args []string) (string, error) {
				if slices.Contains(args, operation) {
					return "", failure
				}
				return "", nil
			}}
			held, err := tm.nudgeHeldReceipt("pane", "human draft", "unowned_draft", time.Time{}, time.Now())
			if !errors.Is(err, failure) || held == nil || held.State != "unowned_draft" || held.Since.IsZero() {
				t.Fatalf("hold evidence erased: held=%+v err=%v", held, err)
			}
		})
	}
}
