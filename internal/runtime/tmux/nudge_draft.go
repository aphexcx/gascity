package tmux

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/mattn/go-runewidth"
)

// nudgeDraftPayloadOption belongs to the pane, so paste provenance survives a
// transport restart but does not follow a replacement pane. It contains only
// a payload digest, never the message text.
const nudgeDraftPayloadOption = "@gc-nudge-draft-payload"

// nudgeDraftGenerationOption distinguishes renewed pastes even when both their
// payload and retry owner are unchanged. Cleanup compares it atomically.
const nudgeDraftGenerationOption = "@gc-nudge-draft-generation"

// nudgeDraftOwnershipHook invalidates provenance when a client attaches or switches sessions.
// Invalidation runs before user hooks, which may block. Checking that ordering
// on retry also detects another caller replacing or moving the clear.
func (t *Tmux) nudgeDraftOwnershipHook(session, target string, install bool) (string, error) {
	pane, err := t.run("display-message", "-p", "-t", exactPaneTarget(target), "#{pane_id}")
	if err != nil {
		return "", err
	}
	pane = strings.TrimSpace(pane)
	if _, err := strconv.ParseUint(strings.TrimPrefix(pane, "%"), 10, 64); err != nil || !strings.HasPrefix(pane, "%") {
		return "", fmt.Errorf("invalid pane identity %q", pane)
	}
	hook := fmt.Sprintf("set-option -p -t %q %s ''", pane, nudgeDraftPayloadOption)
	for _, event := range []string{"client-attached", "client-session-changed"} {
		hooks, err := t.run("show-hooks", "-t", exactPaneTarget(session), event)
		if err != nil {
			return "", err
		}
		firstLine, _, _ := strings.Cut(strings.TrimSpace(hooks), "\n")
		_, firstCommand, _ := strings.Cut(firstLine, " ")
		if firstCommand == hook || strings.HasPrefix(firstCommand, hook+" ; ") {
			continue
		}
		if !install {
			return "", fmt.Errorf("draft ownership hook %q is not first", event)
		}
		if strings.TrimSpace(hooks) == "" {
			// A local array masks the inherited global array. Preserve its
			// current commands after our non-blocking invalidation command.
			hooks, err = t.run("show-hooks", "-g", event)
			if err != nil {
				return "", err
			}
		}
		commands := []string{hook}
		for _, line := range strings.Split(strings.TrimSpace(hooks), "\n") {
			if line == "" || line == event {
				continue
			}
			_, command, ok := strings.Cut(line, " ")
			if !ok {
				return "", fmt.Errorf("cannot read attachment hook %q", event)
			}
			if command != hook {
				commands = append(commands, command)
			}
		}
		if _, err := t.run("set-hook", "-t", exactPaneTarget(session), event, strings.Join(commands, " ; ")); err != nil {
			return "", err
		}
	}
	return pane, nil
}

// stageNudgeDraftProvenance marks a detached fresh paste as pending. An attached
// stage gets a marker usable only by this call, never by resume or recovery.
// A later attachment clears the marker even if it occurs during the paste.
func (t *Tmux) stageNudgeDraftProvenance(pane, message string, deliveryID ...string) error {
	id := ""
	if len(deliveryID) > 0 {
		id = deliveryID[0]
	}
	if _, err := t.run("set-option", "-p", "-t", exactPaneTarget(pane), nudgeDraftGenerationOption, rand.Text()); err != nil {
		return err
	}
	if _, err := t.run("set-option", "-p", "-t", exactPaneTarget(pane), nudgeDraftIdentityOption,
		runtime.NudgePayloadDigest(message)+":"+id); err != nil {
		return err
	}
	// Shape contains no payload text. It is useful only beside the matching,
	// confirmed digest, which the attachment hooks invalidate atomically.
	if _, err := t.run("set-option", "-p", "-t", exactPaneTarget(pane), nudgeDraftShapeOption,
		fmt.Sprintf("%s:%d:%d:%s:", runtime.NudgePayloadDigest(message), time.Now().UnixMilli(), strings.Count(message, "\n"), normalizedNudgeDigest(message))); err != nil {
		return err
	}
	digest := runtime.NudgePayloadDigest(message)
	_, err := t.run("if-shell", "-F", "-t", exactPaneTarget(pane), "#{==:#{session_attached},0}",
		shellquote.Join([]string{"set-option", "-p", "-t", exactPaneTarget(pane), nudgeDraftPayloadOption, "pending:" + digest}),
		shellquote.Join([]string{"set-option", "-p", "-t", exactPaneTarget(pane), nudgeDraftPayloadOption, "attached:" + digest}))
	return err
}

