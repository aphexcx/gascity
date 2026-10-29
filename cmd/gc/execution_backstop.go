package main

// The execution backstop observes quiet pool sessions holding a single claim,
// retries their configured nudge within a bounded budget, and reports exhaustion
// once per claim. Draining requires positive evidence that a nudge reached the
// session. Unknown activity and undeliverable nudges hold the claim.

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/federation"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Session-bead metadata keys for the execution backstop. Persisted for the same
// reason the sibling backstops persist theirs: a controller restart must resume
// the state machine, never replay it (test-5il).
const (
	executionClaimNudgeWorkKey      = "execution_claim_nudge_work"
	executionClaimNudgeRootKey      = "execution_claim_nudge_root"
	executionClaimNudgeStoreRefKey  = "execution_claim_nudge_store_ref"
	executionClaimNudgeCountKey     = "execution_claim_nudge_count"
	executionClaimNudgeDeliveredKey = "execution_claim_nudge_delivered"
	executionClaimNudgeAtKey        = "execution_claim_nudge_at"
	// executionClaimNudgeStalledKey latches the one-shot escalation so the
	// typed event and the drain request fire once per stalled claim rather than
	// once per tick for as long as the claim is held.
	executionClaimNudgeStalledKey = "execution_claim_nudge_stalled"
)

// nudgeStalledPoolExecution re-delivers the configured claim nudge to a pool slot
// that holds an in-progress claim while quiet, and reports once
// the bounded attempts are spent.
//
// work/workStores/workStoreRefs are the reconciler's index-aligned assigned-work
// snapshot; requestDrain is the existing drain request (drainOps.setDrain), taken
// as a function so this file needs no reconciler wiring of its own. A partial
// snapshot is not evidence of a stall, so it disables the predicate for that tick.
func nudgeStalledPoolExecution(
	sp runtime.Provider,
	cfg *config.City,
	store beads.Store,
	sessionBeads []beads.Bead,
	work []beads.Bead,
	workStores []beads.Store,
	workStoreRefs []string,
	snapshotPartial bool,
	now time.Time,
	rec events.Recorder,
	requestDrain func(sessionBead beads.Bead) error,
	stdout io.Writer,
	refusals *claimRefusalLog,
) {
	if sp == nil || cfg == nil || store == nil || snapshotPartial {
		return // hot reconcile path: never panic on a half-built dependency
	}
	if sess, ok := store.(beads.SessionStore); ok && sess.Store == nil {
		return
	}
	runNudgeBackstop(sp, store, sessionBeads, nil, now, stdout, "execution-claim-nudge", poolExecutionBackstop{
		refusals:     refusals,
		cfg:          cfg,
		sp:           sp,
		now:          now,
		rec:          rec,
		requestDrain: requestDrain,
		claims:       newExecutionClaimSnapshot(work, workStores, workStoreRefs, federationIdentity(cfg), now, refusals),
	})
}

// executionClaim is one in-progress claim from the assigned-work snapshot, kept
// with the store handle its live re-read must use.
type executionClaim struct {
	BeadID          string
	RootID          string
	StoreRef        string
	Assignee        string
	Store           beads.Store
	HumanCheckpoint bool
}

// executionClaimSnapshot indexes in-progress claims by their exact assignee
// string. Resolution is by identity rather than by bead id because the question
// this predicate asks is "what does THIS session hold", and the same bead id can
// exist in independent stores.
type executionClaimSnapshot struct {
	byAssignee map[string][]executionClaim
}

// storeRefAt reads the index-aligned store ref for a work row, normalized the
// way the idle-claim snapshot keys it; a short slice reads as the city scope.
func storeRefAt(storeRefs []string, i int) string {
	if i < len(storeRefs) {
		return normalizeIdleClaimStoreRef(storeRefs[i])
	}
	return normalizeIdleClaimStoreRef("")
}

