package tmux

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/mattn/go-runewidth"
)

// nudgeDraftPayloadOption belongs to the pane, so paste provenance survives a
// transport restart but does not follow a replacement pane. It contains only
// a payload digest, never the message text.
const nudgeDraftPayloadOption = "@gc-nudge-draft-payload"

// nudgeDraftOwnershipHook binds provenance to uninterrupted detached input.
// Appending preserves other hooks; checking the hook on retry detects another
// caller replacing or removing it, including the hidden-attach helper.
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
	hooks, err := t.run("show-hooks", "-t", session, "client-attached")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(hooks, "\n") {
		if strings.HasSuffix(line, " "+hook) {
			return pane, nil
		}
	}
	if !install {
		return "", fmt.Errorf("draft ownership hook is absent")
	}
	_, err = t.run("set-hook", "-a", "-t", session, "client-attached", hook)
	return pane, err
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
// It preserves payload whitespace while allowing the rendering
// at the measured composer width. Only padding at generated wrap boundaries
// is ignored; spaces before explicit newlines remain significant even when
// the composer reader drops them. A nonpositive width requires an exact match.
func nudgeDraftMatches(draft, message string, wrapWidth int) bool {
	message = strings.TrimSpace(message)
	if draft == message {
		return true
	}
	if wrapWidth <= 0 {
		return false
	}
	var rows []string
	for _, line := range strings.Split(message, "\n") {
		wrapped := strings.Split(runewidth.Wrap(line, wrapWidth), "\n")
		for i := 0; i < len(wrapped)-1; i++ {
			wrapped[i] = strings.TrimRight(wrapped[i], " ")
		}
		rows = append(rows, wrapped...)
	}
	return draft == strings.Join(rows, "\n")
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
