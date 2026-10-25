package doctor

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

const (
	orderOutcomeHealthyName = "order-outcome-healthy"

	// orderOutcomeInspectHintFmt mirrors the sibling check's
	// orderFiringInspectHintFmt so the pair emits consistently-shaped hints.
	orderOutcomeInspectHintFmt = "Inspect with: gc order check && gc order history %s"

	// orderOutcomeFailureThreshold is the consecutive-failure count that flags an
	// order. Three, not two: two in a row is a plausible transient for anything
	// touching the network or a lock. Three, not five: on a 6h order five failures
	// is 30h before anything surfaces.
	orderOutcomeFailureThreshold = 3

	// orderOutcomeStartGrace covers gastownhall/gascity#3898 — for ~5 minutes
	// after a supervisor start, exec orders fail spuriously because dispatch
	// begins before pack staging completes. 2x margin on the observed window.
	orderOutcomeStartGrace = 10 * time.Minute
)

// nearControllerStart reports whether ts falls within grace after any controller
// start. Every start is checked, not just the newest: two restarts with no
// successful run between them would otherwise leave the older burst counted and
// manufacture a false positive.
func nearControllerStart(ts time.Time, starts []time.Time, grace time.Duration) bool {
	if grace <= 0 {
		return false
	}
	for _, start := range starts {
		if start.IsZero() || ts.Before(start) {
			continue
		}
		if ts.Sub(start) <= grace {
			return true
		}
	}
	return false
}

// consecutiveOrderFailures counts trailing order.failed events for one order.
//
// outcomes must hold order.completed and order.failed events ordered by Seq
// ascending; the walk runs newest-first and stops at the first success.
//
// A success always ends the streak, even if it falls within the post-start grace
// window. The grace window skips only spurious FAILURES (neither counting them nor
// allowing them to break the streak); a success is proof the order works.
//
// Failures inside the post-start grace window are SKIPPED, not reset. Resetting
// would let a frequently-restarting city zero a genuinely broken order's streak
// on every restart, which is the opposite of what this check is for.
//
// sawOutcome distinguishes "ran and succeeded" from "never produced an outcome";
// order-firing-current already owns the never-fired case.
//
// skipped counts trailing failures that were inside the post-start grace
// window and therefore excluded from streak. Callers need this to avoid
// reporting "last run succeeded" when every trailing run actually failed but
// was suppressed as a spurious post-restart burst — see classifyOrderOutcome.
func consecutiveOrderFailures(outcomes []events.Event, subject string, starts []time.Time, grace time.Duration) (streak int, lastMessage string, sawOutcome bool, skipped int) {
	lastMessageSet := false

	for i := len(outcomes) - 1; i >= 0; i-- {
		event := outcomes[i]
		if event.Subject != subject {
			continue
		}
		sawOutcome = true
		if event.Type != events.OrderFailed {
			break
		}
		if nearControllerStart(event.Ts, starts, grace) {
			skipped++
			continue
		}
		streak++
		if !lastMessageSet {
			lastMessage = event.Message
			lastMessageSet = true
		}
	}

	return streak, lastMessage, sawOutcome, skipped
}

// classifyOrderOutcome turns one order's failure streak into a doctor result.
//
// skipped is the grace-window-skipped-failure count from consecutiveOrderFailures.
// When streak == 0 but skipped > 0, every trailing run actually failed (just
// inside the post-start grace window); reporting "last run succeeded" would be
// a false statement in the diagnostic tool at exactly the moment an operator is
// most likely reading it — just after a restart.
func classifyOrderOutcome(order orders.Order, streak int, threshold int, lastMessage string, sawOutcome bool, skipped int) (CheckStatus, string) {
	name := orderDisplayName(order)

	if !sawOutcome {
		return StatusOK, fmt.Sprintf("%s: no completed runs yet", name)
	}
	if streak == 0 {
		if skipped > 0 {
			return StatusOK, fmt.Sprintf("%s: %d recent failure(s) within controller-start grace window", name, skipped)
		}
		return StatusOK, fmt.Sprintf("%s: last run succeeded", name)
	}
	if streak < threshold {
		return StatusOK, fmt.Sprintf("%s: %d consecutive failure(s), under threshold %d", name, streak, threshold)
	}

	detail := fmt.Sprintf("%s: %d consecutive failures", name, streak)
	if strings.TrimSpace(lastMessage) != "" {
		detail = fmt.Sprintf("%s, last %q", detail, lastMessage)
	}
	return StatusWarning, detail
}