func newExecutionClaimSnapshot(work []beads.Bead, stores []beads.Store, storeRefs []string, identity string, now time.Time, refusals *claimRefusalLog) executionClaimSnapshot {
	snapshot := executionClaimSnapshot{byAssignee: make(map[string][]executionClaim)}
	refused := make(map[storeScopedBeadKey]bool)
	for i, wb := range work {
		assignee := strings.TrimSpace(wb.Assignee)
		if assignee == "" || !strings.EqualFold(strings.TrimSpace(wb.Status), "in_progress") {
			continue
		}
		if strings.TrimSpace(wb.ID) == "" {
			continue
		}
		if ok, reason := federation.MayClaim(wb.Labels, identity); !ok {
			key := storeScopedBeadKey{StoreRef: storeRefAt(storeRefs, i), ID: strings.TrimSpace(wb.ID)}
			if !refused[key] {
				refused[key] = true
				if refusals.shouldLog(now, key, reason) {
					log.Printf("execution-claim-nudge: %s", federation.ClaimRefusalLine(wb.ID, reason))
				}
			}
			continue
		}

		claim := executionClaim{
			BeadID:          wb.ID,
			RootID:          strings.TrimSpace(wb.Metadata[beadmeta.RootBeadIDMetadataKey]),
			Assignee:        assignee,
			HumanCheckpoint: executionClaimHasHumanCheckpoint(wb),
		}
		if i < len(storeRefs) {
			claim.StoreRef = normalizeIdleClaimStoreRef(storeRefs[i])
		}
		if i < len(stores) {
			claim.Store = stores[i]
		}
		snapshot.byAssignee[assignee] = append(snapshot.byAssignee[assignee], claim)
	}
	return snapshot
}

// forIdentities returns the deduped claims held by any of the session's current
// identities, in a stable order so a session with several claims resolves the
// same way on every tick.
func (s executionClaimSnapshot) forIdentities(identities []string) []executionClaim {
	seen := make(map[string]struct{})
	var out []executionClaim
	for _, identity := range identities {
		for _, claim := range s.byAssignee[identity] {
			key := claim.StoreRef + "\x00" + claim.BeadID
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, claim)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StoreRef != out[j].StoreRef {
			return out[i].StoreRef < out[j].StoreRef
		}
		return out[i].BeadID < out[j].BeadID
	})
	return out
}

// poolExecutionBackstop observes a quiet pool slot holding a claim.
type poolExecutionBackstop struct {
	refusals     *claimRefusalLog
	cfg          *config.City
	sp           runtime.Provider
	now          time.Time
	rec          events.Recorder
	requestDrain func(sessionBead beads.Bead) error
	claims       executionClaimSnapshot
}

func (p poolExecutionBackstop) governs(s beads.Bead) bool {
	return strings.TrimSpace(s.Metadata["pool_managed"]) == "true"
}

// resolve reports an outstanding stall only when all of it holds: the session
// holds exactly ONE in-progress claim under a current identity, and the runtime
// says the session has been quiet for at least the grace window.
//
// Exactly one, because the pacing state names a single bead: a session juggling
// several claims is demonstrably doing something, and picking one of them to
// nudge about would make the persisted marker a lie. Ambiguity holds rather than
// clears, so a transient multi-claim tick cannot reset a window already running.
func (p poolExecutionBackstop) resolve(s beads.Bead, _ map[string]beads.Bead, sessName string) (backstopTarget, backstopResolution) {
	claims := p.claims.forIdentities(currentSessionAssigneeIdentities(s))
	switch len(claims) {
	case 0:
		return backstopTarget{}, backstopResolutionClear
	case 1:
		// Continue below.
	default:
		return backstopTarget{}, backstopResolutionHold
	}
	claim := claims[0]
	target := executionClaimTarget(s, claim)
	if target.SessionPinned {
		if claim.HumanCheckpoint && !executionClaimMarkerMatches(s, target) {
			return backstopTarget{}, backstopResolutionHold
		}
		switch p.revalidate(target) {
		case backstopResolutionHold:
			return backstopTarget{}, backstopResolutionHold
		case backstopResolutionClear:
			return backstopTarget{}, backstopResolutionClear
		case backstopResolutionOutstanding:
			// Continue below.
		default:
			return backstopTarget{}, backstopResolutionHold
		}
	}
	if !p.sessionIsQuiet(sessName) {
		return backstopTarget{}, backstopResolutionHold
	}
	return target, backstopResolutionOutstanding
}

func executionClaimHasHumanCheckpoint(claim beads.Bead) bool {
	return strings.TrimSpace(claim.Metadata[beadmeta.CheckpointMetadataKey]) != ""
}

