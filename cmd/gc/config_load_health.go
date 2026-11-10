package main

import (
	"encoding/json"
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
	mu      sync.Mutex
	failure *configLoadFailure
	seen    map[string]time.Time
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
	since, seen := h.seen[message]
	if !seen {
		since = now.UTC()
		h.seen[message] = since
	}
	h.failure = &configLoadFailure{Error: message, Since: since}
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
	return fmt.Sprintf("config load failing since %s; keeping last good config, reconciliation and orders paused: %s", failure.Since.UTC().Format(time.RFC3339), failure.Error)
}
