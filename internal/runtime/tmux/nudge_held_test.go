package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
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
					case slices.Contains(args, "if-shell"):
						return (&recoveryPane{options: options}).exec(args)
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
	staged := digest + ":1700000000000:2:" + digest + ":"
	shape := staged + runtime.NudgePayloadDigest("[Pasted text #12 +2 lines]")
	for _, tc := range []struct {
		name, stored, shape, draft string
		want                       bool
	}{
		{"owned", digest, shape, "[Pasted text #12 +2 lines]", true},
		{"late rendering", digest, staged, "[Pasted text #12 +2 lines]", true},
		{"late wrong count", digest, staged, "[Pasted text #12 +3 lines]", false},
		{"late two placeholders", digest, staged, "[Pasted text #12 +2 lines]\n[Pasted text #13]", false},
		{"late attached", "attached:" + digest, staged, "[Pasted text #12 +2 lines]", false},
		{"late inline", digest, staged, "one\ntwo\nthree", false},
		{"wrong count", digest, shape, "[Pasted text #12 +3 lines]", false},
		{"no provenance", "", shape, "[Pasted text #12 +2 lines]", false},
		{"pending observed", "pending:" + digest, shape, "[Pasted text #12 +2 lines]", true},
		{"different counter", digest, shape, "[Pasted text #13 +2 lines]", false},
		{"inline outer padding", digest, shape, " \tone\ntwo\nthree\n", true},
		{"inline interior whitespace edit", digest, shape, "one two\nthree", false},
		{"unobserved pending", "pending:" + digest, digest + ":1700000000000:2:" + digest + ":", "[Pasted text #12 +2 lines]", false},
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
	t.Run("record rendering before retry", checkRecoveryBindsLatePlaceholder)
	t.Run("unseen paste age limit", checkUnseenPasteAgeLimit)
	t.Run("unseen paste retirement guards", checkUnseenPasteRetirementGuards)
	t.Run("empty composer before late render", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tm, pane := newRecoveryPane(t)
			const first = "one\ntwo\nthree"
			receipt, err := tm.nudgeSessionDeliveryWith(t.Name(), first, func(_, text string, _ time.Duration) (int, error) {
				return len(text), nil
			})
			if err != nil || receipt.Submit != runtime.NudgeSubmitUnconfirmed {
				t.Fatalf("unseen first paste: %+v, %v", receipt, err)
			}
			shape := pane.options[nudgeDraftShapeOption]
			for _, next := range []string{first, "next"} {
				_, err := tm.nudgeSessionDeliveryWith(t.Name(), next, func(_, _ string, _ time.Duration) (int, error) {
					t.Fatal("second paste before the first rendered")
					return 0, nil
				})
				if !errors.Is(err, runtime.ErrNudgeDraftUnmatched) {
					t.Fatalf("expected a queue hold for the unrendered paste, got %v", err)
				}
				if !errors.Is(err, ErrNudgeDraftDeferred) || pane.enters != 0 ||
					pane.options[nudgeDraftPayloadOption] != runtime.NudgePayloadDigest(first) || pane.options[nudgeDraftShapeOption] != shape {
					t.Fatalf("lost pending rendering: options=%v enters=%d err=%v", pane.options, pane.enters, err)
				}
			}
		})
	})
}

