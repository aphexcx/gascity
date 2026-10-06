package doctor

import (
	"strings"
	"testing"
)

func TestSupervisorLaunchdSpawnTypeCheck_Metadata(t *testing.T) {
	c := NewSupervisorLaunchdSpawnTypeCheck(SupervisorLaunchdSpawnType{}, true)
	if c.Name() != "supervisor-launchd-spawn-type" {
		t.Errorf("Name() = %q, want %q", c.Name(), "supervisor-launchd-spawn-type")
	}
	if c.CanFix() {
		t.Error("CanFix() = true, want false")
	}
	if c.WarmupEligible() {
		t.Error("WarmupEligible() = true, want false")
	}
}

// TestSupervisorLaunchdSpawnTypeCheckRun pins the hq-a6cny contract: a
// loaded job in a non-interactive spawn type with agents configured warns, a
// job launchctl could not read warns with launchctl's exit status (never its
// output) whether or not agents are configured, and every other state is OK.
func TestSupervisorLaunchdSpawnTypeCheckRun(t *testing.T) {
	const label = "com.gascity.supervisor"
	cases := []struct {
		name             string
		probe            SupervisorLaunchdSpawnType
		agentsConfigured bool
		wantStatus       CheckStatus
		wantMessage      string
		wantFixHint      []string
	}{
		{
			name:             "job not loaded",
			probe:            SupervisorLaunchdSpawnType{Label: label},
			agentsConfigured: true,
			wantStatus:       StatusOK,
			wantMessage:      "is not loaded",
		},
		{
			name:             "job unreadable",
			probe:            SupervisorLaunchdSpawnType{Label: label, Unreadable: true, ExitStatus: 5},
			agentsConfigured: true,
			wantStatus:       StatusWarning,
			wantMessage:      "could not read launchd job " + label + ", so its spawn type is unknown: 'launchctl print gui/$(id -u)/" + label + "' failed (exit status 5)",
			wantFixHint:      []string{"run 'launchctl print gui/$(id -u)/" + label + "' by hand"},
		},
		{
			name:             "job unreadable without an exit status or agents",
			probe:            SupervisorLaunchdSpawnType{Label: label, Unreadable: true},
			agentsConfigured: false,
			wantStatus:       StatusWarning,
			wantMessage:      "'launchctl print gui/$(id -u)/" + label + "' failed (no exit status)",
			wantFixHint:      []string{"run 'launchctl print gui/$(id -u)/" + label + "' by hand"},
		},
		{
			name:             "interactive",
			probe:            SupervisorLaunchdSpawnType{Label: label, Loaded: true, SpawnType: "interactive"},
			agentsConfigured: true,
			wantStatus:       StatusOK,
			wantMessage:      "runs in the interactive spawn type",
		},
		{
			name:             "spawn type line absent",
			probe:            SupervisorLaunchdSpawnType{Label: label, Loaded: true},
			agentsConfigured: true,
			wantStatus:       StatusOK,
			wantMessage:      "reported no spawn type",
		},
		{
			name:             "daemon without agents",
			probe:            SupervisorLaunchdSpawnType{Label: label, Loaded: true, SpawnType: "daemon"},
			agentsConfigured: false,
			wantStatus:       StatusOK,
			wantMessage:      "no agents are configured",
		},
		{
			name:             "daemon with agents",
			probe:            SupervisorLaunchdSpawnType{Label: label, Loaded: true, SpawnType: "daemon"},
			agentsConfigured: true,
			wantStatus:       StatusWarning,
			wantMessage:      "runs in the daemon spawn type, not interactive",
			wantFixHint: []string{
				"'gc supervisor install' to regenerate the plist with ProcessType=Interactive",
				"reloads the supervisor when the plist changes",
				"'gc supervisor install --force' reloads the job",
				"launchctl print gui/$(id -u)/" + label,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewSupervisorLaunchdSpawnTypeCheck(tc.probe, tc.agentsConfigured).Run(&CheckContext{})
			if r.Status != tc.wantStatus {
				t.Fatalf("Status = %v, want %v; message=%q", r.Status, tc.wantStatus, r.Message)
			}
			if !strings.Contains(r.Message, tc.wantMessage) {
				t.Fatalf("Message = %q, want it to contain %q", r.Message, tc.wantMessage)
			}
			if tc.wantStatus == StatusWarning {
				if len(tc.wantFixHint) == 0 {
					t.Fatal("a warning case must name what its FixHint carries")
				}
				for _, want := range tc.wantFixHint {
					if !strings.Contains(r.FixHint, want) {
						t.Fatalf("FixHint = %q, want it to contain %q", r.FixHint, want)
					}
				}
			} else if r.FixHint != "" {
				t.Fatalf("FixHint = %q, want empty for status %v", r.FixHint, r.Status)
			}
		})
	}
}
