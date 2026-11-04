package runtime

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsSessionNotFoundRequiresEveryCause(t *testing.T) {
	failure := errors.New("stop refused")
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"typed absence", ErrSessionNotFound, true},
		{"legacy wording", errors.New("session not found"), false},
		{"wrapped absence", fmt.Errorf("backend: %w", ErrSessionNotFound), true},
		{"all absent", errors.Join(ErrSessionNotFound, fmt.Errorf("fallback: %w", ErrSessionNotFound)), true},
		{"mixed", errors.Join(failure, ErrSessionNotFound), false},
		{"nested mixed", fmt.Errorf("backend: %w", errors.Join(ErrSessionNotFound, errors.Join(failure, ErrSessionNotFound))), false},
		{"multiple wrapped causes", fmt.Errorf("primary: %w; fallback: %w", failure, ErrSessionNotFound), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSessionNotFound(tc.err); got != tc.want {
				t.Fatalf("IsSessionNotFound(%v)=%v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestMergeBackendStopErrorsRetainsNestedFailure(t *testing.T) {
	failure := errors.New("live backend refused stop")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"joined", errors.Join(failure, ErrSessionNotFound)},
		{"wrapped", fmt.Errorf("nested provider: %w", errors.Join(failure, ErrSessionNotFound))},
		{"legacy absence", errors.Join(failure, errors.New("session not found"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := MergeBackendStopErrors(BackendError{Label: "primary", Err: tc.err}, BackendError{Label: "fallback", Err: ErrSessionNotFound})
			if !errors.Is(err, failure) {
				t.Fatalf("nested backend failure was suppressed: %v", err)
			}
		})
	}
	if err := MergeBackendStopErrors(BackendError{Err: errors.Join(ErrSessionNotFound, errors.New("session not found"))}); err != nil {
		t.Fatalf("all-absent legacy composition must remain idempotent: %v", err)
	}
}

func TestMergeBackendListResultsReturnsBestEffortResultsOnPartialFailure(t *testing.T) {
	t.Parallel()

	names, err := MergeBackendListResults(
		BackendListResult{Label: "local", Names: []string{"sess-a"}},
		BackendListResult{Label: "remote", Err: errors.New("backend down")},
	)
	if !IsPartialListError(err) {
		t.Fatalf("MergeBackendListResults() error = %v, want partial list error", err)
	}
	if len(names) != 1 || names[0] != "sess-a" {
		t.Fatalf("MergeBackendListResults() names = %v, want [sess-a]", names)
	}
}

func TestMergeBackendListResultsFailsWhenAllBackendsFail(t *testing.T) {
	t.Parallel()

	names, err := MergeBackendListResults(
		BackendListResult{Label: "local", Err: errors.New("local down")},
		BackendListResult{Label: "remote", Err: errors.New("remote down")},
	)
	if err == nil {
		t.Fatal("MergeBackendListResults() error = nil, want joined error")
	}
	if IsPartialListError(err) {
		t.Fatalf("MergeBackendListResults() error = %v, want total failure not partial", err)
	}
	if names != nil {
		t.Fatalf("MergeBackendListResults() names = %v, want nil", names)
	}
}

func TestMergeBackendListResultsPreservesNamesWhenAllBackendsAreDegraded(t *testing.T) {
	t.Parallel()

	names, err := MergeBackendListResults(
		BackendListResult{Label: "local", Names: []string{"sess-a"}, Err: errors.New("local degraded")},
		BackendListResult{Label: "remote", Names: []string{"sess-b"}, Err: errors.New("remote degraded")},
	)
	if !IsPartialListError(err) {
		t.Fatalf("MergeBackendListResults() error = %v, want partial list error", err)
	}
	if len(names) != 2 || names[0] != "sess-a" || names[1] != "sess-b" {
		t.Fatalf("MergeBackendListResults() names = %v, want [sess-a sess-b]", names)
	}
}