// An accepted paste that Claude never shows holds an empty composer for a
// bounded time. Every held attempt carries a hold receipt, nothing is pasted
// twice, and at the limit the mark is retired and the queue is delivered again.
func checkUnseenPasteAgeLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tm, pane := newRecoveryPane(t)
		limit := DefaultConfig().NudgeUnseenPasteAge
		if limit != 5*time.Minute {
			t.Fatalf("default unseen paste age = %s, want 5m", limit)
		}
		const first, second, third = "one\ntwo\nthree", "four\nfive\nsix", "seven"
		accepted := func(_, text string, _ time.Duration) (int, error) { return len(text), nil }
		refused := func(_, _ string, _ time.Duration) (int, error) {
			t.Fatal("pasted again while the earlier paste could still render")
			return 0, nil
		}
		receipt, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), first, "queue-A", accepted)
		if err != nil || receipt.Submit != runtime.NudgeSubmitUnconfirmed {
			t.Fatalf("unseen first paste: %+v, %v", receipt, err)
		}
		mark, shape := pane.options[nudgeDraftPayloadOption], pane.options[nudgeDraftShapeOption]
		if mark != runtime.NudgePayloadDigest(first) || !strings.HasSuffix(shape, ":") {
			t.Fatalf("unseen paste left no mark: payload=%q shape=%q", mark, shape)
		}

		// The clock starts at the first read of the empty composer, not at the paste.
		held := func(since time.Time) *runtime.NudgeHeld {
			t.Helper()
			var last *runtime.NudgeHeld
			for _, next := range [][2]string{{first, "queue-A"}, {second, "queue-B"}} {
				d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), next[0], next[1], refused)
				if !errors.Is(err, ErrNudgeDraftDeferred) || !errors.Is(err, runtime.ErrNudgeDraftUnmatched) || !errors.Is(err, runtime.ErrNudgePasteUnseen) {
					t.Fatalf("want a hold on the unseen paste, got %v", err)
				}
				if d.Landed() || d.Held == nil || d.Held.State != "own_paste" || (!since.IsZero() && !d.Held.Since.Equal(since)) {
					t.Fatalf("hold receipt = %+v (landed=%t), want own_paste since %s", d.Held, d.Landed(), since)
				}
				if pane.enters != 0 || pane.options[nudgeDraftPayloadOption] != mark || pane.options[nudgeDraftShapeOption] != shape {
					t.Fatalf("hold touched the pane: enters=%d options=%v", pane.enters, pane.options)
				}
				last, since = d.Held, d.Held.Since
			}
			return last
		}
		before := time.Now()
		if seen := held(time.Time{}); seen.Since.Before(before) || seen.AgeMS > time.Second.Milliseconds() {
			t.Fatalf("first sighting = %+v, want it at the first held attempt (%s)", seen, before)
		}
		for _, age := range []time.Duration{time.Minute, limit / 2, limit - time.Second} {
			since := pane.ageHold(t, age)
			if got := held(since); got.AgeMS < age.Milliseconds() || got.AgeMS >= limit.Milliseconds() {
				t.Fatalf("hold aged %s reported %+v", age, got)
			}
		}
		pane.ageHold(t, limit)

		// At the limit the mark is retired and the held item is pasted once.
		pastes := 0
		shown := func(rendered string) func(string, string, time.Duration) (int, error) {
			paste := pane.paste(rendered)
			return func(target, text string, timeout time.Duration) (int, error) {
				pastes++
				return paste(target, text, timeout)
			}
		}
		d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), first, "queue-A", shown("[Pasted text #1 +2 lines]"))
		if err != nil || !d.Landed() || d.Held != nil || d.Submit != runtime.NudgeSubmitConfirmed || pastes != 1 {
			t.Fatalf("held item after the limit: delivery=%+v err=%v pastes=%d", d, err, pastes)
		}
		pane.busy = false
		d, err = tm.nudgeSessionDeliveryWithIdentity(t.Name(), second, "queue-B", shown("[Pasted text #2 +2 lines]"))
		if err != nil || !d.Landed() || d.Submit != runtime.NudgeSubmitConfirmed || pastes != 2 {
			t.Fatalf("item behind the held one: delivery=%+v err=%v pastes=%d", d, err, pastes)
		}
		if want := []string{"[Pasted text #1 +2 lines]", "[Pasted text #2 +2 lines]"}; !slices.Equal(pane.submitted, want) {
			t.Fatalf("submitted %q, want each item exactly once: %q", pane.submitted, want)
		}

		// The lost paste shows up after its mark was retired. It is a draft
		// without provenance: held, never submitted.
		pane.busy, pane.draft = false, "[Pasted text #3 +2 lines]"
		enters := pane.enters
		d, err = tm.nudgeSessionDeliveryWithIdentity(t.Name(), third, "queue-C", refused)
		if !errors.Is(err, ErrNudgeDraftDeferred) || !errors.Is(err, runtime.ErrNudgeDraftUnowned) || errors.Is(err, runtime.ErrNudgePasteUnseen) {
			t.Fatalf("late paste after retirement: want an unowned draft hold, got %v", err)
		}
		if d.Landed() || d.Held == nil || d.Held.State != "unowned_draft" || pane.enters != enters || pane.draft != "[Pasted text #3 +2 lines]" {
			t.Fatalf("late paste after retirement: delivery=%+v enters=%d (was %d) draft=%q", d, pane.enters, enters, pane.draft)
		}
	})
}

