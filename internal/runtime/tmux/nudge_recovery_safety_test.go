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
	width           int
	attached        bool
}

var (
	recoveryFormatValue     = regexp.MustCompile(`#\{([^{}:,]+)\}`)
	recoveryFormatOperation = regexp.MustCompile(`#\{(==|&&|\|\|):([^{}]*),([^{}]*)\}`)
)

func (p *recoveryPane) condition(condition string) bool {
	condition = recoveryFormatValue.ReplaceAllStringFunc(condition, func(value string) string {
		key := recoveryFormatValue.FindStringSubmatch(value)[1]
		if key == "session_attached" {
			if p.attached {
				return "1"
			}
			return "0"
		}
		return p.options[key]
	})
	for recoveryFormatOperation.MatchString(condition) {
		condition = recoveryFormatOperation.ReplaceAllStringFunc(condition, func(operation string) string {
			m := recoveryFormatOperation.FindStringSubmatch(operation)
			if (m[1] == "==" && m[2] == m[3]) ||
				(m[1] == "&&" && m[2] == "1" && m[3] == "1") ||
				(m[1] == "||" && (m[2] == "1" || m[3] == "1")) {
				return "1"
			}
			return "0"
		})
	}
	return condition == "1"
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
				if strings.HasPrefix(statement, "display-message") {
					return "draft-ownership-changed", nil
				}
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
		if p.attached {
			return "1", nil
		}
		return "0", nil
	case slices.Contains(args, "display-message") && slices.Contains(args, "#{pane_width}"):
		if p.width != 0 {
			return strconv.Itoa(p.width), nil
		}
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

func TestNudgeAttachedEmptyComposer(t *testing.T) {
	for _, id := range []string{"", "owner"} {
		t.Run(id, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tm, p := newRecoveryPane(t)
				p.attached = true
				pastes := 0
				paste := func(target, text string, timeout time.Duration) (int, error) {
					pastes++
					return p.paste("")(target, text, timeout)
				}
				d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "reminder", id, paste)
				if err != nil || d.Submit != runtime.NudgeSubmitConfirmed || pastes != 1 || p.enters != 1 || !slices.Equal(p.submitted, []string{"reminder"}) {
					t.Fatalf("attached nudge lost: delivery=%+v err=%v pastes=%d Enters=%d submitted=%v", d, err, pastes, p.enters, p.submitted)
				}
			})
		})
	}
}

func TestNudgeAttachedEarlierPaste(t *testing.T) {
	for _, path := range []string{"resume", "recovery"} {
		t.Run(path, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tm, p := newRecoveryPane(t)
				const message = "earlier paste"
				p.loseEnter = true
				d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), message, "owner", p.paste(""))
				if err != nil || d.Submit != runtime.NudgeSubmitUnconfirmed {
					t.Fatalf("seed paste: delivery=%+v err=%v", d, err)
				}
				text, owner := message, "owner"
				if path == "recovery" {
					p.agePaste(t, 11*time.Minute)
					text, owner = "next paste", "next-owner"
				}
				enters, attachedAtGuard := p.enters, false
				p.loseEnter = false
				tm.exec = &fakeExecutor{fn: func(args []string) (string, error) {
					joined := strings.Join(args, " ")
					if slices.Contains(args, "if-shell") && (strings.Contains(joined, "send-keys") || strings.Contains(joined, "capture-pane")) {
						// Attach after the Go-side read, before the hook clears
						// provenance. The tmux attachment guard must suffice.
						p.attached, attachedAtGuard = true, true
					}
					return p.exec(args)
				}}
				d, err = tm.nudgeSessionDeliveryWithIdentity(t.Name(), text, owner, p.paste(""))
				if !errors.Is(err, ErrNudgeDraftDeferred) && d.Submit != runtime.NudgeSubmitOwnershipLost {
					t.Fatalf("attached earlier paste not held: delivery=%+v err=%v", d, err)
				}
				if !attachedAtGuard || p.enters != enters || len(p.submitted) != 0 || p.draft != message {
					t.Fatalf("attached earlier paste submitted: reached=%t Enters=%d submitted=%v draft=%q", attachedAtGuard, p.enters-enters, p.submitted, p.draft)
				}
			})
		})
	}
}

