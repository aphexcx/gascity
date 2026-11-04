package t3bridge

import (
	"errors"
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