// The limit runs only while no client is attached, and a mark that changed
// under the check is left alone.
func checkUnseenPasteRetirementGuards(t *testing.T) {
	for _, tc := range []string{"client attached", "mark replaced"} {
		t.Run(tc, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tm, pane := newRecoveryPane(t)
				const first = "one\ntwo\nthree"
				if _, err := tm.nudgeSessionDeliveryWith(t.Name(), first, func(_, text string, _ time.Duration) (int, error) { return len(text), nil }); err != nil {
					t.Fatal(err)
				}
				refused := func(_, _ string, _ time.Duration) (int, error) {
					t.Fatal("pasted while the unseen mark still stood")
					return 0, nil
				}
				if _, err := tm.nudgeSessionDeliveryWith(t.Name(), "next", refused); !errors.Is(err, runtime.ErrNudgePasteUnseen) {
					t.Fatalf("first sighting: %v", err)
				}
				pane.ageHold(t, 2*DefaultConfig().NudgeUnseenPasteAge)
				mark := pane.options[nudgeDraftPayloadOption]
				if tc == "client attached" {
					pane.attached = true
				} else {
					tm.exec = &fakeExecutor{fn: func(args []string) (string, error) {
						if command := strings.Join(args, " "); slices.Contains(args, "if-shell") && strings.Contains(command, "session_attached") && strings.Contains(command, nudgeDraftShapeOption) {
							pane.agePaste(t, time.Second) // another paste restaged the mark after it was read
						}
						return pane.exec(args)
					}}
				}
				d, err := tm.nudgeSessionDeliveryWith(t.Name(), "next", refused)
				if !errors.Is(err, runtime.ErrNudgePasteUnseen) || d.Held == nil || pane.enters != 0 {
					t.Fatalf("retired without proof: delivery=%+v err=%v enters=%d", d, err, pane.enters)
				}
				if pane.options[nudgeDraftPayloadOption] != mark {
					t.Fatalf("mark retired without proof: %q", pane.options[nudgeDraftPayloadOption])
				}
			})
		})
	}
}

// ageHold moves the recorded first sighting of the current hold into the past.
func (p *recoveryPane) ageHold(t *testing.T, age time.Duration) time.Time {
	t.Helper()
	parts := strings.Split(p.options[nudgeHeldOption], ":")
	if len(parts) != 3 {
		t.Fatalf("cannot age an unrecorded hold: %q", p.options[nudgeHeldOption])
	}
	since := time.Now().Add(-age).Truncate(time.Millisecond)
	parts[2] = strconv.FormatInt(since.UnixMilli(), 10)
	p.options[nudgeHeldOption] = strings.Join(parts, ":")
	return since
}

func checkRecoveryBindsLatePlaceholder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tm, pane := newRecoveryPane(t)
		const message = "one\ntwo\nthree"
		digest := runtime.NudgePayloadDigest(message)
		staged := fmt.Sprintf("%s:%d:2:%s:", digest, time.Now().UnixMilli(), digest)
		pane.options[nudgeDraftPayloadOption] = digest
		pane.options[nudgeDraftShapeOption] = staged
		pane.options[nudgeDraftIdentityOption] = digest + ":"
		pane.draft = "[Pasted text #1 +2 lines]"
		recoverPaste := func() (bool, error) {
			read := func(content string) composerReading {
				return readNudgeComposer(content, DefaultReadyPromptPrefix, true)
			}
			owned, _, err := tm.recoverEarlierNudgePaste(t.Name(), "%0", pane.draft, "new message", "", true, read)
			return owned, err
		}
		if recovered, err := recoverPaste(); recovered || !errors.Is(err, ErrNudgeDraftDeferred) || pane.enters != 0 {
			t.Fatalf("young paste: recovered=%t enters=%d err=%v", recovered, pane.enters, err)
		}
		if got := pane.options[nudgeDraftShapeOption]; got != staged+runtime.NudgePayloadDigest(pane.draft) {
			t.Fatalf("late rendering not recorded: %q", got)
		}
		pane.agePaste(t, time.Minute)
		pane.draft = "[Pasted text #2 +2 lines]"
		if recovered, err := recoverPaste(); recovered || err != nil || pane.enters != 0 {
			t.Fatalf("changed placeholder: recovered=%t enters=%d err=%v", recovered, pane.enters, err)
		}
		pane.draft = "[Pasted text #1 +2 lines]"
		if recovered, err := recoverPaste(); !recovered || err != nil || pane.enters != 1 {
			t.Fatalf("confirmed late paste: recovered=%t enters=%d err=%v", recovered, pane.enters, err)
		}
	})
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
