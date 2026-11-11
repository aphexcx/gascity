package session

import (
	"reflect"
	"testing"
	"time"
)

func TestStartupContentSurvivesStoreReopenAndClearsOnRestartOrClose(t *testing.T) {
	for _, end := range []string{"restart", "close"} {
		t.Run(end, func(t *testing.T) {
			backing := seedSessionStore(t, sessionBeadFixture("session-1", "open", map[string]string{"state": "active"}))
			store := NewStore(backing)
			info, err := store.Get("session-1")
			if err != nil {
				t.Fatal(err)
			}
			want := StartupContent{Sources: []StartupContentSource{{Path: "/cache/skills/plan", Hash: "accepted", Import: "/cache"}}, Skipped: true}
			if _, err := store.RecordStartupContent(info, want); err != nil {
				t.Fatal(err)
			}
			store = NewStore(backing)
			info, err = store.Get(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			got, err := DecodeStartupContent(info.StartupContent)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("reopened delivery record = %+v, %v; want %+v", got, err, want)
			}
			patch := CommitStartedPatch(CommitStartedPatchInput{CoreHash: "clean"})
			if end == "close" {
				patch = ClosePatch(time.Now(), "closed")
			}
			if err := store.ApplyPatch(info.ID, patch); err != nil {
				t.Fatal(err)
			}
			info, err = NewStore(backing).Get(info.ID)
			if err != nil || info.StartupContent != "" {
				t.Fatalf("%s retained startup repair: %q, %v", end, info.StartupContent, err)
			}
		})
	}
}
