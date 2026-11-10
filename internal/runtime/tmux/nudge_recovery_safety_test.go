package tmux

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// recoveryPane models the pane's conditional option writes as well as its input.
// It keeps rejected ownership checks from accidentally passing a recovery test.
type recoveryPane struct {
	options         map[string]string
	draft           string
	busy, loseEnter bool
	failConfirm     bool
	submitted       []string
	enters          int
	onPaste         func()
	afterEnter      func()
}

var recoveryOptionCondition = regexp.MustCompile(`#\{==:#\{(@gc-[^}]+)\},([^}]*)\}`)

func (p *recoveryPane) condition(condition string) bool {
	matches := recoveryOptionCondition.FindAllStringSubmatch(condition, -1)
	if strings.HasPrefix(condition, "#{||:") {
		for _, m := range matches {
			if p.options[m[1]] == m[2] {
				return true
			}
		}
		return false
	}
	for _, m := range matches {
		if p.options[m[1]] != m[2] {
			return false
		}
	}
	return true
}

func (p *recoveryPane) enter() {
	p.enters++
	if p.loseEnter || p.draft == "" {
		return
	}
	p.submitted = append(p.submitted, p.draft)
	p.draft, p.busy = "", true
	if p.afterEnter != nil {
		p.afterEnter()
	}
}

func (p *recoveryPane) render() string {
	pane := "────\n❯ " + strings.ReplaceAll(p.draft, "\n", "\n  ") + "\n────"
	if p.busy {
		pane = "esc to interrupt\n" + pane
	}
	return pane
}

func (p *recoveryPane) exec(args []string) (string, error) {
	if i := slices.Index(args, "if-shell"); i >= 0 {
		r := args[i+1:]
		condition, command := r[3], r[4]
		if p.failConfirm && strings.Contains(condition, "},pending:") && strings.HasPrefix(command, "set-option") && !strings.Contains(command, "pending:") {
			p.failConfirm = false
			return "", errors.New("confirm unavailable")
		}
		if !p.condition(condition) {
			if len(r) < 6 {
				return "", nil
			}
			command = r[5]
		}
		switch {
		case strings.HasPrefix(command, "display-message"):
			return "draft-ownership-changed", nil
		case strings.HasPrefix(command, "capture-pane"):
			return p.render(), nil
		case strings.HasPrefix(command, "send-keys"):
			if strings.HasSuffix(command, " Enter") {
				p.enter()
			}
		case strings.HasPrefix(command, "set-option"):
			for _, statement := range strings.Split(command, " ; ") {
				fields := strings.Fields(statement)
				p.options[fields[len(fields)-2]] = strings.Trim(fields[len(fields)-1], "'\"")
			}
		}
		return "", nil
	}
	switch {
	case slices.Contains(args, "list-panes"):
		return "%0", nil
	case slices.Contains(args, "show-environment") && args[len(args)-1] == "GC_PROVIDER":
		return "GC_PROVIDER=claude", nil
	case slices.Contains(args, "capture-pane"):
		return p.render(), nil
	case slices.Contains(args, "display-message") && slices.Contains(args, "#{pane_id}"):
		return "%0", nil
	case slices.Contains(args, "display-message") && slices.Contains(args, "#{session_attached}"):
		return "0", nil
	case slices.Contains(args, "display-message") && slices.Contains(args, "#{pane_width}"):
		return "120", nil
	case slices.Contains(args, "show-hooks"):
		return args[len(args)-1] + `[0] set-option -p -t "%0" @gc-nudge-draft-payload ''`, nil
	case slices.Contains(args, "show-options"):
		return p.options[args[len(args)-1]], nil
	case slices.Contains(args, "set-option"):
		if slices.Contains(args[slices.Index(args, "set-option")+1:], "-u") {
			delete(p.options, args[len(args)-1])
			return "", nil
		}
		p.options[args[len(args)-2]] = args[len(args)-1]
	case slices.Contains(args, "paste-buffer"):
		if p.onPaste != nil {
			p.onPaste()
		}
	case slices.Contains(args, "send-keys") && args[len(args)-1] == "Enter":
		p.enter()
	}
	return "", nil
}

func newRecoveryPane(t *testing.T) (*Tmux, *recoveryPane) {
	t.Helper()
	t.Cleanup(func() { sessionNudgeLocks.Delete(t.Name()) })
	tm := NewTmux()
	p := &recoveryPane{options: map[string]string{}}
	tm.exec = &fakeExecutor{fn: p.exec}
	return tm, p
}