// OrderOutcomeHealthyCheck reports scheduled orders failing repeatedly.
//
// Sibling to OrderFiringCurrentCheck, which answers "did it run?" while this
// answers "did it succeed?". An order that fires faithfully on schedule and fails
// every time leaves order-firing-current green, so the failure is invisible: the
// only trace is order.failed in the event log, which nobody reads unprompted.
type OrderOutcomeHealthyCheck struct {
	cfg        *config.City
	cityPath   string
	threshold  int
	grace      time.Duration
	scanOrders func(string, *config.City) ([]orders.Order, error)
	walkEvents func(context.Context, string, func(events.Event) error) error
}

// NewOrderOutcomeHealthyCheck creates the repeated-order-failure check.
func NewOrderOutcomeHealthyCheck(cfg *config.City, cityPath string) *OrderOutcomeHealthyCheck {
	return &OrderOutcomeHealthyCheck{
		cfg:        cfg,
		cityPath:   cityPath,
		threshold:  orderOutcomeFailureThreshold,
		grace:      orderOutcomeStartGrace,
		scanOrders: scanOrderFiringCurrentOrders,
	}
}

// Name returns the check identifier shown by gc doctor.
func (c *OrderOutcomeHealthyCheck) Name() string { return orderOutcomeHealthyName }

// CanFix reports whether the check can repair a failing order. It cannot:
// remediation depends entirely on why the order fails.
func (c *OrderOutcomeHealthyCheck) CanFix() bool { return false }

// Fix is a no-op for the reason given on CanFix.
func (c *OrderOutcomeHealthyCheck) Fix(_ *CheckContext) error { return nil }

// DefaultTimeout bounds the archive diagnostic. On citadel (gf-s6z), the
// first of three history reads was still decompressing 3.53 GB of retained
// archives after 110s; the earlier full probe exceeded 10m. Like the sibling
// freshness check, spend at most 15s before reporting an inconclusive result.
// The runner reports this budget and honors an explicit --check-timeout.
func (c *OrderOutcomeHealthyCheck) DefaultTimeout() time.Duration { return 15 * time.Second }

