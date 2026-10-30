package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

func renderSessionPeek(output, sessionID, target string, lines int, jsonOutput bool, stdout, stderr io.Writer) int {
	view := runtime.RenderPeek(output)
	if jsonOutput {
		if err := writeCLIJSONLine(stdout, sessionPeekJSONResult{
			SchemaVersion: "1",
			SessionID:     sessionID,
			Target:        target,
			Lines:         lines,
			LineCount:     outputLineCount(output),
			Output:        view.Output,
			Suggestion:    view.Suggestion,
		}); err != nil {
			fmt.Fprintf(stderr, "gc session peek: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}
	fmt.Fprint(stdout, view.Text) //nolint:errcheck // best-effort stdout
	if !strings.HasSuffix(view.Text, "\n") {
		fmt.Fprintln(stdout) //nolint:errcheck // best-effort stdout
	}
	return 0
}