func executionClaimTarget(session beads.Bead, claim executionClaim) backstopTarget {
	return backstopTarget{
		ID:            claim.BeadID,
		RootID:        claim.RootID,
		StoreRef:      claim.StoreRef,
		Assignee:      claim.Assignee,
		Store:         claim.Store,
		SessionPinned: strings.TrimSpace(session.Metadata["pin_awake"]) == "true",
	}
}

func executionClaimMarkerMatches(session beads.Bead, target backstopTarget) bool {
	return strings.TrimSpace(session.Metadata[executionClaimNudgeWorkKey]) == target.ID &&
		strings.TrimSpace(session.Metadata[executionClaimNudgeStoreRefKey]) == target.StoreRef
}

// sessionIsQuiet reports whether the runtime has observed no activity for at
// least the grace window. An unreadable or unset activity signal is NOT quiet: a
// backstop that treats "unknown" as "idle" nudges working agents, which is
// exactly how the reverted idle-session nudger produced restart storms.
func (p poolExecutionBackstop) sessionIsQuiet(sessName string) bool {
	last, err := p.sp.GetLastActivity(sessName)
	if err != nil || last.IsZero() {
		return false
	}
	return p.now.Sub(last) >= idleClaimNudgeGrace
}

func (p poolExecutionBackstop) state(s beads.Bead, target backstopTarget) (same bool, attempts int, last time.Time) {
	same = strings.TrimSpace(s.Metadata[executionClaimNudgeWorkKey]) == target.ID &&
		strings.TrimSpace(s.Metadata[executionClaimNudgeStoreRefKey]) == target.StoreRef
	return same, atoiOr0(s.Metadata[executionClaimNudgeCountKey]), parseRFC3339OrZero(s.Metadata[executionClaimNudgeAtKey])
}

func (p poolExecutionBackstop) content(s beads.Bead) string {
	return claimNudgeFor(p.cfg, s)
}

// revalidate re-reads the claim through the owning store's authoritative live
// handle immediately before delivery. Assigned-work snapshots are normally
// CachingStore-backed, so a plain read can still show a claim the agent finished
// seconds ago. A failed read HOLDS: it is not proof the claim went away.
func (p poolExecutionBackstop) revalidate(target backstopTarget) backstopResolution {
	current, resolution := p.revalidateLiveTarget(target)
	if resolution != backstopResolutionOutstanding {
		return resolution
	}
	if target.SessionPinned && executionClaimHasHumanCheckpoint(current) {
		return backstopResolutionHold
	}
	return backstopResolutionOutstanding
}

func (p poolExecutionBackstop) revalidateLiveTarget(target backstopTarget) (beads.Bead, backstopResolution) {
	if target.Store == nil {
		return beads.Bead{}, backstopResolutionHold
	}
	live := beads.HandlesFor(target.Store).Live
	if live == nil {
		return beads.Bead{}, backstopResolutionHold
	}
	current, err := live.Get(target.ID)
	if err != nil || current.ID != target.ID {
		return beads.Bead{}, backstopResolutionHold
	}
	if ok, reason := federation.MayClaim(current.Labels, federationIdentity(p.cfg)); !ok {
		if p.refusals.shouldLog(p.now, storeScopedBeadKey{StoreRef: target.StoreRef, ID: current.ID}, reason) {
			log.Printf("execution-claim-nudge: %s", federation.ClaimRefusalLine(current.ID, reason))
		}
		return current, backstopResolutionClear
	}

	if !strings.EqualFold(strings.TrimSpace(current.Status), "in_progress") ||
		strings.TrimSpace(current.Assignee) != target.Assignee {
		return current, backstopResolutionClear
	}
	return current, backstopResolutionOutstanding
}

func (p poolExecutionBackstop) observe(store beads.Store, s *beads.Bead, target backstopTarget, now time.Time, stdout io.Writer) {
	writeExecutionClaimMarker(store, s, target, 0, now, stdout)
}

func (p poolExecutionBackstop) reserve(store beads.Store, s *beads.Bead, target backstopTarget, attempts int, now time.Time, stdout io.Writer) bool {
	return writeExecutionClaimMarker(store, s, target, attempts, now, stdout)
}