func (p *recoveryPane) paste(rendered string) func(string, string, time.Duration) (int, error) {
	return func(_, text string, _ time.Duration) (int, error) {
		p.draft = rendered
		if rendered == "" {
			p.draft = text
		}
		return len(text), nil
	}
}

func (p *recoveryPane) agePaste(t *testing.T, age time.Duration) {
	t.Helper()
	parts := strings.Split(p.options[nudgeDraftShapeOption], ":")
	if len(parts) != 5 {
		t.Fatalf("cannot age an unrecorded paste: %q", p.options[nudgeDraftShapeOption])
	}
	parts[1] = strconv.FormatInt(time.Now().Add(-age).UnixMilli(), 10)
	p.options[nudgeDraftShapeOption] = strings.Join(parts, ":")
}

func TestNudgeRecoveryRequiresExactObservedPlaceholder(t *testing.T) {
	for _, sameMessage := range []bool{false, true} {
		t.Run(map[bool]string{false: "different message", true: "same message"}[sameMessage], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tm, p := newRecoveryPane(t)
				p.loseEnter = true
				message := "one\ntwo\nthree"
				_, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), message, "", p.paste("[Pasted text #12 +2 lines]"))
				if err != nil {
					t.Fatal(err)
				}
				p.agePaste(t, time.Minute)
				p.loseEnter, p.enters, p.draft = false, 0, "[Pasted text #13 +2 lines]"
				if !sameMessage {
					message = "next nudge"
				}
				d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), message, "", p.paste(""))
				if !errors.Is(err, ErrNudgeDraftDeferred) || p.enters != 0 || d.Held == nil || d.Held.State != "unowned_draft" {
					t.Fatalf("foreign paste submitted: delivery=%+v err=%v enters=%d", d, err, p.enters)
				}
			})
		})
	}
}

func TestNudgeRecoveryInlineAndPending(t *testing.T) {
	for _, transient := range []string{"late rendering", "capture failure"} {
		t.Run(transient, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tm, p := newRecoveryPane(t)
				p.loseEnter = true
				missed := false
				tm.exec = &fakeExecutor{fn: func(args []string) (string, error) {
					if slices.Contains(args, "capture-pane") && p.options[nudgeDraftShapeOption] != "" && !missed {
						missed = true
						if transient == "capture failure" {
							return "", errors.New("capture temporarily unavailable")
						}
						return "────\n❯ \n────", nil
					}
					return p.exec(args)
				}}
				if _, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "one\ntwo", "", p.paste("[Pasted text #12 +1 lines]")); err != nil {
					t.Fatal(err)
				}
				p.agePaste(t, time.Minute)
				p.loseEnter = false
				d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "next", "", p.paste(""))
				if err != nil || !d.Landed() || len(p.submitted) != 2 {
					t.Fatalf("transient observation stranded paste: delivery=%+v err=%v submitted=%q", d, err, p.submitted)
				}
			})
		})
	}
	for _, rendering := range []string{"earlier short nudge", "[Pasted text #12 +2 lines]"} {
		t.Run(rendering, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tm, p := newRecoveryPane(t)
				p.loseEnter, p.failConfirm = true, true
				message := "earlier short nudge"
				if strings.HasPrefix(rendering, "[") {
					message = "one\ntwo\nthree"
				}
				_, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), message, "", p.paste(rendering))
				if err != nil {
					t.Fatal(err)
				}
				p.loseEnter = false
				p.agePaste(t, time.Minute)
				d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "next nudge", "", p.paste(""))
				if err != nil || !d.Landed() || !slices.Equal(p.submitted, []string{rendering, "next nudge"}) {
					t.Fatalf("own paste not recovered: delivery=%+v err=%v submitted=%q", d, err, p.submitted)
				}
			})
		})
	}
}

