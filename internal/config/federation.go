package config

import "github.com/gastownhall/gascity/internal/federation"

// FederationConfig is the [federation] table: how this city names itself in a
// bead store it shares with other cities.
type FederationConfig struct {
	// Identity is this city's name in the federated store — "citadel",
	// "jadegate", "boomtown". When set, every bead gc creates in any store of
	// this city (city scope and every rig scope) carries the label
	// owner:<identity>, and the claim path reads the same key to tell which
	// owner labels are foreign. It is an explicit key rather than the
	// workspace name because the workspace name is a machine-local label that
	// need not be distinct across the federation. Must match
	// ^[a-z0-9][a-z0-9-]*$. Unset means not federated: nothing is stamped and
	// nothing is refused.
	Identity string `toml:"identity,omitempty"`
}

// OwnerLabel returns the owner:<identity> label this city stamps on the beads
// it creates, or ("", false) when the city is not federated.
func (f FederationConfig) OwnerLabel() (string, bool) {
	return federation.OwnerLabel(f.Identity)
}

// validateFederation rejects a malformed [federation] identity at load time.
// The identity is spliced into a label that other cities compare
// byte-for-byte, so a value with the wrong shape would label every bead this
// city creates with an owner no claim filter can match — silently, on every
// create — and the config fails to load instead.
func validateFederation(f FederationConfig) error {
	return federation.ValidateIdentity(f.Identity)
}
