package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Once-per-step injection of the active formula step.
//
// The full step description is injected when a session first sees a step —
// at SessionStart (gc prime --hook) and again whenever the active step
// changes — and every UserPromptSubmit in between carries a one-line pointer
// instead. Before this the whole step (≈1–1.5k tokens for mol-do-work) was
// re-injected on every prompt with no change detection (Fable 5.1 prompt
// audit, jadegate scan G-3, 2026-09-01): the model retains a once-stated
// assignment, so the repeats were per-turn cost and transcript noise.
//
// State is one small JSON file per session under the city runtime root, keyed
// by the gc session (bead id, else the alias/id the hook resolved) and stamped
// with the provider's own conversation id from the hook stdin when the
// provider supplies one. A different conversation id or a different step
// re-injects in full. The state is best-effort: when it cannot be read or
// written, the full reminder is emitted, so a broken state file degrades to
// the previous per-prompt behavior, never to silence.

// wispStepInjectState is the record of the last step whose full description
// was injected for one session.
type wispStepInjectState struct {
	StepID         string `json:"step_id"`
	ConversationID string `json:"conversation_id,omitempty"`
	InjectedAt     string `json:"injected_at"`
}

// wispStepInjectionAtSessionStart is the SessionStart form: always the full
// reminder. When record is true it also stamps the step as injected, so the
// first UserPromptSubmit that follows emits the pointer rather than a second
// full copy; preview callers pass false so a diagnostic render leaves no
// trace.
func wispStepInjectionAtSessionStart(cityPath, sessionKey, conversationID string, record bool) string {
	b := resolveWispStepForInjection(cityPath)
	if b == nil {
		return ""
	}
	if record {
		recordWispStepInjected(wispStepStateCityPath(cityPath), sessionKey, conversationID, b.ID)
	}
	return formatWispStepReminder(b)
}

// wispStepInjectionForPrompt is the UserPromptSubmit form: the full reminder
// for a step this session+conversation has not seen, the pointer otherwise.
func wispStepInjectionForPrompt(cityPath, sessionKey, conversationID string) string {
	b := resolveWispStepForInjection(cityPath)
	if b == nil {
		return ""
	}
	return wispStepPromptInjection(wispStepStateCityPath(cityPath), sessionKey, conversationID, b)
}

// wispStepPromptInjection is the decision and render over an already resolved
// step; split from wispStepInjectionForPrompt so it can be exercised without a
// bead store. cityPath is where the state lives ("" disables the state and
// yields the full reminder every time).
func wispStepPromptInjection(cityPath, sessionKey, conversationID string, b *beads.Bead) string {
	if b == nil {
		return ""
	}
	if prev, ok := readWispStepInjectState(cityPath, sessionKey); ok && prev.StepID == b.ID && prev.ConversationID == conversationID {
		return formatWispStepPointer(b)
	}
	recordWispStepInjected(cityPath, sessionKey, conversationID, b.ID)
	return formatWispStepReminder(b)
}

// formatWispStepPointer is the short per-prompt form of formatWispStepReminder:
// which step is active and how to re-read it, without the description.
func formatWispStepPointer(b *beads.Bead) string {
	title := extmsg.SanitizeForSystemReminder(strings.TrimSpace(b.Title))
	return fmt.Sprintf(
		"<system-reminder>\nActive step: %s (%s). Its description was injected earlier in this conversation; run `gc bd show %s` to re-read it.\n</system-reminder>\n",
		title, b.ID, b.ID,
	)
}

// wispStepStateCityPath mirrors resolveWispStepForInjection's city fallback so
// the state lives beside the store the step was resolved from.
func wispStepStateCityPath(cityPath string) string {
	if strings.TrimSpace(cityPath) != "" {
		return cityPath
	}
	return strings.TrimSpace(os.Getenv("GC_CITY"))
}

// wispStepInjectStatePath is the per-session state file. The session key is
// hashed so any identity string (alias, bead id, qualified name) yields a safe
// file name.
func wispStepInjectStatePath(cityPath, sessionKey string) string {
	cityPath = strings.TrimSpace(cityPath)
	sessionKey = strings.TrimSpace(sessionKey)
	if cityPath == "" || sessionKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(sessionKey))
	return citylayout.RuntimePath(cityPath, "inject", "wisp-step", hex.EncodeToString(sum[:12])+".json")
}

func readWispStepInjectState(cityPath, sessionKey string) (wispStepInjectState, bool) {
	path := wispStepInjectStatePath(cityPath, sessionKey)
	if path == "" {
		return wispStepInjectState{}, false
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is derived from the city runtime root and a hashed key
	if err != nil {
		return wispStepInjectState{}, false
	}
	var st wispStepInjectState
	if err := json.Unmarshal(data, &st); err != nil || strings.TrimSpace(st.StepID) == "" {
		return wispStepInjectState{}, false
	}
	return st, true
}

// recordWispStepInjected stamps stepID as injected for the session. Failures
// are swallowed: the next prompt then finds no matching state and injects the
// full reminder again, which is the pre-change behavior.
func recordWispStepInjected(cityPath, sessionKey, conversationID, stepID string) {
	path := wispStepInjectStatePath(cityPath, sessionKey)
	if path == "" || strings.TrimSpace(stepID) == "" {
		return
	}
	data, err := json.Marshal(wispStepInjectState{
		StepID:         stepID,
		ConversationID: conversationID,
		InjectedAt:     time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = fsys.WriteFileAtomic(fsys.OSFS{}, path, data, 0o644)
}
