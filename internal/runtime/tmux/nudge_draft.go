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
	var draft string
	for i, line := range lines {
		if remainder, ok := lastComposerRemainder([]string{line}, prefix); ok {
			start, draft = i, remainder
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
	framed := boxed
	for _, line := range lines[start+1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && strings.Trim(trimmed, "─") == "" {
			framed = true
			break
		}
	}
	for _, line := range lines[start+1:] {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "╰") || (trimmed != "" && strings.Trim(trimmed, "─") == "") {
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
