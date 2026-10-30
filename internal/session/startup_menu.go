package session

// DisplayWithStartupMenu overlays a live startup-menu observation on the
// projected session display. The bead's lifecycle and resume identity remain
// unchanged; a stale pane cannot make an inactive session look parked.
func DisplayWithStartupMenu(info Info, reason, menu string) (string, string) {
	if info.State == StateActive && !info.Closed && menu != "" {
		return "parked", menu
	}
	return string(info.State), reason
}
