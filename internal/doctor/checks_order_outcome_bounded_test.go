package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

func TestOrderOutcomeHistorySinglePass(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	var journal bytes.Buffer
	enc := json.NewEncoder(&journal)
	types := []string{events.OrderCompleted, events.OrderFailed, events.ControllerStarted, events.BeadCreated}
	for i := 0; i < 10000; i++ {
		if err := enc.Encode(events.Event{Seq: uint64(i + 1), Type: types[i%len(types)], Ts: time.Unix(int64(i), 0)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, journal.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	walk := func(ctx context.Context, path string, visit func(events.Event) error) error {
		calls++
		return events.WalkContext(ctx, path, visit)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	outcomes, starts, err := readOrderOutcomeHistory(ctx, path, walk)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(outcomes) != 5000 || len(starts) != 2500 {
		t.Fatalf("calls=%d outcomes=%d starts=%d", calls, len(outcomes), len(starts))
	}
	for i := 1; i < len(outcomes); i++ {
		if outcomes[i-1].Seq >= outcomes[i].Seq {
			t.Fatal("outcomes out of sequence")
		}
	}
}

func TestOrderOutcomeSkipsHistoryWithoutMonitoredOrders(t *testing.T) {
	c := NewOrderOutcomeHealthyCheck(&config.City{}, t.TempDir())
	c.scanOrders = func(string, *config.City) ([]orders.Order, error) {
		return []orders.Order{{Name: "manual", Trigger: "manual"}}, nil
	}
	c.walkEvents = func(context.Context, string, func(events.Event) error) error {
		t.Fatal("history walked with no monitored orders")
		return nil
	}
	got := c.Run(nil)
	if got.Status != StatusOK || !strings.Contains(got.Message, "no cron or cooldown orders") {
		t.Fatalf("result=%+v", got)
	}
}

func TestOrderOutcomeCachedHistoryPreservesFailuresAndGrace(t *testing.T) {
	city, cfg := orderFiringTestCity(t)
	writeOrderFiringTestOrder(t, city, "scheduled", "cooldown", "5m")
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	records := []events.Event{
		{Seq: 1, Type: events.OrderFailed, Subject: "scheduled", Ts: start.Add(-3 * time.Hour)},
		{Seq: 2, Type: events.OrderFailed, Subject: "scheduled", Ts: start.Add(-2 * time.Hour)},
		{Seq: 3, Type: events.OrderFailed, Subject: "scheduled", Ts: start.Add(-time.Hour)},
		{Seq: 4, Type: events.ControllerStarted, Ts: start},
		{Seq: 5, Type: events.OrderFailed, Subject: "scheduled", Ts: start.Add(time.Minute)},
	}
	var body bytes.Buffer
	for _, e := range records {
		if err := json.NewEncoder(&body).Encode(e); err != nil {
			t.Fatal(err)
		}
	}
	writeOrderFiringArchive(t, city, "20260801T000100Z", 1, 5, body.String())
	c := NewOrderOutcomeHealthyCheck(cfg, city)
	for _, phase := range []string{"cold fill complete", "warm cache"} {
		got := c.Run(nil)
		if got.Status != StatusWarning || len(got.Details) != 1 || !strings.Contains(got.Details[0], "3 consecutive failures") || !strings.Contains(got.Message, phase) || !strings.Contains(got.Message, "after seq 0, archives 1 of 1") {
			t.Fatalf("%s result=%+v", phase, got)
		}
	}
	// Same timestamp, greater Seq: a success still terminates the streak.
	writeOrderFiringTestEvents(t, city, events.Event{Seq: 6, Type: events.OrderCompleted, Subject: "scheduled", Ts: start.Add(time.Minute)})
	if got := c.Run(nil); got.Status != StatusOK || !strings.Contains(got.Details[0], "last run succeeded") {
		t.Fatalf("active success ignored: %+v", got)
	}
}

func TestOrderOutcomeAbandonmentNamesPhase(t *testing.T) {
	for _, phase := range []string{"scan orders", "read order outcome and controller start events"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				c := NewOrderOutcomeHealthyCheck(&config.City{}, "city")
				c.scanOrders = func(string, *config.City) ([]orders.Order, error) {
					if phase == "scan orders" {
						<-release
					}
					return []orders.Order{{Name: "scheduled", Trigger: "cooldown"}}, nil
				}
				c.walkEvents = func(ctx context.Context, _ string, _ func(events.Event) error) error {
					<-ctx.Done()
					return ctx.Err()
				}
				d := &Doctor{CheckTimeout: time.Minute, UseCheckTimeouts: true}
				d.Register(c)
				r := d.RunCollect(nil, false)
				close(release)
				d.Wait()
				got := r.Results[0]
				if !got.TimedOut || got.Severity != SeverityAdvisory || !strings.Contains(got.Message, "abandoned during: "+phase) {
					t.Fatalf("result = %+v", got)
				}
				var out bytes.Buffer
				PrintSummary(&out, r)
				if !strings.Contains(out.String(), fmt.Sprintf("%s=%s", c.Name(), c.DefaultTimeout())) {
					t.Fatalf("budget missing: %s", out.String())
				}
			})
		})
	}
}

func TestOrderOutcomeExplicitBudgetWins(t *testing.T) {
	for _, budget := range []time.Duration{time.Second, 0} {
		t.Run(budget.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := NewOrderOutcomeHealthyCheck(&config.City{}, "city")
				c.scanOrders = func(string, *config.City) ([]orders.Order, error) {
					return []orders.Order{{Name: "scheduled", Trigger: "cooldown"}}, nil
				}
				c.walkEvents = func(ctx context.Context, _ string, _ func(events.Event) error) error {
					if budget == 0 {
						if _, ok := ctx.Deadline(); ok {
							t.Error("disabled timeout has deadline")
						}
						return nil
					}
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) != budget {
						t.Errorf("deadline = %v, want %s", deadline, budget)
					}
					<-ctx.Done()
					return ctx.Err()
				}
				d := &Doctor{CheckTimeout: budget}
				d.Register(c)
				report := d.RunCollect(nil, false)
				d.Wait()
				if got := report.Results[0].TimedOut; got != (budget > 0) {
					t.Fatalf("TimedOut=%v", got)
				}
				var out bytes.Buffer
				PrintSummary(&out, report)
				if strings.Contains(out.String(), "check budgets") {
					t.Fatalf("explicit budget called a default: %s", out.String())
				}
			})
		})
	}
}
