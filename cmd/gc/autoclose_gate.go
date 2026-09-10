package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/federation"
)

// autocloseGate is the cross-city fence for gc's AUTOMATIC bead writers: the
// convoy autoclose (the bead-close event path, the bd on_close hook path and
// the `gc convoy check` sweep) and the molecule autoclose (the step-terminal
// and source-bead triggers). Every one of them runs in EVERY city that holds
// a copy of a federated store, on the same rows, with nothing but wall-clock
// ordering between them: after a `gc dolt pull` brings one city's closes into
// another city's copy, the second city's event path sees "all children
// closed" and closes the convoy again — a second close of the same row with
// its own closed_at, updated_at and row_lock, which the next pull turns into
// a dolt conflict (hw-m0t6n, 2026-09-10; gp-c04p).
//
// The rule is the one every refusing call site already applies to a claim,
// federation.MayClaim, so no automatic writer can disagree with the claim
// fence about whose bead it is: this city's [federation] identity may write
// a bead that carries owner:<identity> for it, a bead with no owner label
// (legacy work), or a bead handed off to it; a bead another city owns is left
// exactly as the pull delivered it, and that city's own sweep closes it. An
// unset identity means the city is not federated and the gate is off.
//
// The gate is evaluated at the write — after "all children are terminal" is
// established, right before the close — so a refusal is logged only for a
// row that would otherwise have been written, once per trigger, never for
// every foreign convoy on every sweep. The zero value is the non-federated
// gate; the production entry points build theirs from the loaded city config
// (autocloseGateFor), and a city whose config cannot be loaded at all is by
// definition not federated (the identity lives only in city.toml), so its
// fallback path keeps the zero gate.
type autocloseGate struct {
	// identity is this city's [federation] identity, trimmed; "" = off.
	identity string
}

// autocloseGateFor builds the gate for the automatic writers of the city cfg
// describes. A nil cfg yields the non-federated gate.
func autocloseGateFor(cfg *config.City) autocloseGate {
	if cfg == nil {
		return autocloseGate{}
	}
	return autocloseGate{identity: strings.TrimSpace(cfg.Federation.Identity)}
}

// allows reports whether this city's automatic writer may write bead. When it
// may not, the one greppable refusal line every fence site logs
// ("cross-city-fence refused bead=… owner=… this_identity=… missing=…") is
// written to w, prefixed with site, and false is returned; nothing is ever
// written to a refused bead.
func (g autocloseGate) allows(bead beads.Bead, w io.Writer, site string) bool {
	ok, reason := federation.MayClaim(bead.Labels, g.identity)
	if ok {
		return true
	}
	fmt.Fprintf(w, "%s: %s\n", site, federation.ClaimRefusalLine(bead.ID, reason)) //nolint:errcheck // best-effort diagnostics
	return false
}
