package t3bridge

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSeamBackedShutdownComplete(t *testing.T) {
	cases := append(t3SessionStatusExpectations(), t3SessionStatusExpectation{name: "malformed binding", status: "stopped", includeStatus: true, malformed: true, wantUnavailable: true})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backed, sp := newSeamBackedSnapshotTestProvider(t, "ws://127.0.0.1:1/ws")
			backed.raw.cacheSnapshot(t3SessionStatusSnapshot("worker", tc))
			completion, ok := sp.(interface{ ShutdownComplete(string) (bool, error) })
			if !ok {
				t.Fatal("production adapter lacks shutdown completion observation")
			}
			complete, err := completion.ShutdownComplete("worker")
			if complete != (!tc.wantLive && !tc.wantUnavailable) || (err != nil) != tc.wantUnavailable || errors.Is(err, runtime.ErrRuntimeUnavailable) != tc.wantUnavailable {
				t.Fatalf("complete=%v err=%v; want live=%v unavailable=%v", complete, err, tc.wantLive, tc.wantUnavailable)
			}
		})
	}
	for _, scenario := range []string{"absent", "unavailable", "recent absent", "recent stopped"} {
		t.Run(scenario, func(t *testing.T) {
			backed, sp := newSeamBackedSnapshotTestProvider(t, "ws://127.0.0.1:1/ws")
			if scenario != "unavailable" {
				snapshot := map[string]interface{}{"threads": []interface{}{}}
				if scenario == "recent stopped" {
					snapshot = t3SessionStatusSnapshot("worker", t3SessionStatusExpectation{status: "stopped", includeStatus: true})
				}
				backed.raw.cacheSnapshot(snapshot)
			}
			if scenario == "recent absent" || scenario == "recent stopped" {
				backed.raw.setRecentStart("worker", time.Now())
			}
			completion, ok := sp.(interface{ ShutdownComplete(string) (bool, error) })
			if !ok {
				t.Fatal("production adapter lacks shutdown completion observation")
			}
			complete, err := completion.ShutdownComplete("worker")
			if complete != (scenario == "absent") || (err != nil) != (scenario == "unavailable") {
				t.Fatalf("complete=%v err=%v", complete, err)
			}
		})
	}
}

type shutdownSnapshotTransport func(*http.Request) (*http.Response, error)

// RoundTrip serves the snapshot through the test's transport callback.
func (f shutdownSnapshotTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestSeamBackedShutdownCompleteStartDuringSnapshot(t *testing.T) {
	backed, sp := newSeamBackedSnapshotTestProvider(t, "ws://127.0.0.1:1/ws")
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = shutdownSnapshotTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/orchestration/snapshot" {
			return nil, errors.New("unexpected request path " + r.URL.Path)
		}
		// A start can be recorded while the snapshot RPC is in flight, before
		// the new thread has materialized in that snapshot.
		backed.raw.setRecentStart("worker", time.Now())
		return &http.Response{
			StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"threads":[]}`)),
		}, nil
	})
	completion, ok := sp.(interface{ ShutdownComplete(string) (bool, error) })
	if !ok {
		t.Fatal("production adapter lacks shutdown completion observation")
	}
	if complete, err := completion.ShutdownComplete("worker"); complete || err != nil {
		t.Fatalf("complete=%v err=%v; want incomplete during recent-start grace", complete, err)
	}
}

func TestSeamBackedShutdownCompleteDuplicateThreads(t *testing.T) {
	for _, tc := range []struct {
		name            string
		status          string
		state           string
		deleted         bool
		malformed       bool
		otherName       bool
		newerLive       bool
		wantComplete    bool
		wantUnavailable bool
	}{
		{name: "older running", status: "running"},
		{name: "both stopped", status: "stopped", wantComplete: true},
		{name: "older unknown", status: "future-state", wantUnavailable: true},
		{name: "older malformed", status: "stopped", malformed: true, wantUnavailable: true},
		{name: "archived running", status: "running", state: "archived", wantComplete: true},
		{name: "deleted running", status: "running", deleted: true, wantComplete: true},
		{name: "other session running", status: "running", otherName: true, wantComplete: true},
		{name: "live does not mask unknown", status: "future-state", newerLive: true, wantUnavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backed, sp := newSeamBackedSnapshotTestProvider(t, "ws://127.0.0.1:1/ws")
			snapshot := t3SessionStatusSnapshot("worker", t3SessionStatusExpectation{status: "stopped", includeStatus: true})
			newer := snapshotThreads(snapshot)[0]
			newer["updatedAt"] = "2026-02-01T00:00:00Z"
			if tc.newerLive {
				newer["session"] = map[string]interface{}{"status": "running"}
			}
			older := snapshotThreads(t3SessionStatusSnapshot("worker", t3SessionStatusExpectation{
				status: tc.status, includeStatus: true, malformed: tc.malformed,
			}))[0]
			older["id"] = "thread-older"
			older["updatedAt"] = "2026-01-01T00:00:00Z"
			for _, thread := range []map[string]interface{}{newer, older} {
				thread["customMetadata"].(map[string]interface{})["gc.sessionEnv"] = `{"GC_CITY_PATH":"/cities/test","GC_SESSION_ID":"session-1"}`
			}
			meta := older["customMetadata"].(map[string]interface{})
			meta["gc.state"] = tc.state
			if tc.otherName {
				meta["gc.sessionName"] = "other"
			}
			if tc.deleted {
				older["deletedAt"] = "2026-02-02T00:00:00Z"
			}
			// Put the newest first so a live result cannot hide later uncertainty.
			snapshot["threads"] = []interface{}{newer, older}
			backed.raw.cacheSnapshot(snapshot)
			completion, ok := sp.(interface{ ShutdownComplete(string) (bool, error) })
			if !ok {
				t.Fatal("production adapter lacks shutdown completion observation")
			}
			complete, err := completion.ShutdownComplete("worker")
			if complete != tc.wantComplete || (err != nil) != tc.wantUnavailable || errors.Is(err, runtime.ErrRuntimeUnavailable) != tc.wantUnavailable {
				t.Fatalf("complete=%v err=%v; want complete=%v unavailable=%v", complete, err, tc.wantComplete, tc.wantUnavailable)
			}
		})
	}
}
