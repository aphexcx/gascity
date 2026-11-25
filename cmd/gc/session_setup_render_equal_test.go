package main

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
	rigs := []config.Rig{{Name: "alpha", Path: filepath.Join(root, "alpha"), DefaultBranch: "trunk"}}
	templates := []string{
		"git pull --rebase",
		"GC_DEFAULT_BRANCH='{{.DefaultBranch}}' {{.ConfigDir}}/assets/scripts/worktree-setup.sh {{.RigRoot}} {{.WorkDir}} {{.AgentBase}} --sync",
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
		ctx := sessionSetupContextForAgent(root, "fixture", tc.name, &tc.agent, rigs)
		ctx.WorkDir = filepath.Join(root, "work")
		ctx.ConfigDir = filepath.Join(root, "config")
		for _, spec := range templates {
			value := expandSessionSetup([]string{spec}, ctx)[0]
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