// confirmNudgeDraftProvenance promotes a full paste only if no attachment
// invalidated its pending marker. Attached-stage markers are never promoted to
// retry provenance. The comparison and update run in tmux.
func (t *Tmux) confirmNudgeDraftProvenance(pane, message string) error {
	return t.confirmNudgeDraftDigest(pane, runtime.NudgePayloadDigest(message))
}

func (t *Tmux) confirmNudgeDraftDigest(pane, digest string) error {
	_, err := t.run("if-shell", "-F", "-t", exactPaneTarget(pane),
		fmt.Sprintf("#{==:#{%s},pending:%s}", nudgeDraftPayloadOption, digest),
		fmt.Sprintf("set-option -p -t %q %s %s", pane, nudgeDraftPayloadOption, digest))
	return err
}

func normalizedNudgeDigest(message string) string {
	// The composer trims outer padding. Interior spaces, tabs and explicit
	// newlines can change a command's meaning and remain part of its identity.
	return runtime.NudgePayloadDigest(strings.TrimSpace(message))
}

// waitForNudgeDraftRendering leaves a third of the remaining lock budget for
// confirmation and I/O after observing a Claude paste.
func (t *Tmux) waitForNudgeDraftRendering(target, message, prefix string, lockDeadline time.Time) (string, error) {
	ctx, cancel := context.WithDeadline(context.Background(), lockDeadline)
	defer cancel()
	remaining := time.Until(lockDeadline)
	budget := min(20*time.Second, remaining/3*2)
	if budget <= 0 {
		return t.recordNudgeDraftRendering(ctx, target, message, prefix, true)
	}
	ctx, stop := context.WithTimeout(ctx, budget)
	defer stop()
	for {
		recorded, err := t.recordNudgeDraftRendering(ctx, target, message, prefix, true)
		if recorded != "" || errors.Is(err, ErrNudgeDraftDeferred) {
			return recorded, err
		}
		select {
		case <-ctx.Done():
			return "", err
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// recordNudgeDraftRendering binds recovery to what the completed paste actually
// displayed. An opaque placeholder needs its counter as well as its line count.
func (t *Tmux) recordNudgeDraftRendering(ctx context.Context, target, message, prefix string, claude bool) (string, error) {
	shape, err := t.runCtx(ctx, "show-options", "-p", "-qv", "-t", exactPaneTarget(target), nudgeDraftShapeOption)
	if err != nil {
		return "", err
	}
	parts := strings.Split(strings.TrimSpace(shape), ":")
	digest := runtime.NudgePayloadDigest(message)
	if len(parts) != 5 || parts[0] != digest {
		return "", fmt.Errorf("%w: paste shape changed before rendering observation", ErrNudgeDraftDeferred)
	}
	if !claude && parts[4] != "" {
		return parts[4], nil
	}
	payload, err := t.runCtx(ctx, "show-options", "-p", "-qv", "-t", exactPaneTarget(target), nudgeDraftPayloadOption)
	if err != nil {
		return "", err
	}
	payload = strings.TrimSpace(payload)
	attachedStage := claude && payload == "attached:"+digest
	if !attachedStage && payload != digest && payload != "pending:"+digest {
		return "", fmt.Errorf("%w: paste ownership changed before rendering observation", ErrNudgeDraftDeferred)
	}
	content, err := t.runCtx(ctx, "capture-pane", "-p", "-e", "-J", "-t", exactPaneTarget(target), "-S", fmt.Sprintf("-%d", promptObservationLines))
	if err != nil {
		return "", err
	}
	reading := readNudgeComposer(content, prefix, claude)
	draft, observed := reading.draft, reading.observed
	if !observed || draft == "" {
		return "", nil
	}
	collapsed := collapsedNudgeDraftMatches(draft, message)
	if !collapsed && !reading.matches(message, 0) {
		widthText, err := t.runCtx(ctx, "display-message", "-p", "-t", exactPaneTarget(target), "#{pane_width}")
		if err != nil {
			return "", fmt.Errorf("reading pasted composer width: %w", err)
		}
		width, err := strconv.Atoi(strings.TrimSpace(widthText))
		if err != nil || width <= 2 {
			return "", fmt.Errorf("invalid pasted composer width %q", widthText)
		}
		if !reading.matches(message, width-2) {
			return "", fmt.Errorf("pasted composer does not match the payload")
		}
	}
	if attachedStage {
		// Only this fresh call may use this observation. An attached pane
		// must never acquire a reusable rendering mark.
		return draft, nil
	}
	if !collapsed {
		// Inline recovery compares this slot to the normalized rendering,
		// including its observed wrap boundaries, rather than the payload.
		parts[3] = normalizedNudgeDigest(draft)
	}
	parts[4] = runtime.NudgePayloadDigest(draft)
	out, err := t.runCtx(ctx, "if-shell", "-F", "-t", exactPaneTarget(target),
		fmt.Sprintf("#{&&:#{==:#{%s},%s},#{&&:#{==:#{%s},%s},#{==:#{session_attached},0}}}", nudgeDraftShapeOption, strings.TrimSpace(shape), nudgeDraftPayloadOption, payload),
		shellquote.Join([]string{"set-option", "-p", "-t", exactPaneTarget(target), nudgeDraftShapeOption, strings.Join(parts, ":")}),
		"display-message -p draft-ownership-changed")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out) != "" {
		return "", fmt.Errorf("%w: paste ownership changed during rendering observation", ErrNudgeDraftDeferred)
	}
	return draft, nil
}

func (t *Tmux) clearNudgeDraftProvenance(target, digest string) error {
	_, err := t.run("if-shell", "-F", "-t", exactPaneTarget(target),
		nudgeDraftDigestCondition(digest, false),
		shellquote.Join([]string{"set-option", "-p", "-t", exactPaneTarget(target), nudgeDraftPayloadOption, ""}))
	return err
}

// sendNudgeDraftSubmitSequence checks ownership in the same tmux command that
// sends each key, so an attachment during the debounce cannot reuse an old read.
func (t *Tmux) sendNudgeDraftSubmitSequence(target, message string, keys []string, requireDetached bool) (bool, error) {
	return t.sendOwnedNudgeDraftSubmitSequence(target, runtime.NudgePayloadDigest(message), keys, requireDetached)
}

func (t *Tmux) sendOwnedNudgeDraftSubmitSequence(target, digest string, keys []string, requireDetached bool) (bool, error) {
	condition := nudgeDraftDigestCondition(digest, requireDetached)
	sent := false
	for i, key := range keys {
		if i > 0 {
			time.Sleep(nudgeSubmitKeySettle)
		}
		out, err := t.run("if-shell", "-F", "-t", exactPaneTarget(target), condition,
			shellquote.Join([]string{"send-keys", "-t", exactPaneTarget(target), key}),
			"display-message -p draft-ownership-changed")
		if err != nil {
			return sent, err
		}
		if strings.TrimSpace(out) != "" {
			return sent, fmt.Errorf("%w: draft ownership changed before submit to %q", ErrNudgeDraftDeferred, target)
		}
		sent = true
	}
	return sent, nil
}

func nudgeDraftDigestCondition(digest string, requireDetached bool) string {
	condition := fmt.Sprintf("#{||:#{==:#{%s},%s},#{==:#{%s},pending:%s}}", nudgeDraftPayloadOption, digest, nudgeDraftPayloadOption, digest)
	// A fresh paste can be delivered while a client is already attached.
	// Resuming or recovering an earlier paste still requires a detached pane.
	if requireDetached {
		condition = fmt.Sprintf("#{&&:%s,#{==:#{session_attached},0}}", condition)
	} else {
		condition = fmt.Sprintf("#{||:%s,#{==:#{%s},attached:%s}}", condition, nudgeDraftPayloadOption, digest)
	}
	return condition
}

// captureNudgeDraftSubmit observes acceptance only while the draft still belongs
// to this payload. Ownership and capture share a tmux command so a later attach
// cannot invalidate the snapshot that already established confirmation.
func (t *Tmux) captureNudgeDraftSubmit(target, message string, requireDetached bool) ([]string, error) {
	lines, err := t.captureOwnedNudgeDraftInput(target, runtime.NudgePayloadDigest(message), requireDetached)
	if err != nil {
		return nil, err
	}
	return strings.Split(runtime.RenderPeek(strings.Join(lines, "\n")).Output, "\n"), nil
}

func (t *Tmux) captureOwnedNudgeDraftInput(target, digest string, requireDetached bool) ([]string, error) {
	out, err := t.run("if-shell", "-F", "-t", exactPaneTarget(target), nudgeDraftDigestCondition(digest, requireDetached),
		shellquote.Join([]string{"capture-pane", "-p", "-e", "-t", exactPaneTarget(target), "-S", fmt.Sprintf("-%d", promptObservationLines)}),
		"display-message -p draft-ownership-changed")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out) == "draft-ownership-changed" {
		return nil, fmt.Errorf("%w: draft ownership changed before observation of %q", ErrNudgeDraftDeferred, target)
	}
	return strings.Split(out, "\n"), nil
}

// capturedPromptRemainder preserves Claude's literal input and its screen
// column, including the space or nonbreaking space after the prompt glyph.
func capturedPromptRemainder(line, prefix string, claude bool) (string, int, bool) {
	if !claude {
		remainder, ok := lastComposerRemainder([]string{line}, prefix)
		return remainder, -1, ok
	}
	fullWidth := runewidth.StringWidth(line)
	line = stripLeadingBoxBorder(strings.TrimLeft(line, " \t\u00a0"))
	prefix = strings.ReplaceAll(prefix, "\u00a0", " ")
	if strings.TrimSpace(line) == strings.TrimSpace(prefix) {
		return "", fullWidth, true
	}
	for _, want := range prefix {
		got, size := utf8.DecodeRuneInString(line)
		if got == '\u00a0' {
			got = ' '
		}
		if size == 0 || got != want {
			return "", 0, false
		}
		line = line[size:]
	}
	// show-environment trims the configured prefix. Account for the
	// separator that the terminal still paints after the prompt glyph.
	if strings.TrimRight(prefix, " \u00a0") == prefix {
		if rest, found := strings.CutPrefix(line, " "); found {
			line = rest
		} else {
			line = strings.TrimPrefix(line, "\u00a0")
		}
	}
	return line, fullWidth - runewidth.StringWidth(line), true
}

func nudgeComposer(content, prefix string) (string, bool) {
	reading := readNudgeComposer(content, prefix, false)
	return reading.draft, reading.observed
}

type composerReading struct {
	draft, literal string
	inputColumn    int
	observed       bool
}

func (r composerReading) matches(message string, width int) bool {
	return nudgeDraftMatches(r.draft, message, width) ||
		(r.inputColumn >= 0 && matchNudgeRows(r.literal, message, width, r.inputColumn))
}

// readNudgeComposer keeps the observed input column and literal row padding
// when matching Claude pastes. Other providers retain their existing reader.
func readNudgeComposer(content, prefix string, claude bool) composerReading {
	lines := strings.Split(runtime.ComposerInputText(content), "\n")
	start := -1
	frame, promptIndent := -1, 0
	inputColumn := -1
	var draft string
	for i, line := range lines {
		if composerFrameBoundary(line) {
			frame = i
			continue
		}
		remainder, column, ok := capturedPromptRemainder(line, prefix, claude)
		if ok {
			candidate := line
			if trimmed := strings.TrimLeft(line, " \t"); strings.HasPrefix(trimmed, "│") || strings.HasPrefix(trimmed, "┃") {
				// Only a vertical border in the frame column is structural.
				// An indented box is continuation text, even if it contains ❯.
				if trimmed != line {
					continue
				}
				candidate = strings.TrimPrefix(strings.TrimPrefix(line, "│"), "┃")
			}
			indent := len(candidate) - len(strings.TrimLeft(candidate, " \t"))
			if start < 0 || start < frame || indent <= promptIndent {
				inputColumn = column
				start, draft, promptIndent = i, remainder, indent
			}
		}
	}
	if start < 0 {
		return composerReading{}
	}
	border := "│"
	if strings.HasPrefix(lines[start], "┃") {
		border = "┃"
	}
	boxed := strings.HasPrefix(lines[start], border)
	if boxed && !claude {
		draft = strings.TrimSuffix(strings.TrimSpace(draft), border)
	} else if boxed {
		draft = strings.TrimSuffix(strings.TrimSuffix(strings.TrimRight(draft, " "), border), " ")
	}
	if !claude {
		draft = strings.TrimSpace(draft)
	}
	parts := []string{draft}
	framed := false
	for _, line := range lines[start+1:] {
		if composerFrameBoundary(line) {
			framed = true
			break
		}
	}
	if boxed && !framed {
		return composerReading{}
	}
	// Codex's original reader recognizes a single footer after a blank row.
	footer := false
	if !claude && !framed && !boxed && strings.TrimSpace(prefix) == "›" {
		end := len(lines)
		for end > start+1 && strings.TrimSpace(lines[end-1]) == "" {
			end--
		}
		if end > start+2 && strings.TrimSpace(lines[end-2]) == "" && codexComposerFooter(lines[end-1]) {
			lines = lines[:end-2]
			footer, framed = true, true
		}
	}
	// An attributed placeholder is evidence of empty input. Without a frame
	// or that signal, a blank row cannot distinguish a footer from more input.
	emptySuggestion := false
	if !framed && strings.TrimSpace(draft) == "" {
		rawLines := strings.Split(runtime.RenderPeek(content).Output, "\n")
		rawDraft, ok := lastComposerRemainder([]string{rawLines[start]}, prefix)
		emptySuggestion = ok && strings.TrimSpace(rawDraft) != ""
	}
	for i, line := range lines[start+1:] {
		trimmed := strings.TrimSpace(line)
		if composerFrameBoundary(line) {
			break
		}
		if boxed {
			if !strings.HasPrefix(trimmed, border) {
				return composerReading{}
			}
			if !claude {
				parts = append(parts, strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, border), border)))
				continue
			}
			row := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(trimmed, border), border), " ")
			// The TUI aligns continuation text with the first input column.
			// Any additional indentation belongs to the paste.
			gutter := inputColumn - runewidth.StringWidth(border)
			padding := min(gutter, len(row)-len(strings.TrimLeft(row, " ")))
			parts = append(parts, row[padding:])
			continue
		}
		if footer && trimmed != "" && !strings.HasPrefix(line, "  ") {
			return composerReading{}
		}
		if !framed {
			if trimmed == "" {
				if strings.TrimSpace(strings.Join(lines[start+1+i:], "\n")) == "" {
					break
				}
				return composerReading{observed: emptySuggestion && strings.TrimSpace(strings.Join(parts, "\n")) == ""}
			}
			if !strings.HasPrefix(line, "  ") {
				return composerReading{}
			}
		}
		parts = append(parts, strings.TrimPrefix(line, "  "))
	}
	literal := strings.TrimSpace(strings.Join(parts, "\n"))
	for i := range parts {
		parts[i] = strings.TrimRight(parts[i], " ")
	}
	return composerReading{draft: strings.TrimSpace(strings.Join(parts, "\n")), literal: literal, inputColumn: inputColumn, observed: true}
}