func TestNudgeReservationExpiresAfterAbandonedHandoff(t *testing.T) {
	for _, pause := range []string{"before snapshot", "before mutation"} {
		t.Run("duplicate cleanup "+pause, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tm, p := newRecoveryPane(t)
				p.loseEnter = true
				message := "one\ntwo"
				if _, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), message, "owner", p.paste("[Pasted text #12 +1 lines]")); err != nil {
					t.Fatal(err)
				}
				pending := true
				tm.cfg.NudgeReservationInvalidated = func(string, string) (bool, error) { return pending, nil }
				provider := &Provider{tm: tm}
				renewed := false
				tm.exec = &fakeExecutor{fn: func(args []string) (string, error) {
					boundary := slices.Contains(args, "list-panes")
					if pause == "before mutation" {
						boundary = slices.Contains(args, "if-shell") && strings.Contains(strings.Join(args, " "), nudgeDraftIdentityOption)
					}
					if !renewed && boundary {
						renewed = true
						// A second processor completes the same request while the
						// first is paused. The operator then clears the old input.
						if err := provider.ReleaseNudgeReservation(t.Name(), "owner", true); err != nil {
							t.Fatal(err)
						}
						pending = false
						p.draft, p.busy = "", false
						// Deliberately reuse both payload and retry identity: only
						// the paste generation distinguishes the renewed input.
						d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), message, "owner", p.paste("[Pasted text #13 +1 lines]"))
						if err != nil || d.Submit != runtime.NudgeSubmitUnconfirmed {
							t.Fatalf("renewed paste=%+v err=%v", d, err)
						}
					}
					return p.exec(args)
				}}
				if err := provider.ReleaseNudgeReservation(t.Name(), "owner", true); err != nil {
					t.Fatal(err)
				}
				if !renewed || p.options[nudgeDraftPayloadOption] != runtime.NudgePayloadDigest(message) {
					t.Fatal("stale cleanup invalidated the surviving owner's fresh paste")
				}
				p.loseEnter = false
				d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), message, "owner", func(string, string, time.Duration) (int, error) {
					t.Fatal("fresh owned draft was pasted again")
					return 0, nil
				})
				if err != nil || d.Submit != runtime.NudgeSubmitConfirmed || len(p.submitted) != 1 {
					t.Fatalf("survivor retry=%+v err=%v submitted=%v", d, err, p.submitted)
				}
			})
		})
	}
	t.Run("fresh paste waits for cancellation cleanup", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tm, _ := newRecoveryPane(t)
			tm.cfg.NudgeReservationInvalidated = func(string, string) (bool, error) { return true, nil }
			pastes := 0
			d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "fresh reminder", "owner", func(_, text string, _ time.Duration) (int, error) {
				pastes++
				return len(text), nil
			})
			if pastes != 0 || d.Delivered || !errors.Is(err, ErrNudgeDraftDeferred) || errors.Is(err, runtime.ErrNudgeDraftUnowned) {
				t.Fatalf("pending cleanup allowed fresh paste or terminalized survivor: pastes=%d delivery=%+v err=%v", pastes, d, err)
			}
		})
	})
	t.Run("fresh retry paste requires a new generation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tm, p := newRecoveryPane(t)
			writeErr := errors.New("paste generation unavailable")
			tm.exec = &fakeExecutor{fn: func(args []string) (string, error) {
				if slices.Contains(args, "set-option") && slices.Contains(args, nudgeDraftGenerationOption) {
					return "", writeErr
				}
				return p.exec(args)
			}}
			pastes := 0
			d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "fresh reminder", "owner", func(_, text string, _ time.Duration) (int, error) {
				pastes++
				return len(text), nil
			})
			if pastes != 0 || d.Delivered || !errors.Is(err, writeErr) {
				t.Fatalf("unfenced retry pasted: pastes=%d delivery=%+v err=%v", pastes, d, err)
			}
		})
	})
	t.Run("full cancellation retires receipt without enabling recovery", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tm, p := newRecoveryPane(t)
			p.loseEnter = true
			message := "one\ntwo"
			original := "[Pasted text #12 +1 lines]"
			if _, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), message, "owner", p.paste(original)); err != nil {
				t.Fatal(err)
			}
			p.options[recoveredNudgeOption("owner")] = "pending:" + runtime.NudgePayloadDigest(message)
			provider := &Provider{tm: tm}
			if err := provider.ReleaseNudgeReservation(t.Name(), "owner", true); err != nil {
				t.Fatal(err)
			}
			if err := provider.ReleaseNudgeReservation(t.Name(), "owner", false); err != nil {
				t.Fatal(err)
			}
			if p.options[recoveredNudgeOption("owner")] != "" {
				t.Fatal("terminal receipt not retired")
			}
			p.agePaste(t, 11*time.Minute)
			p.loseEnter, p.enters = false, 0
			d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "next", "", p.paste(""))
			if !errors.Is(err, runtime.ErrNudgeDraftUnowned) || d.Held == nil || p.enters != 0 || p.draft != original {
				t.Fatalf("full cancellation recovered: delivery=%+v err=%v enters=%d", d, err, p.enters)
			}
		})
	})
	t.Run("ambiguous pane cannot acknowledge cleanup", func(t *testing.T) {
		tm, p := newRecoveryPane(t)
		lookups := 0
		tm.exec = &fakeExecutor{fn: func(args []string) (string, error) {
			if slices.Contains(args, "list-panes") {
				lookups++
				if lookups == 1 {
					return "%shell", nil
				}
				return "%shell\n%agent", nil
			}
			return p.exec(args)
		}}
		if err := (&Provider{tm: tm}).ReleaseNudgeReservation(t.Name(), "owner", true); err == nil {
			t.Fatal("ambiguous pane acknowledged cleanup")
		}
	})
	t.Run("cleanup normalizes definitive absence", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			err    error
			absent bool
		}{
			{"session gone", ErrSessionNotFound, true},
			{"server gone", ErrNoServer, true},
			{"executable missing", errors.New("tmux executable not found"), false},
		} {
			for _, failAt := range []int{1, 2} {
				t.Run(fmt.Sprintf("%s lookup %d", tc.name, failAt), func(t *testing.T) {
					tm, _ := newRecoveryPane(t)
					lookups := 0
					tm.exec = &fakeExecutor{fn: func(args []string) (string, error) {
						if !slices.Contains(args, "list-panes") {
							t.Fatalf("cleanup continued after failed pane discovery: %v", args)
						}
						lookups++
						if lookups == failAt {
							return "", fmt.Errorf("lookup: %w", tc.err)
						}
						return "%agent", nil
					}}
					err := (&Provider{tm: tm}).ReleaseNudgeReservation(t.Name(), "owner", true)
					if errors.Is(err, runtime.ErrSessionNotFound) != tc.absent || !errors.Is(err, tc.err) {
						t.Fatalf("cleanup error=%v, want runtime absence=%t with original cause", err, tc.absent)
					}
				})
			}
		}
	})
	t.Run("failed pane discovery cannot acknowledge cleanup", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tm, p := newRecoveryPane(t)
			p.loseEnter = true
			if _, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "one\ntwo", "owner", p.paste("[Pasted text #12 +1 lines]")); err != nil {
				t.Fatal(err)
			}
			before := p.options[nudgeDraftPayloadOption]
			tm.exec = &fakeExecutor{fn: func(args []string) (string, error) {
				if slices.Contains(args, "list-panes") {
					return "", errors.New("pane lookup unavailable")
				}
				if slices.Contains(args, "show-options") && slices.Contains(args, exactPaneTarget(t.Name())) {
					return "", nil
				}
				return p.exec(args)
			}}
			provider := &Provider{tm: tm}
			if err := provider.ReleaseNudgeReservation(t.Name(), "owner", true); err == nil {
				t.Fatal("failed lookup was acknowledged as successful cleanup")
			}
			if p.options[nudgeDraftPayloadOption] != before {
				t.Fatal("failed lookup changed agent provenance")
			}
		})
	})
	t.Run("pending recovery resumes unchanged draft", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tm, p := newRecoveryPane(t)
			p.loseEnter = true
			message := "one\ntwo"
			if _, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), message, "owner", p.paste("[Pasted text #12 +1 lines]")); err != nil {
				t.Fatal(err)
			}
			p.options[recoveredNudgeOption("owner")] = "pending:" + runtime.NudgePayloadDigest(message)
			p.agePaste(t, 11*time.Minute)
			p.loseEnter = false
			d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), message, "owner", p.paste(""))
			if err != nil || !d.Landed() || d.Submit != runtime.NudgeSubmitConfirmed || len(p.submitted) != 1 {
				t.Fatalf("pending recovery stranded intact paste: delivery=%+v err=%v submitted=%q", d, err, p.submitted)
			}
		})
	})
	t.Run("pending recovery write failure sends no Enter", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tm, p := newRecoveryPane(t)
			p.loseEnter = true
			if _, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "one\ntwo", "owner", p.paste("[Pasted text #12 +1 lines]")); err != nil {
				t.Fatal(err)
			}
			p.agePaste(t, 11*time.Minute)
			p.loseEnter, p.enters = false, 0
			tm.exec = &fakeExecutor{fn: func(args []string) (string, error) {
				if slices.Contains(args, "set-option") && args[len(args)-2] == recoveredNudgeOption("owner") {
					return "", errors.New("pending receipt unavailable")
				}
				return p.exec(args)
			}}
			_, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "next", "", p.paste(""))
			if err == nil || p.enters != 0 || p.draft != "[Pasted text #12 +1 lines]" {
				t.Fatalf("sent before durable intent: err=%v enters=%d draft=%q", err, p.enters, p.draft)
			}
		})
	})
	t.Run("recovery receipt finalization failure prevents replay", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tm, p := newRecoveryPane(t)
			p.loseEnter = true
			message := "one\ntwo"
			if _, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), message, "owner", p.paste("[Pasted text #12 +1 lines]")); err != nil {
				t.Fatal(err)
			}
			p.agePaste(t, 11*time.Minute)
			p.loseEnter = false
			failed := false
			tm.exec = &fakeExecutor{fn: func(args []string) (string, error) {
				if slices.Contains(args, "set-option") && args[len(args)-2] == recoveredNudgeOption("owner") && len(p.submitted) > 0 && !failed {
					failed = true
					return "", errors.New("receipt write unavailable")
				}
				return p.exec(args)
			}}
			if _, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "next", "", p.paste("")); err == nil || !failed || len(p.submitted) != 1 {
				t.Fatalf("receipt failure not exercised: err=%v failed=%v submitted=%q", err, failed, p.submitted)
			}
			p.busy = false
			d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), message, "owner", p.paste(""))
			if err != nil || !d.Landed() || d.Submit != runtime.NudgeSubmitUnverified || len(p.submitted) != 1 || p.draft != "" {
				t.Fatalf("uncertain recovery replayed: delivery=%+v err=%v submitted=%q draft=%q", d, err, p.submitted, p.draft)
			}
		})
	})
	t.Run("late original submit survives failed recovery receipt", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tm, p := newRecoveryPane(t)
			p.loseEnter = true
			message := "one\ntwo"
			original := "[Pasted text #12 +1 lines]"
			if _, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), message, "owner", p.paste(original)); err != nil {
				t.Fatal(err)
			}
			p.agePaste(t, 11*time.Minute)
			p.loseEnter = false
			consumed := false
			tm.exec = &fakeExecutor{fn: func(args []string) (string, error) {
				if slices.Contains(args, "if-shell") && strings.Contains(strings.Join(args, " "), "capture-pane") && !consumed {
					consumed = true
					p.submitted = append(p.submitted, original)
					p.draft, p.busy = "", true
				}
				if slices.Contains(args, "set-option") && args[len(args)-2] == recoveredNudgeOption("owner") && consumed {
					return "", errors.New("receipt write unavailable")
				}
				return p.exec(args)
			}}
			if _, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "next", "", p.paste("")); err == nil || !consumed {
				t.Fatalf("late consumption not exercised: err=%v consumed=%v", err, consumed)
			}
			tm.exec = &fakeExecutor{fn: p.exec}
			p.busy = false
			d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), message, "owner", p.paste(""))
			if err != nil || d.Submit != runtime.NudgeSubmitUnverified || len(p.submitted) != 1 {
				t.Fatalf("late original submit replayed: delivery=%+v err=%v submitted=%q", d, err, p.submitted)
			}
		})
	})
	for _, next := range []string{"first\nremaining", "remaining", "unrelated"} {
		for _, failure := range []string{"pending", "unreadable", "before-enter"} {
			t.Run(fmt.Sprintf("durable invalidation %q %s", next, failure), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					tm, p := newRecoveryPane(t)
					p.loseEnter = true
					original := "[Pasted text #12 +1 lines]"
					if _, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "first\nremaining", "batch-owner", p.paste(original)); err != nil {
						t.Fatal(err)
					}
					checks := 0
					tm.cfg.NudgeReservationInvalidated = func(session, id string) (bool, error) {
						if session != t.Name() || id != "batch-owner" {
							t.Fatalf("checked wrong owner: %q %q", session, id)
						}
						checks++
						if failure == "before-enter" && checks == 1 {
							return false, nil
						}
						if failure == "unreadable" {
							return false, errors.New("queue unreadable")
						}
						return true, nil
					}
					p.agePaste(t, 11*time.Minute)
					p.loseEnter, p.enters = false, 0
					d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), next, "batch-owner", p.paste(""))
					if !errors.Is(err, runtime.ErrNudgeDraftUnowned) || p.enters != 0 || p.draft != original || d.Held == nil || d.Held.State != "unowned_draft" {
						t.Fatalf("durably canceled paste recovered: delivery=%+v err=%v enters=%d draft=%q", d, err, p.enters, p.draft)
					}
				})
			})
		}
	}
	t.Run("partial batch invalidates automatic recovery", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tm, p := newRecoveryPane(t)
			p.loseEnter = true
			original := "[Pasted text #12 +1 lines]"
			if _, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "withdrawn\nremaining", "batch-owner", p.paste(original)); err != nil {
				t.Fatal(err)
			}
			provider := &Provider{tm: tm}
			if err := provider.ReleaseNudgeReservation(t.Name(), "batch-owner", true); err != nil {
				t.Fatal(err)
			}
			p.agePaste(t, 11*time.Minute)
			p.loseEnter, p.enters = false, 0
			d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "remaining", "batch-owner", p.paste(""))
			if !errors.Is(err, ErrNudgeDraftDeferred) || p.enters != 0 || p.draft != original || d.Held == nil || d.Held.State != "unowned_draft" {
				t.Fatalf("partial batch resubmitted: delivery=%+v err=%v enters=%d draft=%q", d, err, p.enters, p.draft)
			}
		})
	})
	t.Run("survivor acknowledges recovery already in progress", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tm, p := newRecoveryPane(t)
			p.loseEnter = true
			if _, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "first\nremaining", "batch-owner", p.paste("[Pasted text #12 +1 lines]")); err != nil {
				t.Fatal(err)
			}
			p.agePaste(t, 11*time.Minute)
			p.loseEnter = false
			d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "remaining", "batch-owner", p.paste(""))
			if err != nil || !d.Landed() || len(p.submitted) != 1 {
				t.Fatalf("survivor duplicated after recovery: delivery=%+v err=%v submitted=%q", d, err, p.submitted)
			}
			provider := &Provider{tm: tm}
			if err := provider.ReleaseNudgeReservation(t.Name(), "batch-owner", true); err != nil {
				t.Fatal(err)
			}
			d, err = tm.nudgeSessionDeliveryWithIdentity(t.Name(), "remaining", "batch-owner", p.paste(""))
			if err != nil || !d.Landed() || len(p.submitted) != 1 {
				t.Fatalf("invalidation erased prior acknowledgement: delivery=%+v err=%v submitted=%q", d, err, p.submitted)
			}
		})
	})
	t.Run("inline whitespace edit is not owned", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tm, p := newRecoveryPane(t)
			p.loseEnter = true
			if _, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), `print("a b")`, "", p.paste("")); err != nil {
				t.Fatal(err)
			}
			p.agePaste(t, 11*time.Minute)
			p.loseEnter, p.enters, p.draft = false, 0, `print("ab")`
			d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "next nudge", "", p.paste(""))
			if !errors.Is(err, ErrNudgeDraftDeferred) || p.enters != 0 || d.Held == nil || d.Held.State != "unowned_draft" {
				t.Fatalf("edited inline input submitted: delivery=%+v err=%v enters=%d", d, err, p.enters)
			}
		})
	})
	synctest.Test(t, func(t *testing.T) {
		tm, p := newRecoveryPane(t)
		p.loseEnter = true
		_, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "one\ntwo\nthree", "lost-process", p.paste("[Pasted text #12 +2 lines]"))
		if err != nil {
			t.Fatal(err)
		}
		p.loseEnter, p.enters = false, 0
		p.agePaste(t, time.Minute)
		d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "next nudge", "", p.paste(""))
		if !errors.Is(err, ErrNudgeDraftDeferred) || d.Held == nil || d.Held.State != "own_paste" || p.enters != 0 {
			t.Fatalf("reservation not respected: delivery=%+v err=%v enters=%d", d, err, p.enters)
		}
		p.agePaste(t, 11*time.Minute)
		d, err = tm.nudgeSessionDeliveryWithIdentity(t.Name(), "next nudge", "", p.paste(""))
		if err != nil || !d.Landed() || len(p.submitted) != 2 {
			t.Fatalf("abandoned reservation stranded delivery: delivery=%+v err=%v submitted=%q", d, err, p.submitted)
		}
		p.busy = false
		d, err = tm.nudgeSessionDeliveryWithIdentity(t.Name(), "remaining subset", "lost-process", p.paste(""))
		if err != nil || !d.Landed() || len(p.submitted) != 2 {
			t.Fatalf("late owner duplicated recovered input: delivery=%+v err=%v submitted=%q", d, err, p.submitted)
		}
		p.draft = "preserved input"
		p.options[nudgeDraftIdentityOption] = "other-digest:other-owner"
		provider := &Provider{tm: tm}
		if err := provider.ReleaseNudgeReservation(t.Name(), "lost-process", false); err != nil {
			t.Fatal(err)
		}
		if p.options[recoveredNudgeOption("lost-process")] != "" || p.options[nudgeDraftIdentityOption] != "other-digest:other-owner" {
			t.Fatalf("release erased a different owner or retained recovered receipt: %v", p.options)
		}
		if err := provider.ReleaseNudgeReservation(t.Name(), "other-owner", false); err != nil {
			t.Fatal(err)
		}
		if p.options[nudgeDraftIdentityOption] != "other-digest:" || p.draft != "preserved input" || len(p.submitted) != 2 {
			t.Fatalf("reservation release modified input: options=%v draft=%q submitted=%q", p.options, p.draft, p.submitted)
		}
	})
}

