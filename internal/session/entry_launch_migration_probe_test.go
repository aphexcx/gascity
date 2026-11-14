package session

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestInterruptMigrationPreservesEntryLaunch(t *testing.T) {
	for _, tc := range []struct {
		name                string
		legacyMCP, prepared bool
	}{
		{"prepared", false, true},
		{"prepared_migration", true, true},
		{"cached", false, false},
		{"cached_migration", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			metadata := map[string]string{
				"session_name": "probe-runtime", "template": "runner", "provider": "test-provider",
				"provider_kind": "codex", "state": "active", "command": "test-provider",
				"session_origin": "named", NamedSessionMetadataKey: "true", NamedSessionIdentityMetadata: "runner",
			}
			if tc.legacyMCP {
				metadata[MCPIdentityMetadataKey] = "runner"
			}
			b, err := store.Create(beads.Bead{Type: BeadType, Metadata: metadata})
			if err != nil {
				t.Fatal(err)
			}
			if err := sp.Start(t.Context(), "probe-runtime", runtime.Config{Command: "test-provider"}); err != nil {
				t.Fatal(err)
			}
			sp.InterruptBoundaryErrors["probe-runtime"] = errors.New("injected boundary failure")
			cheapCalls := 0
			mgr := NewManagerWithOptions(store, sp,
				WithTransportResolver(func(_, _ string) string { return "tmux" }),
				WithResumeConfigResolver(func(Info, map[string]string) (string, ProviderResume, runtime.Config, error) {
					cheapCalls++
					return "test-provider", ProviderResume{ResumeFlag: "--resume", ResumeStyle: "flag"}, runtime.Config{Env: map[string]string{"CHEAP_RESOLVER": "1"}}, nil
				}),
				WithStaleKeyDetectionWaiter(func(context.Context, string) error { return nil }),
			)
			fullLaunch := runtime.Config{Command: "test-provider --entry-launch", Env: map[string]string{
				"TEST_MODEL_BASE_URL":   "https://model.example.invalid",
				"TEST_MODEL_AUTH_TOKEN": "invented-test-token",
			}}
			if !tc.prepared {
				fullLaunch.Command = ""
			}
			_, err = mgr.Submit(t.Context(), b.ID, "replacement turn", "test-provider --entry-launch", fullLaunch, SubmitIntentInterruptNow)
			if err != nil {
				t.Fatal(err)
			}
			if sp.CountCalls("Start", "probe-runtime") != 2 {
				t.Fatal("expected exactly one replacement start")
			}
			launch := sp.LastStartConfig("probe-runtime")
			wantCalls := 0
			if tc.legacyMCP {
				wantCalls = 1
				stored, err := store.Get(b.ID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.Metadata[MCPIdentityMetadataKey] != "" || stored.Metadata["command"] != "test-provider" || stored.Metadata["resume_flag"] != "--resume" || stored.Metadata["resume_style"] != "flag" {
					t.Error("durable provider migration was not preserved")
				}
			}
			if cheapCalls != wantCalls {
				t.Errorf("cheap resolver calls=%d want=%d", cheapCalls, wantCalls)
			}
			if tc.legacyMCP && !tc.prepared {
				if launch.Command != "test-provider" || launch.Env["CHEAP_RESOLVER"] != "1" {
					t.Error("unprepared control did not consume migrated launch")
				}
				return
			}
			for _, key := range []string{"TEST_MODEL_BASE_URL", "TEST_MODEL_AUTH_TOKEN"} {
				if launch.Env[key] == "" {
					t.Errorf("replacement launch lost %s", key)
				}
			}
			if launch.Command != "test-provider --entry-launch" {
				t.Error("replacement launch lost the entry command")
			}
		})
	}
}
