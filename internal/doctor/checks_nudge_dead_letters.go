package doctor

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// NudgeDeadLettersCheck reports preserved inputs that could not be delivered.
type NudgeDeadLettersCheck struct {
	now func() time.Time
}

// NewNudgeDeadLettersCheck creates a read-only check of the persisted queue.
func NewNudgeDeadLettersCheck() *NudgeDeadLettersCheck {
	return &NudgeDeadLettersCheck{now: time.Now}
}

// Name returns the check identifier.
func (c *NudgeDeadLettersCheck) Name() string { return "nudge-dead-letters" }

// Run lists dead letters without running queue maintenance or opening beads.
func (c *NudgeDeadLettersCheck) Run(ctx *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name(), Status: StatusOK, Message: "no dead-letter nudges"}
	state, err := nudgequeue.LoadState(ctx.CityPath)
	if err != nil {
		r.Status, r.Severity = StatusError, SeverityAdvisory
		r.Message = err.Error()
		return r
	}
	if len(state.Dead) == 0 {
		return r
	}
	var oldest time.Time
	now := c.now()
	for _, item := range state.Dead {
		if !item.UnresolvedDeadLetter() {
			continue
		}
		if !item.DeadAt.IsZero() && (oldest.IsZero() || item.DeadAt.Before(oldest)) {
			oldest = item.DeadAt
		}
		detail := fmt.Sprintf("item %s session %s", item.ID, item.Agent)
		if item.SessionID != "" {
			detail += fmt.Sprintf(" (%s)", item.SessionID)
		}
		detail += fmt.Sprintf(" reason=%q", item.LastError)
		if item.DeadAt.IsZero() {
			detail += " age=not-recorded cap=7d expires=not-recorded"
		} else {
			age := max(now.Sub(item.DeadAt), 0).Truncate(time.Second)
			detail += fmt.Sprintf(" age=%s cap=7d expires=%s", age, item.DeadAt.Add(nudgequeue.DeadLetterRetention).UTC().Format(time.RFC3339Nano))
		}
		r.Details = append(r.Details, detail)
	}
	if len(r.Details) == 0 {
		return r
	}
	age := "not recorded"
	if !oldest.IsZero() {
		age = max(now.Sub(oldest), 0).Truncate(time.Second).String()
	}
	r.Status, r.Severity = StatusWarning, SeverityAdvisory
	r.Message = fmt.Sprintf("%d dead-letter nudges unresolved; oldest age %s; retention cap 7d", len(r.Details), age)
	return r
}

// CanFix returns false; replay requires an operator to resolve the failed input.
func (c *NudgeDeadLettersCheck) CanFix() bool { return false }

// Fix is a no-op because dead letters are retained for explicit replay.
func (c *NudgeDeadLettersCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible keeps this read-only diagnostic in on-demand doctor runs.
func (c *NudgeDeadLettersCheck) WarmupEligible() bool { return false }