func TestNudgeStartupFailureAllowsLaterRecovery(t *testing.T) {
	for _, failure := range []string{"cancellation", "hard error"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tm, p := newRecoveryPane(t)
				p.loseEnter = true
				p.onPaste = func() { p.draft = "[Pasted text #12 +2 lines]" }
				startup := newTmuxStartOps(tm, "", 0, runtime.Config{}, false)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				ops := &fakeStartOps{hasSessionResult: true}
				hardErr := errors.New("startup transport failed")
				sends := 0
				ops.sendKeysHook = func() {
					sends++
					if sends > 1 {
						ops.sendKeysErr = hardErr
						return
					}
					ops.sendKeysErr = startup.sendKeys(t.Name(), "one\ntwo\nthree")
					if failure == "cancellation" {
						cancel()
					}
				}
				ops.releaseNudgeHook = startup.releaseNudge
				wantErr := hardErr
				if failure == "cancellation" {
					wantErr = context.Canceled
				}
				if err := launchOrchestration(ctx, ops, t.Name(), runtime.Config{Nudge: "one\ntwo\nthree"}, time.Second); !errors.Is(err, wantErr) {
					t.Fatal(err)
				}
				p.loseEnter = false
				p.agePaste(t, time.Minute)
				d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "next nudge", "", p.paste(""))
				if err != nil || !d.Landed() || len(p.submitted) != 2 {
					t.Fatalf("delivery=%+v err=%v submitted=%q", d, err, p.submitted)
				}
			})
		})
	}
}

