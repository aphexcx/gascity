package runtime

import (
	"regexp"
	"strings"
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
	var faint []bool
	dim, pos := false, 0
	appendText := func(text string) {
		plain.WriteString(text)
		for range len(text) {
			faint = append(faint, dim)
		}
	}
	for _, match := range terminalSGR.FindAllStringIndex(capture, -1) {
		appendText(capture[pos:match[0]])
		dim = sgrFaint(capture[match[0]+2:match[1]-1], dim)
		pos = match[1]
	}
	appendText(capture[pos:])
	return plain.String(), faint
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
// suggestion: the prompt marker and status text also use grey.
func RenderPeek(capture string) PeekView {
	text, faint := terminalTextAndFaint(capture)
	view := PeekView{Text: text, Output: text}
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
		return view
	}
	input := strings.TrimLeft(lines[top+1], " \t")
	rest, ok := strings.CutPrefix(input, "❯")
	if !ok || (!strings.HasPrefix(rest, " ") && !strings.HasPrefix(rest, "\u00a0")) {
		return view
	}
	start := starts[top+1] + len(lines[top+1]) - len(rest)
	var output, tagged, suggestion strings.Builder
	output.WriteString(text[:start])
	tagged.WriteString(text[:start])
	lastSuggestionLine := -1
	line := top + 1
	for i := start; i < starts[bottom]; {
		if text[i] == '\n' {
			output.WriteByte('\n')
			tagged.WriteByte('\n')
			line++
			i++
			continue
		}
		end := i + 1
		for end < starts[bottom] && text[end] != '\n' && faint[end] == faint[i] {
			end++
		}
		run := text[i:end]
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
		i = end
	}
	output.WriteString(text[starts[bottom]:])
	tagged.WriteString(text[starts[bottom]:])
	view.Output = output.String()
	view.Text = tagged.String()
	view.Suggestion = strings.TrimSpace(suggestion.String())
	return view
}

func sgrFaint(parameters string, faint bool) bool {
	parts := strings.Split(parameters, ";")
	for i := 0; i < len(parts); i++ {
		switch parts[i] {
		case "", "0":
			faint = false
		case "2":
			faint = true
		case "22":
			faint = false
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
	return faint
}