func (p poolExecutionBackstop) delivered(store beads.Store, s *beads.Bead, stdout io.Writer) {
	writeSessionMetadata(store, s, map[string]string{
		executionClaimNudgeDeliveredKey: strconv.Itoa(atoiOr0(s.Metadata[executionClaimNudgeDeliveredKey]) + 1),
	}, stdout)
}

// exhausted reports once and drains only when runtime evidence proves contact.
func (p poolExecutionBackstop) exhausted(store beads.Store, s *beads.Bead, target backstopTarget, stdout io.Writer) {
	if strings.TrimSpace(s.Metadata[executionClaimNudgeStalledKey]) != "" {
		return
	}
	beadID := strings.TrimSpace(s.Metadata[executionClaimNudgeWorkKey])
	if beadID == "" {
		return
	}
	switch p.revalidate(target) {
	case backstopResolutionHold:
		return
	case backstopResolutionClear:
		p.clear(store, s, stdout)
		return
	case backstopResolutionOutstanding:
		// Escalate below.
	default:
		return
	}
	sessName := strings.TrimSpace(s.Metadata["session_name"])
	last, err := p.sp.GetLastActivity(sessName)
	if err != nil || last.IsZero() || p.now.Sub(last) < idleClaimNudgeGrace {
		return
	}
	// Latch FIRST. A failed event write or a failed drain must not leave the
	// escalation armed to repeat on every subsequent tick; the marker itself is
	// the durable record that this claim was escalated, and the operator sees the
	// failure on stdout.
	if !writeSessionMetadata(store, s, map[string]string{
		executionClaimNudgeStalledKey: p.now.UTC().Format(time.RFC3339),
	}, stdout) {
		return
	}
	delivered := max(0, atoiOr0(s.Metadata[executionClaimNudgeDeliveredKey]))
	p.emitStepStalled(s, beadID, delivered)
	provider := strings.TrimSpace(s.Metadata["provider"])
	if provider == "" {
		if agent := findAgentByTemplate(p.cfg, normalizedSessionTemplate(*s, p.cfg)); agent != nil {
			provider = strings.TrimSpace(agent.Provider)
		}
	}
	if provider == "" {
		provider = strings.TrimSpace(p.cfg.Workspace.Provider)
	}
	if provider == "" {
		provider = "unknown"
	}
	if delivered == 0 {
		condition := fmt.Sprintf("0 of %d nudges deliverable", idleClaimNudgeMaxAttempts)
		if p.content(*s) == "" {
			condition = "no nudge configured"
		}
		fmt.Fprintf(stdout, "execution-claim-nudge: %s holds %s; quiet for %.0f s, %s (provider %s); holding claim\n", sessName, beadID, p.now.Sub(last).Seconds(), condition, provider) //nolint:errcheck // best-effort
		return
	}
	fmt.Fprintf(stdout, "execution-claim-nudge: %s holds %s; quiet for %.0f s, %d of %d nudges delivered (provider %s); draining (%s)\n", sessName, beadID, p.now.Sub(last).Seconds(), delivered, idleClaimNudgeMaxAttempts, provider, executionStalledDrainReason) //nolint:errcheck // best-effort
	if p.requestDrain == nil || sessName == "" {
		return
	}
	if err := p.requestDrain(*s); err != nil {
		fmt.Fprintf(stdout, "execution-claim-nudge: draining %s failed: %v\n", sessName, err) //nolint:errcheck // best-effort
	}
}

func (p poolExecutionBackstop) emitStepStalled(s *beads.Bead, beadID string, attempts int) {
	if p.rec == nil {
		return
	}
	rootID := strings.TrimSpace(s.Metadata[executionClaimNudgeRootKey])
	payload, err := json.Marshal(events.ExecutionStepStalledPayload{
		BeadID:     beadID,
		RootBeadID: rootID,
		SessionID:  s.ID,
		Attempts:   attempts,
	})
	if err != nil {
		return
	}
	p.rec.Record(events.Event{
		Type:      events.ExecutionStepStalled,
		Actor:     eventActor(),
		Subject:   beadID,
		RunID:     rootID,
		SessionID: s.ID,
		Payload:   payload,
	})
}