func TestNudgeCompletionPreservesConcurrentPaste(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, p := newRecoveryPane(t)
		second := NewTmux()
		second.exec = &fakeExecutor{fn: p.exec}
		secondSession := t.Name() + "/other-process"
		t.Cleanup(func() { sessionNudgeLocks.Delete(secondSession) })
		p.afterEnter = func() {
			p.afterEnter = nil
			p.loseEnter = true
			d, err := second.nudgeSessionDeliveryWithIdentity(secondSession, "later paste", "", p.paste(""))
			if err != nil || d.Submit != runtime.NudgeSubmitUnverified {
				t.Fatalf("second delivery=%+v err=%v", d, err)
			}
		}
		d, err := first.nudgeSessionDeliveryWithIdentity(t.Name(), "first paste", "", p.paste(""))
		if err != nil || d.Submit != runtime.NudgeSubmitConfirmed {
			t.Fatalf("first delivery=%+v err=%v", d, err)
		}
		if p.draft != "later paste" || p.options[nudgeDraftPayloadOption] != runtime.NudgePayloadDigest("later paste") {
			t.Fatalf("first sender erased second sender's provenance: draft=%q options=%v", p.draft, p.options)
		}
	})
}

func TestNudgeUnverifiedClearsConsumedPasteProvenance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tm, p := newRecoveryPane(t)
		p.busy = true
		d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "one\ntwo\nthree", "", p.paste("[Pasted text #12 +2 lines]"))
		if err != nil || d.Submit != runtime.NudgeSubmitUnverified {
			t.Fatalf("delivery=%+v err=%v", d, err)
		}
		if p.options[nudgeDraftPayloadOption] != "" {
			t.Fatalf("consumed paste retained provenance: %v", p.options)
		}
	})
}
