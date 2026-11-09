package tmux

import (
	"fmt"
	"strings"
)

const nudgeDraftIdentityOption = "@gc-nudge-draft-identity"

// A nonempty identity reserves submission for the caller that owns retries.
// A known empty identity permits recovery of a one-shot delivery. Missing
// provenance is unknown, so a different payload cannot recover it.
func (t *Tmux) nudgeDraftIdentity(target, digest string) (string, bool, error) {
	value, err := t.run("show-options", "-p", "-qv", "-t", exactPaneTarget(target), nudgeDraftIdentityOption)
	if err != nil {
		return "", false, fmt.Errorf("reading draft retry identity: %w", err)
	}
	parts := strings.SplitN(strings.TrimSpace(value), ":", 2)
	if len(parts) != 2 || parts[0] != digest {
		return "", false, nil
	}
	return parts[1], true, nil
}
