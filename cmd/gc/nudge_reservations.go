package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

func releaseLiveNudgeReservation(target nudgeTarget, sp runtime.Provider, id string) error {
	return releaseNudgeReservation(target, sp, id, false)
}

func releaseNudgeReservation(target nudgeTarget, sp runtime.Provider, id string, invalidate bool) error {
	if id == "" || target.sessionName == "" {
		return nil
	}
	handle, err := worker.NewRuntimeHandle(worker.RuntimeHandleConfig{Provider: sp, SessionName: target.sessionName})
	if err != nil {
		return err
	}
	if err := handle.ReleaseNudgeReservation(context.Background(), id, invalidate); err != nil {
		return fmt.Errorf("releasing nudge reservation %q: %w", id, err)
	}
	return nil
}

type nudgeReservationRelease = nudgequeue.ReservationRelease

func retryNudgeReservationReleases(cityPath string) error {
	state, err := nudgequeue.LoadState(cityPath)
	if err != nil || len(state.ReservationReleases) == 0 {
		return err
	}
	if err := nudgeReleaseEndedReservations(cityPath, state.ReservationReleases); err != nil {
		return err
	}
	return nudgequeue.AcknowledgeReservationReleases(cityPath, state.ReservationReleases)
}

// Release occurs after the queue commit and outside its lock. A cleanup failure
// is reported without reversing a terminal queue transition. Durable intent
// blocks unsafe recovery and remains pending until cleanup succeeds.
func releaseEndedNudgeReservations(cityPath string, items []nudgeReservationRelease) error {
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
		result = errors.Join(result, releaseQueuedNudgeReservation(sp, item))
	}
	return result
}

func releaseQueuedNudgeReservation(sp runtime.Provider, item nudgeReservationRelease) error {
	err := releaseNudgeReservation(nudgeTarget{sessionName: item.SessionName}, sp, item.ReservationID, item.Invalidate)
	if err == nil && item.RetireReceipt {
		// Revoke recovery first. Retiring the receipt cannot make a
		// canceled paste recoverable if the second provider call fails.
		err = releaseLiveNudgeReservation(nudgeTarget{sessionName: item.SessionName}, sp, item.ReservationID)
	}
	// A lookup or executable failure containing "not found" is not proof
	// that the session is gone. Only the provider's typed absence is final.
	if errors.Is(err, runtime.ErrSessionNotFound) {
		return nil
	}
	return err
}
