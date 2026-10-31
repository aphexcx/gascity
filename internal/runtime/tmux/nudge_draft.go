package tmux

import (
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

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
			if trimmed := strings.TrimLeft(line, " \t"); strings.HasPrefix(trimmed, "│") {
				candidate = strings.TrimPrefix(trimmed, "│")
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
	boxed := strings.HasPrefix(strings.TrimSpace(lines[start]), "│")
	if boxed {
		draft = strings.TrimSuffix(strings.TrimSpace(draft), "│")
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
	for _, line := range lines[start+1:] {
		trimmed := strings.TrimSpace(line)
		if composerFrameBoundary(line) {
			break
		}
		if boxed {
			if !strings.HasPrefix(trimmed, "│") {
				return "", false
			}
			parts = append(parts, strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "│"), "│")))
			continue
		}
		if !framed && (trimmed == "" || !strings.HasPrefix(line, "  ")) {
			break
		}
		parts = append(parts, strings.TrimRight(strings.TrimPrefix(line, "  "), " "))
	}
	return strings.TrimSpace(strings.Join(parts, "\n")), true
}

func composerFrameBoundary(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "╭") || strings.HasPrefix(trimmed, "╰") ||
		(trimmed != "" && !strings.HasPrefix(line, " ") && strings.Trim(trimmed, "─") == "")
}