func TestNudgeAttachedStartupPrompt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tm, p := newRecoveryPane(t)
		p.attached = true
		const prompt = "startup prompt"
		pastes := 0
		p.onPaste = func() {
			pastes++
			p.draft = prompt
		}
		startup := newTmuxStartOps(tm, "", 0, runtime.Config{}, false)
		ops := &fakeStartOps{hasSessionResult: true}
		ops.sendKeysHook = func() {
			call := ops.calls[len(ops.calls)-1]
			ops.sendKeysErr = startup.sendKeys(call.name, call.command)
		}
		ops.releaseNudgeHook = startup.releaseNudge
		cfg := runtime.Config{Command: "claude", ReadyPromptPrefix: "❯", Nudge: prompt}
		if err := doStartSession(context.Background(), ops, t.Name(), cfg, DefaultConfig().SetupTimeout); err != nil {
			t.Fatal(err)
		}
		if sent := callsByMethod(t, ops, "sendKeys", 1)[0].command; sent != prompt {
			t.Fatalf("startup sent %q, want %q", sent, prompt)
		}
		callsByMethod(t, ops, "recordUnconfirmedNudge", 0)
		methods := ops.callMethods()
		if ready, send := methodIndex(methods, "waitForReady"), methodIndex(methods, "sendKeys"); ready < 0 || send <= ready {
			t.Fatalf("startup nudge did not follow readiness: %v", methods)
		}
		if pastes != 1 || p.enters != 1 || !slices.Equal(p.submitted, []string{prompt}) {
			t.Fatalf("startup prompt lost: pastes=%d Enters=%d submitted=%v", pastes, p.enters, p.submitted)
		}
	})
}

