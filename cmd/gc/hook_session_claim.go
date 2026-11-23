package main

import (
	"io"
	"log"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/session"
)

// sessionCurrentClaimFrontDoor opens the routed session-class write front door
// for the city this process is running in. It is the shared root for the two
// halves of the claim back-channel: `gc hook --claim` stamps the claimed bead id
// onto the calling session's bead through it, and `gc hook current` reads that
// stamp back through it.
//
// It routes through cliSessionFrontDoor so a [beads.classes.sessions] relocation
// reaches both halves — a raw work-store front door would stamp the claim onto
// the work store while the real session bead lives in the relocated store, and
// `gc hook current` would then read back nothing forever. The no-refresh config
// loader matches the other hook-path roots (cmd_prime.go's
// persistPrimeHookProviderSessionKey): this runs on every claim, and a nil cfg
// leaves cliSessionStore identity to the input store.
func sessionCurrentClaimFrontDoor() (*session.Store, error) {
	cityPath, err := resolveCity()
	if err != nil {
		return nil, err
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		return nil, err
	}
	cfg, _ := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	return cliSessionFrontDoor(store, cfg, cityPath), nil
}

// hookStampSessionCurrentClaim records beadID as the work bead the session
// identified by sessionID is currently running. It is the production
// implementation of the hookClaimOps.StampSessionClaim seam.
//
// The write goes through session.Store.SetCurrentClaim, which resolves the id
// EXACTLY and refuses a non-session bead before writing anything: bd's fuzzy id
// resolver would otherwise let a post-claim update land on a prefix-colliding
// session if the intended one disappeared concurrently, which is why the claim
// path decorates the session bead only through this guarded seam (see
// publishHookClaimRunMap, which stays a file-based sidecar for exactly that
// reason). SetCurrentClaim also compare-and-skips, so the per-tick adoption
// re-run issues no write once the value is current.
func hookStampSessionCurrentClaim(sessionID, beadID string) error {
	sessFront, err := sessionCurrentClaimFrontDoor()
	if err != nil {
		return err
	}
	_, err = sessFront.SetCurrentClaim(sessionID, beadID)
	return err
}

// hookSessionDrainPending reports whether the session identified by sessionID is
// already draining. It is the production implementation of the
// hookClaimOps.DrainPending seam — the F-D claim fence's only input.
//
// The SESSION ROW is the source, not provider meta. `gc runtime drain-check`
// reads GC_DRAIN, which reconciler-tracked drains never set, so a provider-meta
// probe would miss the whole keyed population this fence exists for. It reads
// through the same routed front door the claim back-channel writes through, so a
// [beads.classes.sessions] relocation reaches the fence too.
//
// Any state OTHER than draining — including a closed row, whose runtime state
// GetState reports as empty — is not this fence's business. A closed or
// superseded incarnation is the runtime-identity fence's stale-session lane, and
// answering false here leaves that lane's verdict intact rather than relabelling
// it. Errors are returned rather than swallowed: the caller fails OPEN on them,
// and it can only make that choice if it can see them.
func hookSessionDrainPending(sessionID string) (bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false, nil
	}
	sessFront, err := sessionCurrentClaimFrontDoor()
	if err != nil {
		return false, err
	}
	state, _, err := sessFront.GetState(sessionID)
	if err != nil {
		return false, err
	}
	return state == session.StateDraining, nil
}

// hookResolveClaimSession reads the claiming session by exact ID. A prefix
// match could stamp another session's slot or checkout onto the work bead.
func hookResolveClaimSession(sessionID string) session.Info {
	if strings.TrimSpace(sessionID) == "" {
		return session.Info{}
	}
	sessFront, err := sessionCurrentClaimFrontDoor()
	if err != nil {
		log.Printf("hookResolveClaimSession: opening the session front door for %q: %v", sessionID, err)
		return session.Info{}
	}
	resolved, err := sessFront.ResolveIDByExactID(strings.TrimSpace(sessionID))
	if err != nil {
		log.Printf("hookResolveClaimSession: resolving session id %q exactly: %v", sessionID, err)
		return session.Info{}
	}
	info, err := sessFront.Get(resolved)
	if err != nil {
		log.Printf("hookResolveClaimSession: reading session bead %q: %v", resolved, err)
		return session.Info{}
	}
	return info
}

func claimedPoolSlotRecord(info session.Info) string {
	template := strings.TrimSpace(info.Template)
	slot, err := strconv.Atoi(strings.TrimSpace(info.PoolSlot))
	if identity := info.CanonicalIdentity(); identity.Present {
		slot = identity.PoolSlot
	} else if err != nil {
		return ""
	}
	if template == "" || slot <= 0 {
		return ""
	}
	return template + "#" + strconv.Itoa(slot)
}

// sessionStampableWorkDir returns the checkout of info that may be stamped onto a
// work bead as gc.work_dir, or "" when info has none that qualifies. It is the
// work directory fallback for a claim with no recorded checkout.
//
// A POOL-MANAGED session is refused outright: its WorkDir is the slot label
// (worker-slots/worker-N), a SHARED directory rather than an isolated worktree,
// so stamping it would turn a slot label into worktree-ownership evidence on the
// bead (gc-j0cfh). The reconciler side refuses the same value for the same reason
// (workDirStampHasOwnershipEvidence), and this keeps the claim path from becoming
// a second way to mint it.
//
// The classifier is isPoolManagedSessionInfo, this package's canonical one, not
// the raw PoolManaged flag: a session carrying a pool_slot, or one whose origin is
// ephemeral, is running in a shared slot directory for exactly the same gc-j0cfh
// reason even when pool_managed was never stamped on its bead.
func sessionStampableWorkDir(info session.Info) string {
	if isPoolManagedSessionInfo(info) {
		return ""
	}
	return strings.TrimSpace(info.WorkDir)
}
