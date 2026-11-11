package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

const configLoadStatusCommand = "config-load-status"

type configLoadFailure struct {
	Error string    `json:"error"`
	Since time.Time `json:"since"`
}

type configLoadStatusReply struct {
	Failure *configLoadFailure `json:"failure,omitempty"`
}

// configLoadHealth is shared by the serial reconcile loop and socket readers.
// It lives with the runtime, so a stopped controller cannot leave stale health.
type configLoadHealth struct {
	mu           sync.Mutex
	failure      *configLoadFailure
	seen         map[string]time.Time
	seenWarnings map[string]struct{}
}

func (h *configLoadHealth) snapshot() *configLoadFailure {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failure == nil {
		return nil
	}
	failure := *h.failure
	return &failure
}

func (h *configLoadHealth) record(err error, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seen == nil {
		h.seen = make(map[string]time.Time)
	}
	message := err.Error()
	_, seen := h.seen[message]
	if !seen {
		h.seen[message] = now.UTC()
	}
	since := now.UTC()
	if h.failure != nil {
		since = h.failure.Since
	}
	h.failure = &configLoadFailure{Error: message, Since: since}
	return !seen
}

func (h *configLoadHealth) recordWarning(message string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seenWarnings == nil {
		h.seenWarnings = make(map[string]struct{})
	}
	_, seen := h.seenWarnings[message]
	h.seenWarnings[message] = struct{}{}
	return !seen
}

func (h *configLoadHealth) clear() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failure = nil
	h.seen = nil
	h.seenWarnings = nil
}

var readConfigLoadFailureHook = readConfigLoadFailure

func readConfigLoadFailure(cityPath string) (*configLoadFailure, error) {
	const timeout = 750 * time.Millisecond
	data, err := sendControllerCommandWithTimeouts(cityPath, configLoadStatusCommand, timeout, timeout, timeout)
	if err != nil {
		return nil, err
	}
	var reply configLoadStatusReply
	if err := json.Unmarshal(data, &reply); err != nil {
		return nil, fmt.Errorf("reading controller config load status: %w", err)
	}
	return reply.Failure, nil
}

func configLoadFailureMessage(failure *configLoadFailure) string {
	return fmt.Sprintf("config reload failing since %s; keeping last good config: %s", failure.Since.UTC().Format(time.RFC3339), failure.Error)
}

// finishConfigReloadHealth reports rejected reloads until a replacement is
// accepted. Reconciliation continues on the last accepted configuration.
func (cr *CityRuntime) finishConfigReloadHealth(reply reloadControlReply) {
	switch reply.Outcome {
	case reloadOutcomeFailed:
		if cr.configLoadHealth == nil {
			cr.configLoadHealth = &configLoadHealth{}
		}
		if cr.configLoadHealth.record(errors.New(reply.Error), time.Now()) {
			fmt.Fprintf(cr.stderr, "%s: %s\n", cr.logPrefix, configLoadFailureMessage(cr.configLoadHealth.snapshot())) //nolint:errcheck // best-effort stderr
		}
	case reloadOutcomeApplied, reloadOutcomeNoChange:
		cr.configLoadHealth.clear()
	default:
		return
	}
	for _, warning := range reply.Warnings {
		if reply.Outcome == reloadOutcomeFailed && !cr.configLoadHealth.recordWarning(warning) {
			continue
		}
		fmt.Fprintf(cr.stderr, "%s: warning: %s\n", cr.logPrefix, warning) //nolint:errcheck // best-effort stderr
	}
}
