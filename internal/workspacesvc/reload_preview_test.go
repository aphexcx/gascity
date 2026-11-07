package workspacesvc

import (
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/supervisor"
)

func TestServiceReloadChangesNamesDefinitionChangesWithoutRestarting(t *testing.T) {
	old := config.Service{Name: "adapter", Kind: "proxy_process", SourceDir: "/packs/old", Process: config.ServiceProcessConfig{Command: []string{"old-command"}}}
	inst := &testInstance{}
	entries := map[string]*entry{
		"adapter": {spec: old, inst: inst},
		"removed": {spec: config.Service{Name: "removed"}, inst: &testInstance{}},
	}
	next := old
	next.SourceDir = "/packs/new"
	next.Process.Command = []string{"new-command"}
	got := serviceReloadChanges(entries, &config.City{Services: []config.Service{next}}, supervisor.PublicationConfig{}, publicationRefs{})
	want := []ReloadChange{
		{Name: "adapter", Action: "restart", Reasons: []string{"pack source changed", "command changed"}},
		{Name: "removed", Action: "stop", Reasons: []string{"service removed"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes=%+v, want %+v", got, want)
	}
	if inst.closed || !reflect.DeepEqual(entries["adapter"].spec, old) {
		t.Fatal("preview modified the running service")
	}
}

func TestServiceReloadChangesUnchangedAndPublicationEnvironment(t *testing.T) {
	spec := config.Service{Name: "adapter", Kind: "proxy_process"}
	cfg := &config.City{Services: []config.Service{spec}}
	status := baseStatus(cfg, supervisor.PublicationConfig{}, publicationRefs{}, spec, time.Time{})
	entries := map[string]*entry{"adapter": {spec: spec, status: status, inst: &testInstance{}}}
	if got := serviceReloadChanges(entries, cfg, supervisor.PublicationConfig{}, publicationRefs{}); len(got) != 0 {
		t.Fatalf("unchanged service=%+v", got)
	}
	entries["adapter"].status.URL = "https://old.example"
	got := serviceReloadChanges(entries, cfg, supervisor.PublicationConfig{}, publicationRefs{})
	if len(got) != 1 || got[0].Action != "restart" || !reflect.DeepEqual(got[0].Reasons, []string{"publication env changed"}) {
		t.Fatalf("publication environment changes=%+v", got)
	}
}
