package workdir

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestExistingTemplatesRenderByteEqual(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GC_WORKTREES_DIR", filepath.Join(root, "trees"))
	rigs := []config.Rig{{Name: "alpha", Path: filepath.Join(root, "alpha"), DefaultBranch: "trunk"}}
	templates := []string{
		".gc/agents/{{.AgentBase}}",
		".gc/agents/{{.Rig}}/polecats/{{.AgentBase}}",
		".gc/agents/{{.Rig}}/refinery",
		".gc/agents/{{.Rig}}/witness",
		".gc/worktrees/{{.Rig}}/convoymaster",
		".gc/worktrees/{{.Rig}}/crew",
		".gc/worktrees/{{.Rig}}/polecats/{{.AgentBase}}",
		".gc/worktrees/{{.Rig}}/refinery",
		".gc/worktrees/{{.Rig}}/witness",
		"{{.Rig}}",
		"{{.AgentBase}}",
		"{{.CityRoot}}",
		"{{.WorktreesRoot}}",
		"{{.DefaultBranch}}",
	}
	var actual strings.Builder
	for _, tc := range []struct {
		name  string
		agent config.Agent
	}{
		{"alpha/worker", config.Agent{Name: "worker", Dir: "alpha"}},
		{"worker", config.Agent{Name: "worker"}},
		{"worker-1", config.Agent{Name: "worker-1", PoolName: "worker"}},
	} {
		for _, spec := range templates {
			agent := tc.agent
			agent.WorkDir = spec
			value, err := ResolveWorkDirPathStrict(root, "fixture", tc.name, agent, rigs)
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&actual, "%s\t%s\t%q\n", tc.name, spec, strings.ReplaceAll(value, root, "CITY"))
		}
	}
	want, err := os.ReadFile("testdata/existing_templates.golden")
	if err != nil {
		t.Fatal(err)
	}
	if actual.String() != string(want) {
		t.Fatalf("template renderings differ from base golden:\n%s", actual.String())
	}
}
