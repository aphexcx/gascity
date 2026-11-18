package runtime

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// PeekView separates an input suggestion from a terminal capture. Text is the
// human-readable, tagged view; Output omits suggestions and preserves newlines.
type PeekView struct {
	Text       string
	Output     string
	Suggestion string
}

var terminalSGR = regexp.MustCompile(`\x1b\[[0-9;:]*m`)

func stripTerminalSGR(text string) string {
	return terminalSGR.ReplaceAllString(text, "")
}

func terminalTextAndFaint(capture string) (string, []bool) {
	var plain strings.Builder
	var faint, inverse []bool
	dim, reversed, pos := false, false, 0
	appendText := func(text string) {
		plain.WriteString(text)
		for range len(text) {
			faint = append(faint, dim)
			inverse = append(inverse, reversed)
		}
	}
	for _, match := range terminalSGR.FindAllStringIndex(capture, -1) {
		appendText(capture[pos:match[0]])
		dim, reversed = sgrAttributes(capture[match[0]+2:match[1]-1], dim, reversed)
		pos = match[1]
	}
	appendText(capture[pos:])
	text := plain.String()
	markPlaceholderCursor(text, faint, inverse)
	return text, faint
}

// markPlaceholderCursor restores the faint attribute obscured by Claude's
// inverse cursor on an empty placeholder. Its renderer hides placeholders when
// input is nonempty and renders inline completions only at the end of input.
// Require one inverse character followed entirely by faint text in the final
// framed composer; inverse output or a cursor within a typed draft stays input.
func markPlaceholderCursor(text string, faint, inverse []bool) {
	start, end, ok := framedComposerBounds(text)
	if !ok {
		return
	}
	start += len(text[start:end]) - len(strings.TrimLeft(text[start:end], " \t\u00a0"))
	if start == end || !inverse[start] || faint[start] {
		return
	}
	first, size := utf8.DecodeRuneInString(text[start:end])
	if unicode.IsSpace(first) {
		return
	}
	hasSuggestion := false
	for offset, char := range text[start+size : end] {
		if unicode.IsSpace(char) {
			continue
		}
		i := start + size + offset
		if !faint[i] || inverse[i] {
			return
		}
		hasSuggestion = true
	}
	if hasSuggestion {
		for i := start; i < start+size; i++ {
			faint[i] = true
		}
	}
}

// ComposerInputText removes SGR styling and faint suggestion text for input
// inspection. Callers must locate the live composer in the result; this is not
// a transcript renderer, since faint output outside the composer is omitted too.
func ComposerInputText(capture string) string {
	text, faint := terminalTextAndFaint(capture)
	var input strings.Builder
	for i := range len(text) {
		if !faint[i] || text[i] == '\n' {
			input.WriteByte(text[i])
		}
	}
	return input.String()
}

// RenderPeek labels faint text in the final framed Claude input box. Faint
// output elsewhere remains ordinary text. Grey foreground alone is not a
// suggestion: the prompt marker and status text also use grey. An inverse
// cursor over the first character of an otherwise faint placeholder belongs
// to that suggestion too.
func RenderPeek(capture string) PeekView {
	text, faint := terminalTextAndFaint(capture)
	view := PeekView{Text: text, Output: text}
	start, end, ok := framedComposerBounds(text)
	if !ok {
		return view
	}
	var output, tagged, suggestion strings.Builder
	output.WriteString(text[:start])
	tagged.WriteString(text[:start])
	lastSuggestionLine := -1
	line := 0
	for i := start; i < end; {
		if text[i] == '\n' {
			output.WriteByte('\n')
			tagged.WriteByte('\n')
			line++
			i++
			continue
		}
		runEnd := i + 1
		for runEnd < end && text[runEnd] != '\n' && faint[runEnd] == faint[i] {
			runEnd++
		}
		run := text[i:runEnd]
		if faint[i] && strings.TrimSpace(run) != "" {
			if lastSuggestionLine >= 0 && lastSuggestionLine != line {
				suggestion.WriteByte('\n')
			}
			suggestion.WriteString(run)
			lastSuggestionLine = line
			tagged.WriteString("[suggestion] ")
		} else {
			output.WriteString(run)
		}
		tagged.WriteString(run)
		i = runEnd
	}
	output.WriteString(text[end:])
	tagged.WriteString(text[end:])
	view.Output = output.String()
	view.Text = tagged.String()
	view.Suggestion = strings.TrimSpace(suggestion.String())
	return view
}

func framedComposerBounds(text string) (start, end int, ok bool) {
	lines := strings.Split(text, "\n")
	starts := make([]int, len(lines))
	top, bottom := -1, -1
	for i, line := range lines {
		if i > 0 {
			starts[i] = starts[i-1] + len(lines[i-1]) + 1
		}
		border := strings.TrimSpace(line)
		if len([]rune(border)) >= 4 && strings.Trim(border, "─") == "" {
			top, bottom = bottom, i
		}
	}
	if top < 0 || bottom <= top+1 {
		return 0, 0, false
	}
	input := strings.TrimLeft(lines[top+1], " \t")
	rest, ok := strings.CutPrefix(input, "❯")
	if !ok || (!strings.HasPrefix(rest, " ") && !strings.HasPrefix(rest, "\u00a0")) {
		return 0, 0, false
	}
	return starts[top+1] + len(lines[top+1]) - len(rest), starts[bottom], true
}

func sgrAttributes(parameters string, faint, inverse bool) (bool, bool) {
	parts := strings.Split(parameters, ";")
	for i := 0; i < len(parts); i++ {
		switch parts[i] {
		case "", "0":
			faint, inverse = false, false
		case "2":
			faint = true
		case "22":
			faint = false
		case "7":
			inverse = true
		case "27":
			inverse = false
		case "38", "48", "58":
			// Extended-color payloads are not independent SGR attributes.
			if i+1 < len(parts) {
				switch parts[i+1] {
				case "5":
					i += 2
				case "2":
					i += 4
				}
			}
		}
	}
	return faint, inverse
}
