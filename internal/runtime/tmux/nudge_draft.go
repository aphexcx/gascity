package tmux

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/mattn/go-runewidth"
)

// nudgeDraftPayloadOption belongs to the pane, so paste provenance survives a
// transport restart but does not follow a replacement pane. It contains only
// a payload digest, never the message text.
const nudgeDraftPayloadOption = "@gc-nudge-draft-payload"

// nudgeDraftOwnershipHook binds provenance to uninterrupted detached input.
// Invalidation runs before user hooks, which may block. Checking that ordering
// on retry also detects another caller replacing or moving the clear.
func (t *Tmux) nudgeDraftOwnershipHook(session, target string, install bool) (string, error) {
	pane, err := t.run("display-message", "-p", "-t", target, "#{pane_id}")
	if err != nil {
		return "", err
	}
	pane = strings.TrimSpace(pane)
	if _, err := strconv.ParseUint(strings.TrimPrefix(pane, "%"), 10, 64); err != nil || !strings.HasPrefix(pane, "%") {
		return "", fmt.Errorf("invalid pane identity %q", pane)
	}
	hook := fmt.Sprintf("set-option -p -t %q %s ''", pane, nudgeDraftPayloadOption)
	for _, event := range []string{"client-attached", "client-session-changed"} {
		hooks, err := t.run("show-hooks", "-t", session, event)
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
		if _, err := t.run("set-hook", "-t", session, event, strings.Join(commands, " ; ")); err != nil {
			return "", err
		}
	}
	return pane, nil
}

// stageNudgeDraftProvenance marks a paste as pending only while detached.
// An attachment clears the marker even if it occurs during the paste.
func (t *Tmux) stageNudgeDraftProvenance(pane, message string) error {
	set := fmt.Sprintf("set-option -p -t %q %s ", pane, nudgeDraftPayloadOption)
	_, err := t.run("if-shell", "-F", "-t", pane, "#{==:#{session_attached},0}",
		set+"pending:"+runtime.NudgePayloadDigest(message), set+"''")
	return err
}

// confirmNudgeDraftProvenance promotes a full paste only if no attachment
// invalidated its pending marker. The comparison and update run in tmux.
func (t *Tmux) confirmNudgeDraftProvenance(pane, message string) error {
	digest := runtime.NudgePayloadDigest(message)
	_, err := t.run("if-shell", "-F", "-t", pane,
		fmt.Sprintf("#{==:#{%s},pending:%s}", nudgeDraftPayloadOption, digest),
		fmt.Sprintf("set-option -p -t %q %s %s", pane, nudgeDraftPayloadOption, digest))
	return err
}

// sendNudgeDraftSubmitSequence checks ownership in the same tmux command that
// sends each key, so an attachment during the debounce cannot reuse an old read.
func (t *Tmux) sendNudgeDraftSubmitSequence(target, message string, keys []string) (bool, error) {
	condition := nudgeDraftOwnershipCondition(message)
	sent := false
	for i, key := range keys {
		if i > 0 {
			time.Sleep(nudgeSubmitKeySettle)
		}
		out, err := t.run("if-shell", "-F", "-t", target, condition,
			shellquote.Join([]string{"send-keys", "-t", target, key}),
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

func nudgeDraftOwnershipCondition(message string) string {
	return fmt.Sprintf("#{&&:#{==:#{%s},%s},#{==:#{session_attached},0}}", nudgeDraftPayloadOption, runtime.NudgePayloadDigest(message))
}

// captureNudgeDraftSubmit observes acceptance only while the draft still belongs
// to this payload. Ownership and capture share a tmux command so a later attach
// cannot invalidate the snapshot that already established confirmation.
func (t *Tmux) captureNudgeDraftSubmit(target, message string) ([]string, error) {
	out, err := t.run("if-shell", "-F", "-t", target, nudgeDraftOwnershipCondition(message),
		shellquote.Join([]string{"capture-pane", "-p", "-t", target, "-S", fmt.Sprintf("-%d", promptObservationLines)}),
		"display-message -p draft-ownership-changed")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out) == "draft-ownership-changed" {
		return nil, fmt.Errorf("%w: draft ownership changed before observation of %q", ErrNudgeDraftDeferred, target)
	}
	return strings.Split(out, "\n"), nil
}

// nudgeComposer reads the last live input, including continuation rows inside
// its frame. Unknown layouts stay unreadable: a missing prompt is not an empty
// composer. ComposerInputText removes attributed suggestions, not typed input.
func nudgeComposer(content, prefix string) (string, bool) {
	lines := strings.Split(runtime.ComposerInputText(content), "\n")
	start := -1
	frame, promptIndent := -1, 0
	var draft string
	for i, line := range lines {
		if composerFrameBoundary(line) {
			frame = i
			continue
		}
		if remainder, ok := lastComposerRemainder([]string{line}, prefix); ok {
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
				start, draft, promptIndent = i, remainder, indent
			}
		}
	}
	if start < 0 {
		return "", false
	}
	border := "│"
	if strings.HasPrefix(lines[start], "┃") {
		border = "┃"
	}
	boxed := strings.HasPrefix(lines[start], border)
	if boxed {
		draft = strings.TrimSuffix(strings.TrimSpace(draft), border)
	}
	parts := []string{strings.TrimSpace(draft)}
	framed := false
	for _, line := range lines[start+1:] {
		if composerFrameBoundary(line) {
			framed = true
			break
		}
	}
	if boxed && !framed {
		return "", false
	}
	// Codex separates its composer from the model/effort/cwd footer with
	// a blank row instead of a box border. Only the final nonblank row can
	// be that footer: footer-shaped lines inside a draft remain input.
	footer := false
	if !framed && !boxed && strings.TrimSpace(prefix) == "›" {
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
				return "", false
			}
			parts = append(parts, strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, border), border)))
			continue
		}
		if footer && trimmed != "" && !strings.HasPrefix(line, "  ") {
			return "", false
		}
		if !framed {
			if trimmed == "" {
				if strings.TrimSpace(strings.Join(lines[start+1+i:], "\n")) == "" {
					break
				}
				return "", emptySuggestion && strings.TrimSpace(strings.Join(parts, "\n")) == ""
			}
			if !strings.HasPrefix(line, "  ") {
				return "", false
			}
		}
		parts = append(parts, strings.TrimRight(strings.TrimPrefix(line, "  "), " "))
	}
	return strings.TrimSpace(strings.Join(parts, "\n")), true
}

// nudgeDraftMatches checks rendering compatibility, not payload identity.
// Callers need full-paste provenance before relying on a normalized match.
// A positive width permits renderer-inserted row boundaries standing for a
// space or nothing. Provenance establishes identity; the renderer's word-wrap
// and cursor margins need not be reconstructed. Explicit newlines and interior
// whitespace remain significant. A nonpositive width requires an exact match.
func nudgeDraftMatches(draft, message string, wrapWidth int) bool {
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