func (p poolExecutionBackstop) clear(store beads.Store, s *beads.Bead, stdout io.Writer) {
	clearExecutionClaimMarker(store, s, stdout)
}

func writeExecutionClaimMarker(store beads.Store, s *beads.Bead, target backstopTarget, attempts int, now time.Time, stdout io.Writer) bool {
	kvs := map[string]string{
		executionClaimNudgeWorkKey:     target.ID,
		executionClaimNudgeRootKey:     target.RootID,
		executionClaimNudgeStoreRefKey: target.StoreRef,
		executionClaimNudgeCountKey:    strconv.Itoa(attempts),
		executionClaimNudgeAtKey:       now.UTC().Format(time.RFC3339),
	}
	if attempts == 0 {
		kvs[executionClaimNudgeDeliveredKey] = ""
		kvs[executionClaimNudgeStalledKey] = ""
	}
	return writeSessionMetadata(store, s, kvs, stdout)
}

// clearExecutionClaimMarker wipes the state machine — including the escalation
// latch — so the next claim this slot takes starts a fresh window. No-op when
// there is nothing to clear, so steady-state ticks stay write-free.
func clearExecutionClaimMarker(store beads.Store, s *beads.Bead, stdout io.Writer) {
	keys := []string{
		executionClaimNudgeWorkKey,
		executionClaimNudgeRootKey,
		executionClaimNudgeStoreRefKey,
		executionClaimNudgeCountKey,
		executionClaimNudgeDeliveredKey,
		executionClaimNudgeAtKey,
		executionClaimNudgeStalledKey,
	}
	dirty := false
	for _, key := range keys {
		if s.Metadata[key] != "" {
			dirty = true
			break
		}
	}
	if !dirty {
		return
	}
	kvs := make(map[string]string, len(keys))
	for _, key := range keys {
		kvs[key] = ""
	}
	if !writeSessionMetadata(store, s, kvs, stdout) {
		return
	}
	for _, key := range keys {
		delete(s.Metadata, key)
	}
}

// writeSessionMetadata persists a marker patch and mirrors it into the in-memory
// session bead so the rest of this tick reads the just-written values.
func writeSessionMetadata(store beads.Store, s *beads.Bead, kvs map[string]string, stdout io.Writer) bool {
	if err := store.SetMetadataBatch(s.ID, kvs); err != nil {
		fmt.Fprintf(stdout, "execution-claim-nudge: marking %s failed: %v\n", s.ID, err) //nolint:errcheck // best-effort
		return false
	}
	if s.Metadata == nil {
		s.Metadata = make(map[string]string, len(kvs))
	}
	for k, v := range kvs {
		s.Metadata[k] = v
	}
	return true
}

// requestExecutionStalledDrain begins a TRACKED drain of a seat that claimed
// work, remained quiet, and received at least one backstop nudge.
//
// Tracked, not a bare runtime flag: the drainTracker is the machinery that
// actually converges a session — it defers the interrupt one tick so a
// false positive can still be canceled, then advances through stop, close, and
// the dead-assignee reopen that puts the claim back in the demand set. Setting
// the runtime's GC_DRAIN meta alone announces an intention that nothing drives.
//
// The reason is executionStalledDrainReason precisely because this session looks
// exactly like one every keep-alive guard exists to protect (awake, running,
// holding an in_progress claim); a cancelable reason would be canceled by the
// very claim that justified the drain.
//
// It re-reads the session through the front door rather than trusting the
// backstop's snapshot: the drain carries the session's generation, and acting on
// a stale generation is how a drain lands on the wrong incarnation.
func (cr *CityRuntime) requestExecutionStalledDrain(sessionBead beads.Bead) error {
	if cr == nil || cr.sessionDrains == nil {
		return fmt.Errorf("no drain tracker configured for %q", sessionBead.ID)
	}
	info, err := sessionFrontDoor(cr.sessionsBeadStore()).Get(sessionBead.ID)
	if err != nil {
		return fmt.Errorf("reading session %q before draining: %w", sessionBead.ID, err)
	}
	beginSessionDrainInfo(info, cr.sp, cr.sessionDrains, executionStalledDrainReason, clock.Real{}, defaultDrainTimeout)
	return nil
}
