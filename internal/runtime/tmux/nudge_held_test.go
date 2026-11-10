package tmux

import (
	"bytes"
	"context"
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
	for _, scenario := range []string{"recover", "retry owner", "retry owner identical", "retry owner startup", "startup finished busy", "startup exhausted", "acknowledged retry", "retry owner release failure", "suggestion", "busy", "young", "disabled", "attached", "human identical", "empty metadata failure"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				t.Cleanup(func() { sessionNudgeLocks.Delete(t.Name()) })
				tm := NewTmux()
				var retirementLog bytes.Buffer
				if scenario == "retry owner release failure" {
					writer := log.Writer()
					log.SetOutput(&retirementLog)
					defer log.SetOutput(writer)
				}
				prior := runtime.NudgePayloadDigest("one\ntwo\nthree")
				options := map[string]string{
					nudgeDraftPayloadOption:    prior,
					"@gc-nudge-draft-shape":    fmt.Sprintf("%s:%d:2:%s:%s", prior, time.Now().Add(-time.Minute).UnixMilli(), runtime.NudgePayloadDigest("onetwothree"), runtime.NudgePayloadDigest("[Pasted text #12 +2 lines]")),
					"@gc-nudge-draft-identity": prior + ":",
				}
				if strings.HasPrefix(scenario, "retry owner") {
					options[nudgeDraftIdentityOption] = prior + ":queue-A"
				}
				draft, busy := "[Pasted text #12 +2 lines]", false
				startup := newTmuxStartOps(tm, "", 0, runtime.Config{}, false)
				terminalRetry := scenario == "acknowledged retry" || scenario == "retry owner release failure"
				startupFinished := scenario == "startup finished busy" || scenario == "startup exhausted"
				stagingStartup := scenario == "retry owner startup" || terminalRetry || startupFinished
				if stagingStartup {
					draft = ""
					clear(options)
					busy = terminalRetry
				}
				if scenario == "busy" {
					busy = true
				}
				if scenario == "young" {
					options[nudgeDraftShapeOption] = fmt.Sprintf("%s:%d:2:%s:%s", prior, time.Now().UnixMilli(), runtime.NudgePayloadDigest("onetwothree"), runtime.NudgePayloadDigest(draft))
				}
				if scenario == "disabled" {
					tm.cfg.DisableOpaquePasteRecovery = true
				}
				if scenario == "human identical" {
					draft = "next nudge"
					options[nudgeDraftPayloadOption] = ""
				}
				if scenario == "empty metadata failure" {
					draft = ""
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
					case slices.Contains(args, "if-shell") && strings.Contains(args[len(args)-1], "set-option") && strings.Contains(args[len(args)-1], nudgeDraftIdentityOption):
						if scenario == "retry owner release failure" {
							return "", errors.New("retry identity storage unavailable")
						}
						options[nudgeDraftIdentityOption] = prior + ":"
					case slices.Contains(args, "set-option"):
						if scenario == "empty metadata failure" {
							return "", errors.New("pane metadata unavailable")
						}
						options[args[len(args)-2]] = args[len(args)-1]
					case slices.Contains(args, "paste-buffer"):
						pastes++
						draft = "[Pasted text #12 +2 lines]"
					case slices.Contains(args, "send-keys") || (slices.Contains(args, "if-shell") && strings.HasPrefix(args[len(args)-2], "send-keys ")):
						if stagingStartup {
							return "", nil
						}
						submits++
						draft, busy = "", true
					}
					return "", nil
				}}
				if stagingStartup {
					switch {
					case startupFinished:
						ops := &fakeStartOps{hasSessionResult: true, paneBusyResult: scenario == "startup finished busy"}
						ops.sendKeysHook = func() {
							ops.sendKeysErr = startup.sendKeys(t.Name(), "one\ntwo\nthree")
							options[nudgeDraftPayloadOption] = prior
						}
						ops.releaseNudgeHook = startup.releaseNudge
						if err := launchOrchestration(context.Background(), ops, t.Name(), runtime.Config{Nudge: "one\ntwo\nthree"}, time.Second); err != nil {
							t.Fatal(err)
						}
					case terminalRetry:
						d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "one\ntwo\nthree", "queue-A", func(_, text string, _ time.Duration) (int, error) {
							pastes++
							draft = "[Pasted text #12 +2 lines]"
							return len(text), nil
						})
						if scenario == "retry owner release failure" {
							if err != nil || d.UnconfirmedSubmitError(t.Name()) != nil || d.Submit != runtime.NudgeSubmitUnverified || !d.Landed() {
								t.Fatalf("metadata failure made terminal delivery replayable: delivery=%+v err=%v", d, err)
							}
							if !strings.Contains(retirementLog.String(), "retiring nudge retry ownership") {
								t.Fatal("retirement failure was not logged")
							}
						} else if err != nil || d.UnconfirmedSubmitError(t.Name()) != nil || d.Submit != runtime.NudgeSubmitUnverified || !d.Landed() {
							t.Fatalf("busy delivery cannot be acknowledged: delivery=%+v err=%v", d, err)
						}
					default:
						if err := startup.sendKeys(t.Name(), "one\ntwo\nthree"); !errors.Is(err, runtime.ErrNudgeSubmitUnconfirmed) {
							t.Fatalf("staging startup: %v", err)
						}
					}
					if pastes != 1 {
						t.Fatalf("startup pasted %d times", pastes)
					}
					stagingStartup, pastes = false, 0
					busy = false
					options[nudgeDraftPayloadOption] = prior
					options[nudgeDraftShapeOption] = fmt.Sprintf("%s:%d:2:%s:%s", prior, time.Now().Add(-time.Minute).UnixMilli(), runtime.NudgePayloadDigest("onetwothree"), runtime.NudgePayloadDigest(draft))
				}
				message, identity := "next nudge", ""
				if scenario == "retry owner identical" {
					message, identity = "one\ntwo\nthree", "queue-B"
				}
				delivery, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), message, identity, func(_, text string, _ time.Duration) (int, error) {
					if draft != "" {
						t.Fatalf("new paste overwrote held draft %q", draft)
					}
					pastes++
					draft = text
					return len(text), nil
				})
				switch {
				case scenario == "empty metadata failure":
					if err == nil || delivery.Landed() || delivery.Held != nil || pastes != 0 || submits != 0 {
						t.Fatalf("empty composer reported a hold: delivery=%+v err=%v pastes=%d submits=%d", delivery, err, pastes, submits)
					}
				case scenario != "recover" && scenario != "suggestion" && scenario != "acknowledged retry" && !startupFinished:
					if !errors.Is(err, ErrNudgeDraftDeferred) || delivery.Landed() || delivery.Held == nil || pastes != 0 || submits != 0 {
						t.Fatalf("unsafe recovery: delivery=%+v err=%v pastes=%d submits=%d", delivery, err, pastes, submits)
					}
					want := "unowned_draft"
					if scenario == "busy" || scenario == "young" || strings.HasPrefix(scenario, "retry owner") {
						want = "own_paste"
					}
					if delivery.Held.State != want {
						t.Fatalf("held=%+v, want %s", delivery.Held, want)
					}
				case err != nil || !delivery.Landed() || delivery.Bytes != len(message) || pastes != 1 || submits != 2:
					t.Fatalf("delivery=%+v err=%v pastes=%d submits=%d; want recovery then exactly one new delivery", delivery, err, pastes, submits)
				}
				if strings.HasPrefix(scenario, "retry owner") && scenario != "retry owner release failure" {
					send := func(_, text string, _ time.Duration) (int, error) {
						pastes++
						draft = text
						return len(text), nil
					}
					if scenario == "retry owner startup" {
						if err := startup.sendKeys(t.Name(), "one\ntwo\nthree"); err != nil || pastes != 0 || submits != 1 || draft != "" {
							t.Fatalf("startup retry duplicated input: err=%v pastes=%d submits=%d draft=%q", err, pastes, submits, draft)
						}
					} else {
						retry, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "one\ntwo\nthree", "queue-A", send)
						if err != nil || !retry.Landed() || retry.Submit != runtime.NudgeSubmitConfirmed || pastes != 0 || submits != 1 || draft != "" {
							t.Fatalf("recovered queue retry duplicated input: receipt=%+v err=%v pastes=%d submits=%d draft=%q", retry, err, pastes, submits, draft)
						}
					}
					busy = false
					fresh, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), message, identity, send)
					if err != nil || !fresh.Landed() || pastes != 1 || submits != 2 {
						t.Fatalf("later message was suppressed after the retry owner resolved: receipt=%+v err=%v pastes=%d submits=%d", fresh, err, pastes, submits)
					}
				}
			})
		})
	}
}

func TestOwnedPasteShape(t *testing.T) {
	digest := runtime.NudgePayloadDigest("one\ntwo\nthree")
	shape := digest + ":1700000000000:2:" + runtime.NudgePayloadDigest("onetwothree") + ":" + runtime.NudgePayloadDigest("[Pasted text #12 +2 lines]")
	for _, tc := range []struct {
		name, stored, shape, draft string
		want                       bool
	}{
		{"owned", digest, shape, "[Pasted text #12 +2 lines]", true},
		{"wrong count", digest, shape, "[Pasted text #12 +3 lines]", false},
		{"no provenance", "", shape, "[Pasted text #12 +2 lines]", false},
		{"pending observed", "pending:" + digest, shape, "[Pasted text #12 +2 lines]", true},
		{"different counter", digest, shape, "[Pasted text #13 +2 lines]", false},
		{"inline whitespace", digest, shape, "one two\nthree", true},
		{"unobserved pending", "pending:" + digest, digest + ":1700000000000:2:" + runtime.NudgePayloadDigest("onetwothree") + ":", "[Pasted text #12 +2 lines]", false},
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
