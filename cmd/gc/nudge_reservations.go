package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

func releaseLiveNudgeReservation(target nudgeTarget, sp runtime.Provider, id string) error {
	if id == "" || target.sessionName == "" {
		return nil
	}
	handle, err := worker.NewRuntimeHandle(worker.RuntimeHandleConfig{Provider: sp, SessionName: target.sessionName})
	if err != nil {
		return err
	}
	if err := handle.ReleaseNudgeReservation(context.Background(), id); err != nil {
		return fmt.Errorf("releasing nudge reservation %q: %w", id, err)
	}
	return nil
}

// Release occurs after the queue commit and outside its lock. A cleanup failure
// is reported without reversing a terminal queue transition; the pane's expiry
// remains the backstop for a failed provider call or a process death here.
func releaseEndedNudgeReservations(cityPath string, items []queuedNudge) error {
	if len(items) == 0 {
		return nil
	}
	cfg, err := loadCityConfig(cityPath, nudgeWarningWriter)
	if err != nil {
		return err
	}
	sp, err := newSessionProviderForCity(cfg, cityPath)
	if err != nil {
		return err
	}
	var result error
	for _, item := range items {
		result = errors.Join(result, releaseLiveNudgeReservation(nudgeTarget{sessionName: item.SessionName}, sp, item.ReservationID))
	}
	return result
}