// nudgeDraftMatches checks rendering compatibility, not payload identity.
// Callers need full-paste provenance before relying on a normalized match.
// A positive width permits renderer-inserted row boundaries standing for a
// space or nothing. Provenance establishes identity; the renderer's word-wrap
// and cursor margins need not be reconstructed. Explicit newlines and interior
// whitespace remain significant. A nonpositive width requires an exact match.
func nudgeDraftMatches(draft, message string, wrapWidth int) bool {
	return matchNudgeRows(draft, message, wrapWidth, -1)
}

func matchNudgeRows(draft, message string, wrapWidth, inputColumn int) bool {
	message = strings.TrimSpace(message)
	if draft == message {
		return true
	}
	if wrapWidth <= 0 {
		return false
	}
	rows := strings.Split(draft, "\n")
	positions := map[int]bool{0: true}
	for i, row := range rows {
		if runewidth.StringWidth(row) > wrapWidth {
			return false
		}
		next := make(map[int]bool)
		match := func(pos int) {
			if strings.HasPrefix(message[pos:], row) {
				next[pos+len(row)] = true
			}
			if inputColumn >= 0 {
				if n, ok := renderedNudgeRow(row, message[pos:], inputColumn); ok {
					next[pos+n] = true
				}
			}
		}
		for pos := range positions {
			switch {
			case i == 0:
				match(pos)
			case pos < len(message) && message[pos] == '\n':
				match(pos + 1)
			case row != "" && rows[i-1] != "":
				match(pos)
				if pos < len(message) && message[pos] == ' ' {
					match(pos + 1)
				}
			}
		}
		positions = next
	}
	return positions[len(message)]
}