// Run counts each scheduled order's trailing consecutive failures. The runner
// bounds the scan and reports its phase; the event walker observes cancellation
// within archives so abandoned reads stop consuming CPU and memory.
func (c *OrderOutcomeHealthyCheck) Run(ctx *CheckContext) *CheckResult {
	// This check is advisory on every path: a failing order must not gate gc doctor.
	// Blocking would fail gc doctor outright and gate every clean-doctor dependency
	// on transient order breakage, including during maintenance. SeverityAdvisory
	// is the only source of truth, set here at construction.
	result := &CheckResult{Name: c.Name(), Severity: SeverityAdvisory}
	if c.cfg == nil {
		result.Status = StatusOK
		result.Message = "no city config loaded"
		return result
	}

	cityPath := c.cityPath
	if cityPath == "" && ctx != nil {
		cityPath = ctx.CityPath
	}
	if cityPath == "" {
		result.Status = StatusError
		result.Message = "city path unavailable"
		return result
	}

	// Same helper order-firing-current uses, so the two checks can never
	// disagree about which orders are in scope.
	ctx.SetPhase("scan orders")
	allOrders, err := c.scanOrders(cityPath, c.cfg)
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("scan orders: %v", err)
		return result
	}

	var monitoredOrders []orders.Order
	suspendedRigs := orderFiringCurrentSuspendedRigs(c.cfg, cityPath)
	for _, order := range allOrders {
		if (order.Trigger == "cron" || order.Trigger == "cooldown") && !orderFiringCurrentOrderSuspended(suspendedRigs, order) {
			monitoredOrders = append(monitoredOrders, order)
		}
	}
	if len(monitoredOrders) == 0 {
		result.Status = StatusOK
		result.Message = "no cron or cooldown orders; history read skipped"
		return result
	}

	eventPath := filepath.Join(cityPath, citylayout.RuntimeRoot, "events.jsonl")
	workCtx := context.Background()
	if ctx != nil && ctx.Context != nil {
		workCtx = ctx.Context
	}
	ctx.SetPhase("read order outcome and controller start events")
	var historyProgress string
	walk := c.walkEvents
	if walk == nil {
		walk = func(workCtx context.Context, path string, visit func(events.Event) error) error {
			return events.WalkCachedContext(workCtx, path, events.ArchiveCacheOptions{
				Dir:      filepath.Join(cityPath, citylayout.RuntimeRoot, "doctor-cache", "order-outcome"),
				Types:    []string{events.OrderCompleted, events.OrderFailed, events.ControllerStarted},
				MaxBytes: 256 << 20,
				// Order discovery has no creation sequence. The full retained
				// history is needed for old failure streaks and restart grace.
				AfterSeq: 0,
				Progress: func(p events.ArchiveCacheProgress) {
					historyProgress = fmt.Sprintf("after seq %d, archives %d of %d, cache hits %d, misses %d, %s", p.AfterSeq, p.Index, p.Archives, p.Hits, p.Misses, p.Phase)
					ctx.SetPhase("read order outcome and controller start events " + historyProgress)
				},
			}, visit)
		}
	}
	outcomes, starts, err := readOrderOutcomeHistory(workCtx, eventPath, walk)
	if err != nil {
		result.Status = StatusError
		result.Message = fmt.Sprintf("read order outcome and controller start events %s: %v", historyProgress, err)
		return result
	}
	ctx.SetPhase("classify order outcomes")

	worst := StatusOK
	failing := 0
	var firstFailingHint string

	for _, order := range monitoredOrders {
		streak, lastMessage, sawOutcome, skipped := consecutiveOrderFailures(outcomes, order.ScopedName(), starts, c.grace)
		status, detail := classifyOrderOutcome(order, streak, c.threshold, lastMessage, sawOutcome, skipped)
		worst = worseStatus(worst, status)
		result.Details = append(result.Details, detail)
		if status != StatusOK {
			failing++
			if firstFailingHint == "" {
				// gc order history takes a bare name positionally and filters
				// on a.Name; the scoped form matches zero orders on the
				// local-iterator path used when the supervisor API is
				// unavailable — exactly when someone is debugging a broken
				// city. orderHistoryHintTarget yields the --rig form instead.
				firstFailingHint = orderHistoryHintTarget(order)
			}
		}
	}

	result.Status = worst
	if worst == StatusOK {
		result.Message = "all scheduled orders succeeding"
	} else {
		result.Message = fmt.Sprintf("%d order(s) failing repeatedly", failing)
		result.FixHint = fmt.Sprintf(orderOutcomeInspectHintFmt, firstFailingHint)
	}
	if historyProgress != "" {
		result.Message += " (" + historyProgress + ")"
	}
	return result
}

// readOrderOutcomeHistory collects all three event types in one retained-history
// traversal. Only relevant events remain in memory; errors discard partial
// history so an incomplete scan can never produce a healthy verdict.
func readOrderOutcomeHistory(ctx context.Context, path string, walk func(context.Context, string, func(events.Event) error) error) ([]events.Event, []time.Time, error) {
	var outcomes []events.Event
	var starts []time.Time
	err := walk(ctx, path, func(event events.Event) error {
		switch event.Type {
		case events.OrderCompleted, events.OrderFailed:
			outcomes = append(outcomes, event)
		case events.ControllerStarted:
			starts = append(starts, event.Ts)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	// Sequence, not timestamp, determines order even for events in the same second.
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].Seq < outcomes[j].Seq })
	return outcomes, starts, nil
}
