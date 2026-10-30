package runtime

import "strings"

// StartupMenuReason returns the heading of a recognized interactive startup
// menu. It describes an observed pane, not a durable session lifecycle state.
func StartupMenuReason(content string) string {
	// Peek can include scrollback. A later ready prompt means an earlier menu
	// has cleared; its retained text must not keep the session parked forever.
	lines := strings.Split(content, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "›") || strings.HasPrefix(line, "❯") {
			choice := strings.TrimSpace(strings.TrimLeft(line, "›❯"))
			if len(choice) < 2 || choice[0] < '1' || choice[0] > '9' || choice[1] != '.' {
				return ""
			}
			break
		}
	}
	if containsCodexHookReviewDialog(content) {
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			if line == "Hooks need review" || strings.HasPrefix(line, "Hooks need review:") {
				return line
			}
		}
	}
	if strings.Contains(content, "1. Yes, continue") && strings.Contains(content, "2. No, exit") {
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			if line == "Do you trust the contents of this directory?" || line == "Do you trust the files in this folder?" {
				return line
			}
		}
	}
	return ""
}

// ObserveStartupMenu reads the current pane for a startup menu. A read error
// remains an error rather than being reported as an active or parked session.
func ObserveStartupMenu(sp Provider, name string) (string, error) {
	if sp == nil || name == "" {
		return "", nil
	}
	content, err := sp.Peek(name, 120)
	if err != nil {
		return "", err
	}
	return StartupMenuReason(content), nil
}