// These cells cover distinct fences, not twenty independent tmux guards:
//   - before paste: the pending-cleanup callback or the provenance read on an
//     existing draft/receipt; cleanup deliberately remains unacknowledged.
//   - after paste before Enter and between Enter resends: the tmux payload
//     condition on submit/observation, after cleanup has cleared provenance.
//   - after failed recovery and manual clear: every route converges on the same
//     pending-recovery receipt and its provenance read in recoveredNudgeStatus.
//
// Acknowledged cleanup before staging is outside these fences: a fresh stage
// can recreate provenance for the withdrawn retry (the queue contract in gf-ko8c).
func TestNudgeReservationInvalidationBoundaries(t *testing.T) {
	for _, path := range []string{"fresh paste", "resume", "recovery", "survivor retry"} {
		for _, boundary := range []string{"before paste", "after paste before Enter", "between Enter resends", "after failed recovery", "manual clear"} {
			t.Run(path+"/"+boundary, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					tm, p := newRecoveryPane(t)
					const message, owner = "first\nremaining", "batch-owner"
					const original = "[Pasted text #12 +1 lines]"
					success := func(d runtime.NudgeDelivery) bool {
						return d.Submit == runtime.NudgeSubmitConfirmed || d.Submit == runtime.NudgeSubmitUnverified
					}
					p.loseEnter = true
					paste := p.paste(original)
					failedRecovery := func() {
						t.Helper()
						p.agePaste(t, 11*time.Minute)
						d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "remaining", owner, paste)
						if !errors.Is(err, ErrNudgeDraftDeferred) || success(d) || p.options[recoveredNudgeOption(owner)] == "" {
							t.Fatalf("failed recovery not reached: delivery=%+v err=%v", d, err)
						}
					}
					if path != "fresh paste" {
						d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), message, owner, paste)
						if err != nil || d.Submit != runtime.NudgeSubmitUnconfirmed {
							t.Fatalf("seed paste: delivery=%+v err=%v", d, err)
						}
					}
					if path == "recovery" {
						p.agePaste(t, 11*time.Minute)
					}
					if path == "survivor retry" {
						failedRecovery()
					}
					invoke := func() (runtime.NudgeDelivery, error) {
						text, id := message, owner
						switch path {
						case "recovery":
							text, id = "next", "next-owner"
						case "survivor retry":
							text = "remaining"
						}
						return tm.nudgeSessionDeliveryWithIdentity(t.Name(), text, id, paste)
					}
					invalidated, entersAtCleanup, pastesAfterCleanup := false, 0, 0
					pending := false
					tm.cfg.NudgeReservationInvalidated = func(string, string) (bool, error) { return pending, nil }
					invalidate := func(manual bool) {
						t.Helper()
						if invalidated {
							return
						}
						invalidated, entersAtCleanup = true, p.enters
						if manual {
							// Attaching clears provenance before the person clears
							// input. Detaching again must not acknowledge it.
							p.options[nudgeDraftPayloadOption] = ""
							p.draft = ""
						} else {
							pending = true
							if err := (&Provider{tm: tm}).ReleaseNudgeReservation(t.Name(), owner, true); err != nil {
								t.Fatal(err)
							}
							pending = boundary == "before paste" // no paste exists to fence yet
						}
						p.loseEnter = false // an illegal Enter would now accept input
					}
					switch boundary {
					case "after failed recovery", "manual clear":
						d, _ := invoke()
						if success(d) {
							t.Fatalf("lost Enters reported success: %+v", d)
						}
						failedRecovery()
						invalidate(boundary == "manual clear")
						// Every route converges on the original batch's receipt.
						invoke = func() (runtime.NudgeDelivery, error) {
							return tm.nudgeSessionDeliveryWithIdentity(t.Name(), "remaining", owner, paste)
						}
					case "before paste":
						invalidate(false)
					}
					paste = func(target, text string, timeout time.Duration) (int, error) {
						if invalidated {
							pastesAfterCleanup++
						}
						n, err := p.paste(original)(target, text, timeout)
						if boundary == "after paste before Enter" {
							invalidate(false)
						}
						return n, err
					}
					guardedAcceptance := false
					tm.exec = &fakeExecutor{fn: func(args []string) (string, error) {
						joined := strings.Join(args, " ")
						isSubmit := strings.Contains(joined, "send-keys") && strings.Contains(joined, " Enter")
						if isSubmit && boundary == "after paste before Enter" {
							invalidate(false) // race after Go's last read, inside tmux
						}
						out, err := p.exec(args)
						if slices.Contains(args, "if-shell") && strings.Contains(joined, "capture-pane") &&
							out != "draft-ownership-changed" && p.draft == "" && len(p.submitted) > 0 {
							guardedAcceptance = true
						}
						if isSubmit && boundary == "between Enter resends" {
							invalidate(false)
						}
						return out, err
					}}
					d, err := invoke()
					if !invalidated {
						t.Fatal("cleanup boundary was not exercised")
					}
					if p.enters != entersAtCleanup || pastesAfterCleanup != 0 {
						t.Errorf("input after cleanup: Enters=%d pastes=%d", p.enters-entersAtCleanup, pastesAfterCleanup)
					}
					if success(d) && !guardedAcceptance {
						t.Errorf("acknowledged without guarded acceptance: delivery=%+v err=%v", d, err)
					}
					if boundary == "manual clear" && (err != nil || d.Submit != runtime.NudgeSubmitOwnershipLost) {
						t.Errorf("cleared pending receipt: delivery=%+v err=%v", d, err)
					}
					if boundary != "manual clear" && boundary != "before paste" && p.draft != original {
						t.Errorf("withdrawn draft not preserved: %q", p.draft)
					}
				})
			})
		}
	}
}

