package doctor

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

type scopedOrphanProvider struct {
	runtime.Provider
	scoped  bool
	metaErr error
}

func (p scopedOrphanProvider) IsCityScoped(string) bool { return p.scoped }

func (p scopedOrphanProvider) GetMeta(name, key string) (string, error) {
	if p.metaErr != nil {
		return "", p.metaErr
	}
	return p.Provider.GetMeta(name, key)
}

type failedBatchOrphanProvider struct{ scopedOrphanProvider }

func (failedBatchOrphanProvider) GetAllEnvironment(string) (map[string]string, error) {
	return nil, context.DeadlineExceeded
}

func TestOrphanSessionsCheckBatchOwnershipErrors(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "test-stale", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	p := failedBatchOrphanProvider{scopedOrphanProvider{Provider: sp, scoped: true}}
	c := NewOrphanSessionsCheck(&config.City{Agents: []config.Agent{{Name: "worker"}}}, "test", "{{.City}}-{{.Agent}}", p)
	c.AllowFix = true
	if err := c.Fix(&CheckContext{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Fix() = %v, want ownership-read timeout", err)
	}
	if !sp.IsRunning("test-stale") {
		t.Fatal("cleanup used the name after an ownership-read failure")
	}
}

func TestOrphanSessionsCheckRemovedRigTemplate(t *testing.T) {
	sp := runtime.NewFake()
	for _, rig := range []string{"alpha", "beta"} {
		id := "ci-wisp-" + rig
		name := "worker-" + id
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		for key, value := range map[string]string{"GC_CITY_PATH": "/cities/test", "GC_TEMPLATE": rig + "/worker", "GC_SESSION_ID": id} {
			if err := sp.SetMeta(name, key, value); err != nil {
				t.Fatal(err)
			}
		}
	}
	c := NewOrphanSessionsCheck(&config.City{Agents: []config.Agent{{Name: "worker", Dir: "beta"}}}, "test", "", scopedOrphanProvider{Provider: sp, scoped: true})
	c.AllowFix = true
	ctx := &CheckContext{CityPath: "/cities/test"}
	r := c.Run(ctx)
	if len(r.Details) != 1 || !strings.Contains(r.Details[0], "worker-ci-wisp-alpha") {
		t.Fatalf("Run() = %+v, want the removed rig's session", r)
	}
	if err := c.Fix(ctx); err != nil {
		t.Fatal(err)
	}
	if sp.IsRunning("worker-ci-wisp-alpha") || !sp.IsRunning("worker-ci-wisp-beta") {
		t.Fatal("cleanup did not distinguish the recorded qualified templates")
	}
}

func TestOrphanSessionsCheckCityOwnership(t *testing.T) {
	sp := runtime.NewFake()
	for _, name := range []string{
		"test-worker", "test-worker-ci-wisp-abc123", "test-stale",
		"other-worker", "other-stale", "personal-shell",
	} {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	c := NewOrphanSessionsCheck(&config.City{Agents: []config.Agent{{Name: "worker"}}}, "test", "{{.City}}-{{.Agent}}", sp)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning || len(r.Details) != 1 || !strings.Contains(r.Details[0], "test-stale") || !strings.Contains(r.Details[0], "not configured") {
		t.Fatalf("Run() = %+v, want only test-stale with an explanation", r)
	}
}

func TestOrphanSessionsCheckRequiresOptIn(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "test-stale", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	c := NewOrphanSessionsCheck(&config.City{Agents: []config.Agent{{Name: "worker"}}}, "test", "{{.City}}-{{.Agent}}", sp)
	if err := c.Fix(&CheckContext{}); err == nil || !strings.Contains(err.Error(), "--fix-orphans") {
		t.Errorf("Fix() = %v, want explicit opt-in refusal", err)
	}
	if !sp.IsRunning("test-stale") {
		t.Fatal("cleanup without opt-in stopped a session")
	}
	var d Doctor
	d.Register(c)
	report := d.Run(&CheckContext{}, io.Discard, true)
	if report.Warned != 1 || report.Fixed != 0 || !sp.IsRunning("test-stale") {
		t.Fatalf("ordinary --fix changed the orphan: %+v", report)
	}
}

func TestOrphanSessionsCheckOptInStopsOnlyOwnedOrphans(t *testing.T) {
	sp := runtime.NewFake()
	names := []string{"test-worker", "test-worker-ci-wisp-abc123", "core__worker-ci-wisp-xyz", "test-stale", "other-stale", "shell", "test-foreign"}
	for _, name := range names {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sp.SetMeta("test-foreign", "GC_CITY_PATH", "/cities/other"); err != nil {
		t.Fatal(err)
	}
	c := NewOrphanSessionsCheck(&config.City{Agents: []config.Agent{{Name: "worker"}, {Name: "core.worker", Dir: "repo"}}}, "test", "{{.City}}-{{.Agent}}", scopedOrphanProvider{Provider: sp, scoped: true})
	c.AllowFix = true
	if err := c.Fix(&CheckContext{CityPath: "/cities/test"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if got, want := sp.IsRunning(name), name != "test-stale"; got != want {
			t.Errorf("IsRunning(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestOrphanSessionsCheckAgentOnlyNamesNeedOwnership(t *testing.T) {
	sp := runtime.NewFake()
	for _, name := range []string{"worker", "worker-ci-wisp-abc123", "stale", "retired-ci-wisp-abc123", "foreign", "shell"} {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	for name, city := range map[string]string{"stale": "/cities/test", "retired-ci-wisp-abc123": "/cities/test", "foreign": "/cities/other", "shell": "/cities/test"} {
		if err := sp.SetMeta(name, "GC_CITY_PATH", city); err != nil {
			t.Fatal(err)
		}
	}
	for name, identity := range map[string]string{"stale": "stale", "shell": "worker"} {
		if err := sp.SetMeta(name, "GC_AGENT", identity); err != nil {
			t.Fatal(err)
		}
	}
	for key, value := range map[string]string{"GC_TEMPLATE": "repo/retired", "GC_SESSION_ID": "ci-wisp-abc123"} {
		if err := sp.SetMeta("retired-ci-wisp-abc123", key, value); err != nil {
			t.Fatal(err)
		}
	}
	c := NewOrphanSessionsCheck(&config.City{Agents: []config.Agent{{Name: "worker"}}}, "test", "", scopedOrphanProvider{Provider: sp, scoped: true})
	c.AllowFix = true
	ctx := &CheckContext{CityPath: "/cities/test"}
	r := c.Run(ctx)
	details := strings.Join(r.Details, "\n")
	if len(r.Details) != 2 || !strings.Contains(details, "stale: GC_CITY_PATH") || !strings.Contains(details, "retired-ci-wisp-abc123: GC_CITY_PATH") {
		t.Fatalf("Run() = %+v, want only the owned stale sessions", r)
	}
	if err := c.Fix(ctx); err != nil {
		t.Fatal(err)
	}
	if sp.IsRunning("stale") || sp.IsRunning("retired-ci-wisp-abc123") || !sp.IsRunning("foreign") || !sp.IsRunning("shell") || !sp.IsRunning("worker-ci-wisp-abc123") {
		t.Fatal("Fix stopped a protected session or retained the orphan")
	}
}

func TestOrphanSessionsCheckRefusesUnsafeFix(t *testing.T) {
	configured := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	for _, tt := range []struct {
		name     string
		cfg      *config.City
		template string
		scoped   bool
		metaErr  error
		want     string
	}{
		{"nil config", nil, "", true, nil, "no agents"},
		{"zero agents", &config.City{}, "", true, nil, "no agents"},
		{"empty expected", configured, " ", true, nil, "empty or partial"},
		{"partial expected", &config.City{Agents: []config.Agent{{Name: "worker"}, {}}}, "", true, nil, "empty or partial"},
		{"ambiguous expected", &config.City{Agents: []config.Agent{{Name: "worker"}, {Name: "reviewer"}}}, "{{.City}}", true, nil, "ambiguous expected"},
		{"shared socket", configured, "{{.City}}-{{.Agent}}", false, nil, "city-scoped socket"},
		{"ownership error", configured, "{{.City}}-{{.Agent}}", true, errors.New("metadata unavailable"), "metadata unavailable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sp := runtime.NewFake()
			if err := sp.Start(context.Background(), "test-stale", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			c := NewOrphanSessionsCheck(tt.cfg, "test", tt.template, scopedOrphanProvider{Provider: sp, scoped: tt.scoped, metaErr: tt.metaErr})
			c.AllowFix = true
			if err := c.Fix(&CheckContext{}); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Fix() = %v, want %q", err, tt.want)
			}
			if !sp.IsRunning("test-stale") {
				t.Fatal("unsafe cleanup stopped a session")
			}
		})
	}
}

func TestOrphanSessionsCheckTemplateSuffix(t *testing.T) {
	sp := runtime.NewFake()
	for _, name := range []string{"test-worker-terminal", "test-stale-terminal", "test-unrelated-shell"} {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	c := NewOrphanSessionsCheck(&config.City{Agents: []config.Agent{{Name: "worker"}}}, "test", "{{.City}}-{{.Agent}}-terminal", sp)
	r := c.Run(&CheckContext{})
	if len(r.Details) != 1 || !strings.Contains(r.Details[0], "test-stale-terminal") {
		t.Fatalf("Run() = %+v, want only the session matching the complete city naming shape", r)
	}
}
