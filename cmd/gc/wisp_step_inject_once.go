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
// re-injects in full. The marker is recorded only AFTER the hook payload has
// been written (the callers run the returned callback on a successful write),
// so a failed write leaves no claim that the conversation saw the step. The
// state is best-effort: when it cannot be read or written, the full reminder
// is emitted, so a broken state file degrades to the previous per-prompt
// behavior, never to silence. Files untouched for wispStepInjectStateTTL are
// pruned opportunistically whenever a marker is recorded, so churn of
// ephemeral sessions does not grow the directory without bound; a pruned
// live session simply re-receives the full step once.

// wispStepInjectStateTTL is how long a per-session marker survives without a
// new record before an opportunistic prune removes it.
const wispStepInjectStateTTL = 48 * time.Hour

// wispStepInjectState is the record of the last step whose full description
// was injected for one session.
type wispStepInjectState struct {
	StepID         string `json:"step_id"`
	ConversationID string `json:"conversation_id,omitempty"`
	InjectedAt     string `json:"injected_at"`
}

// wispStepInjectionAtSessionStart is the SessionStart form: always the full
// reminder. When record is true the returned callback stamps the step as
// injected — the caller runs it once the hook payload has been written — so
// the first UserPromptSubmit that follows emits the pointer rather than a
// second full copy. Preview callers pass false and get a nil callback, so a
// diagnostic render leaves no trace.
func wispStepInjectionAtSessionStart(cityPath, sessionKey, conversationID string, record bool) (string, func()) {
	b := resolveWispStepForInjection(cityPath)
	if b == nil {
		return "", nil
	}
	var delivered func()
	if record {
		stateCity, stepID := wispStepStateCityPath(cityPath), b.ID
		delivered = func() { recordWispStepInjected(stateCity, sessionKey, conversationID, stepID) }
	}
	return formatWispStepReminder(b), delivered
}

// wispStepInjectionForPrompt is the UserPromptSubmit form: the full reminder
// for a step this session+conversation has not seen, the pointer otherwise.
// The callback (nil for the pointer) records the marker and must run only
// after the payload carrying the full reminder has been written.
func wispStepInjectionForPrompt(cityPath, sessionKey, conversationID string) (string, func()) {
	b := resolveWispStepForInjection(cityPath)
	if b == nil {
		return "", nil
	}
	return wispStepPromptInjection(wispStepStateCityPath(cityPath), sessionKey, conversationID, b)
}

// wispStepPromptInjection is the decision and render over an already resolved
// step; split from wispStepInjectionForPrompt so it can be exercised without a
// bead store. cityPath is where the state lives ("" disables the state and
// yields the full reminder every time). The returned callback records the
// marker; it is nil when the pointer was returned.
func wispStepPromptInjection(cityPath, sessionKey, conversationID string, b *beads.Bead) (string, func()) {
	if b == nil {
		return "", nil
	}
	if prev, ok := readWispStepInjectState(cityPath, sessionKey); ok && prev.StepID == b.ID && prev.ConversationID == conversationID {
		return formatWispStepPointer(b), nil
	}
	stepID := b.ID
	return formatWispStepReminder(b), func() { recordWispStepInjected(cityPath, sessionKey, conversationID, stepID) }
}

// chainAfterDelivery composes post-write callbacks, skipping nil ones; nil
// when both are nil so callers can keep testing for "nothing to run".
func chainAfterDelivery(fns ...func()) func() {
	var live []func()
	for _, fn := range fns {
		if fn != nil {
			live = append(live, fn)
		}
	}
	if len(live) == 0 {
		return nil
	}
	return func() {
		for _, fn := range live {
			fn()
		}
	}
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
	pruneWispStepInjectState(filepath.Dir(path), path, time.Now())
}

// pruneWispStepInjectState removes sibling marker files whose last record is
// older than wispStepInjectStateTTL. Best-effort and bounded by the directory
// listing; it runs only when a marker is recorded (a step change), never on
// the per-prompt read path. keep is the file just written and is never
// removed.
func pruneWispStepInjectState(dir, keep string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := now.Add(-wispStepInjectStateTTL)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if path == keep {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.Remove(path)
	}
}
