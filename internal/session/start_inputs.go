package session

import "strings"

// FirstProviderSessionStart identifies a pending create whose provider session
// has not yet been started. Entry-time command and prompt preparation share this
// rule; runtime-only reconciler starts retain their own transcript-aware policy.
func FirstProviderSessionStart(info Info) bool {
	if info.State != StateStartPending && info.State != StateCreating {
		return false
	}
	return strings.TrimSpace(info.CreationCompleteAt) == "" && strings.TrimSpace(info.StartedConfigHash) == ""
}