func TestNudgeWrappedOneLineRecovery(t *testing.T) {
	for _, scenario := range []string{"recover", "resize", "human draft"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tm, p := newRecoveryPane(t)
				p.width, p.loseEnter = 80, true
				message := strings.Repeat("a", p.width+60)
				rendering := message[:78] + "\n" + message[78:]
				if scenario == "human draft" {
					p.draft = "a human's unfinished instruction"
				} else {
					d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), message, "owner", p.paste(rendering))
					if err != nil || d.Submit != runtime.NudgeSubmitUnconfirmed {
						t.Fatalf("lost Enter: delivery=%+v err=%v", d, err)
					}
					shape := strings.Split(p.options[nudgeDraftShapeOption], ":")
					if len(shape) != 5 || shape[4] != runtime.NudgePayloadDigest(rendering) {
						t.Errorf("observed wrapping not recorded: %q", shape)
					}
					p.agePaste(t, 11*time.Minute)
					if scenario == "resize" {
						p.width = 100
						p.draft = message[:98] + "\n" + message[98:]
					}
				}
				before := p.draft
				p.loseEnter, p.enters = false, 0
				d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "next nudge", "next-owner", p.paste(""))
				if scenario == "recover" {
					if err != nil || !d.Landed() || !slices.Equal(p.submitted, []string{rendering, "next nudge"}) {
						t.Fatalf("wrapped paste not recovered: delivery=%+v err=%v submitted=%q", d, err, p.submitted)
					}
				} else if !errors.Is(err, ErrNudgeDraftDeferred) || d.Landed() || p.enters != 0 || p.draft != before {
					t.Fatalf("unproven input changed: delivery=%+v err=%v Enters=%d draft=%q", d, err, p.enters, p.draft)
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
	t.Run("survivor reports invalidation after unsuccessful recovery", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tm, p := newRecoveryPane(t)
			p.loseEnter = true
			original := "[Pasted text #12 +1 lines]"
			if _, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "first\nremaining", "batch-owner", p.paste(original)); err != nil {
				t.Fatal(err)
			}
			p.agePaste(t, 11*time.Minute)
			_, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "remaining", "batch-owner", p.paste(""))
			if !errors.Is(err, ErrNudgeDraftDeferred) || len(p.submitted) != 0 || !strings.HasPrefix(p.options[recoveredNudgeOption("batch-owner")], "pending:") {
				t.Fatalf("failed recovery did not leave pending input: err=%v submitted=%v options=%v", err, p.submitted, p.options)
			}
			if err := (&Provider{tm: tm}).ReleaseNudgeReservation(t.Name(), "batch-owner", true); err != nil {
				t.Fatal(err)
			}
			p.loseEnter, p.enters = false, 0
			d, err := tm.nudgeSessionDeliveryWithIdentity(t.Name(), "remaining", "batch-owner", func(string, string, time.Duration) (int, error) {
				t.Fatal("invalidated survivor was pasted again")
				return 0, nil
			})
			if err != nil || d.Submit != runtime.NudgeSubmitOwnershipLost || p.enters != 0 || p.draft != original || len(p.submitted) != 0 {
				t.Fatalf("unsubmitted survivor appeared delivered: delivery=%+v err=%v enters=%d draft=%q", d, err, p.enters, p.draft)
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
	for _, timing := range []string{"before observation", "after observation"} {
		t.Run(timing, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				first, p := newRecoveryPane(t)
				second := NewTmux()
				second.exec = &fakeExecutor{fn: p.exec}
				secondSession := t.Name() + "/other-process"
				t.Cleanup(func() { sessionNudgeLocks.Delete(secondSession) })
				replaced := false
				replace := func() {
					replaced = true
					p.afterEnter = nil
					p.loseEnter = true
					d, err := second.nudgeSessionDeliveryWithIdentity(secondSession, "later paste", "", p.paste(""))
					if err != nil || d.Submit != runtime.NudgeSubmitUnverified {
						t.Fatalf("second delivery=%+v err=%v", d, err)
					}
				}
				want := runtime.NudgeSubmitOwnershipLost
				if timing == "before observation" {
					p.afterEnter = replace
				} else {
					want = runtime.NudgeSubmitConfirmed
					first.exec = &fakeExecutor{fn: func(args []string) (string, error) {
						out, err := p.exec(args)
						if !replaced && slices.Contains(args, "if-shell") && strings.Contains(strings.Join(args, " "), "capture-pane") && len(p.submitted) > 0 {
							replace()
						}
						return out, err
					}}
				}
				d, err := first.nudgeSessionDeliveryWithIdentity(t.Name(), "first paste", "", p.paste(""))
				if err != nil || d.Submit != want || !replaced {
					t.Fatalf("first delivery=%+v err=%v replaced=%v; want %s", d, err, replaced, want)
				}
				if p.draft != "later paste" || p.options[nudgeDraftPayloadOption] != runtime.NudgePayloadDigest("later paste") {
					t.Fatalf("first sender erased second sender's provenance: draft=%q options=%v", p.draft, p.options)
				}
			})
		})
	}
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