// renderedNudgeRow accounts for terminal tab stops and end-of-row padding.
// A painted row must still contain every space the payload explicitly sent.
func renderedNudgeRow(row, message string, column int) (int, bool) {
	consumed := 0
	for row != "" {
		source := message[consumed:]
		if source == "" || source[0] == '\n' {
			return consumed, strings.Trim(row, " ") == ""
		}
		ch, size := utf8.DecodeRuneInString(source)
		painted := source[:size]
		if ch == '\t' {
			painted = strings.Repeat(" ", 8-column%8)
		}
		var ok bool
		if row, ok = strings.CutPrefix(row, painted); !ok {
			return 0, false
		}
		column += runewidth.StringWidth(painted)
		consumed += size
	}
	return consumed, true
}

var claudeCollapsedPaste = regexp.MustCompile(`^\[Pasted text #[1-9][0-9]*(?: \+([1-9][0-9]*) lines)?\]$`)

// collapsedNudgeDraftMatches checks the renderer's line-count evidence only;
// callers still need full-paste provenance and uninterrupted detached ownership.
func collapsedNudgeDraftMatches(draft, message string) bool {
	match := claudeCollapsedPaste.FindStringSubmatch(draft)
	if match == nil {
		return false
	}
	lines := 0
	if match[1] != "" {
		var err error
		lines, err = strconv.Atoi(match[1])
		if err != nil {
			return false
		}
	}
	return lines == strings.Count(message, "\n")
}

func codexComposerFooter(line string) bool {
	if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
		return false
	}
	modelEffort, cwd, ok := strings.Cut(strings.TrimSpace(line), " · ")
	fields := strings.Fields(modelEffort)
	if !ok || len(fields) != 2 || (cwd != "~" && !strings.HasPrefix(cwd, "~/") && !strings.HasPrefix(cwd, "/")) {
		return false
	}
	switch fields[1] {
	case "default", "none", "minimal", "low", "medium", "high", "xhigh":
		return true
	default:
		return false
	}
}

func composerFrameBoundary(line string) bool {
	// Continuation text is indented, even when it contains a box drawing.
	// Only a border in the composer's frame column can end the input.
	if strings.TrimLeft(line, " \t") != line {
		return false
	}
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "╭") || strings.HasPrefix(trimmed, "╰") ||
		(trimmed != "" && strings.Trim(trimmed, "─") == "")
}
