package doctor

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
)

type orphanMetadataProvider struct {
	runtime.Provider
	metaErr error
}

type orphanScopedProvider struct {
	orphanMetadataProvider
	tmux.Config
}

// Keep sessions in memory and use tmux's pure configuration scope check.
func orphanProvider(sp runtime.Provider, socket string, metaErr error) orphanScopedProvider {
	return orphanScopedProvider{orphanMetadataProvider{sp, metaErr}, tmux.Config{SocketName: socket}}
}

func (p orphanMetadataProvider) GetMeta(name, key string) (string, error) {
	if p.metaErr != nil {
		return "", p.metaErr
	}
	if !p.IsRunning(name) {
		return "", runtime.ErrSessionNotFound
	}
	return p.Provider.GetMeta(name, key)
}

func TestOrphanSessionsCheckCompositeScopedLeaf(t *testing.T) {
	for _, backend := range []string{"auto", "hybrid"} {
		t.Run(backend, func(t *testing.T) {
			local := runtime.NewFake()
			if err := local.Start(context.Background(), "test-stale", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			leaf := orphanProvider(local, "test", nil)
			var sp runtime.Provider = auto.New(leaf, runtime.NewFake())
			if backend == "hybrid" {
				sp = hybrid.New(leaf, runtime.NewFake(), func(string) bool { return false })
			}
			c := NewOrphanSessionsCheck(&config.City{Agents: []config.Agent{{Name: "worker"}}}, "test", "{{.City}}-{{.Agent}}", sp)
			c.AllowFix = true
			if err := c.Fix(&CheckContext{}); err != nil {
				t.Fatal(err)
			}
			if local.IsRunning("test-stale") {
				t.Fatal("cleanup did not reach the scoped owning leaf")
			}
		})
	}
}

func TestOrphanSessionsCheckRecordedRuntimeName(t *testing.T) {
	for _, tt := range []struct {
		name, identity, key, city string
		want                      int
	}{
		{"s-ci-123", "s", "GC_AGENT", "/cities/test", 1},
		{"s-ci-123", "s-ci-123", "GC_AGENT", "/cities/test", 1},
		{"s-ci-123", "s-ci-123", "GC_TEMPLATE", "/cities/test", 1},
		{"runtime.name-ci-123", "runtime.name", "GC_AGENT", "/cities/test", 1},
		{"s-ci-123", "s-ci-123", "GC_AGENT", "/cities/other", 0},
		{"s-ci-123", "s-ci-123", "GC_AGENT", "", 0},
	} {
		t.Run(tt.name+"/"+tt.identity+"/"+tt.key+"/"+tt.city, func(t *testing.T) {
			sp := runtime.NewFake()
			if err := sp.Start(context.Background(), tt.name, runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			for key, value := range map[string]string{"GC_CITY_PATH": tt.city, tt.key: tt.identity, "GC_SESSION_ID": "ci-123"} {
				if err := sp.SetMeta(tt.name, key, value); err != nil {
					t.Fatal(err)
				}
			}
			c := NewOrphanSessionsCheck(&config.City{Agents: []config.Agent{{Name: "worker"}}}, "test", "{{.City}}-{{.Agent}}", sp)
			r := c.Run(&CheckContext{CityPath: "/cities/test"})
			if len(r.Details) != tt.want {
				t.Fatalf("Run() = %+v, want %d candidates", r, tt.want)
			}
			if tt.want > 0 && !strings.Contains(r.Details[0], tt.name+": GC_CITY_PATH matches this city") {
				t.Errorf("Details = %v, want recorded identity with city ownership", r.Details)
			}
		})
	}
}

func TestOrphanSessionsCheckMixedBackends(t *testing.T) {
	for _, tt := range []struct {
		name   string
		err    error
		nested bool
		batch  bool
	}{
		{name: "unrouted ACP"},
		{name: "unreadable ACP", err: context.DeadlineExceeded},
		{name: "vanished ACP", err: runtime.ErrSessionNotFound},
		{name: "nested hybrid", nested: true},
		{name: "unreadable ACP batch", err: context.DeadlineExceeded, batch: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			local, acp := runtime.NewFake(), runtime.NewFake()
			for name, sp := range map[string]*runtime.Fake{"test-local-stale": local, "test-acp-stale": acp} {
				if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
					t.Fatal(err)
				}
			}
			var acpBackend runtime.Provider = orphanMetadataProvider{Provider: acp, metaErr: tt.err}
			if tt.batch {
				acpBackend = failedBatchOrphanProvider{orphanProvider(acp, "", nil)}
			}
			if tt.nested {
				acpBackend = hybrid.New(runtime.NewFake(), acpBackend, func(string) bool { return false })
			}
			// The ACP session is deliberately absent from both routing tables.
			sp := auto.New(orphanProvider(local, "test", nil), acpBackend)
			c := NewOrphanSessionsCheck(&config.City{Agents: []config.Agent{{Name: "worker"}}}, "test", "{{.City}}-{{.Agent}}", sp)
			c.AllowFix = true
			ctx := &CheckContext{}
			r := c.Run(ctx)
			details := strings.Join(r.Details, "\n")
			if r.Status != StatusWarning || len(r.Details) != 2 || !strings.Contains(details, "test-local-stale:") || !strings.Contains(details, "test-acp-stale") {
				t.Fatalf("Run() = %+v, want both sessions reported without aborting", r)
			}
			if tt.err != nil {
				if !strings.Contains(details, tt.err.Error()) || !strings.Contains(details, "excluded from cleanup candidates") {
					t.Errorf("Details = %q, want the ownership error and cleanup exclusion", details)
				}
				var d Doctor
				d.Register(c)
				report := d.Run(ctx, io.Discard, false)
				if report.Failed != 0 || report.BlockingFailed != 0 || report.Warned != 1 {
					t.Errorf("plain doctor report = %+v, want one warning and no failures", report)
				}
				if err := c.Fix(ctx); !errors.Is(err, tt.err) {
					t.Errorf("Fix() = %v, want unreadable ownership refusal: %v", err, tt.err)
				}
				if local.CountCalls("Stop", "test-local-stale") != 0 || acp.CountCalls("Stop", "test-acp-stale") != 0 {
					t.Fatal("partial metadata view stopped a session")
				}
				return
			}
			if !strings.Contains(details, "test-acp-stale: runtime cannot confirm a city-scoped socket") {
				t.Errorf("Details = %q, want the unscoped candidate's cleanup refusal", details)
			}
			if err := c.Fix(ctx); err == nil || !strings.Contains(err.Error(), "test-acp-stale") {
				t.Errorf("Fix() = %v, want the unscoped candidate named", err)
			}
			if local.IsRunning("test-local-stale") || !acp.IsRunning("test-acp-stale") {
				t.Fatal("cleanup must stop the scoped leaf's orphan and retain the unscoped leaf's orphan")
			}
		})
	}
}

type failedBatchOrphanProvider struct{ orphanScopedProvider }

func (failedBatchOrphanProvider) GetAllEnvironment(string) (map[string]string, error) {
	return nil, context.DeadlineExceeded
}

func TestOrphanSessionsCheckBatchOwnershipErrors(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "test-stale", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	p := failedBatchOrphanProvider{orphanProvider(sp, "test", nil)}
	c := NewOrphanSessionsCheck(&config.City{Agents: []config.Agent{{Name: "worker"}}}, "test", "{{.City}}-{{.Agent}}", p)
	c.AllowFix = true
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning || len(r.Details) != 1 || !strings.Contains(r.Details[0], "excluded from cleanup candidates") {
		t.Fatalf("Run() = %+v, want a warning when the only session is unreadable", r)
	}
	if err := c.Fix(&CheckContext{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Fix() = %v, want ownership-read timeout", err)
	}
	if !sp.IsRunning("test-stale") {
		t.Fatal("cleanup used the name after an ownership-read failure")
	}
}

func TestOrphanSessionsCheckSameNameAcrossBackends(t *testing.T) {
	local, acp := runtime.NewFake(), runtime.NewFake()
	for sp, city := range map[*runtime.Fake]string{local: "/cities/other", acp: "/cities/test"} {
		if err := sp.Start(context.Background(), "test-stale", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		if err := sp.SetMeta("test-stale", "GC_CITY_PATH", city); err != nil {
			t.Fatal(err)
		}
	}
	sp := auto.New(local, orphanProvider(acp, "test", nil))
	c := NewOrphanSessionsCheck(&config.City{Agents: []config.Agent{{Name: "worker"}}}, "test", "{{.City}}-{{.Agent}}", sp)
	c.AllowFix = true
	ctx := &CheckContext{CityPath: "/cities/test"}
	r := c.Run(ctx)
	if r.Status != StatusWarning || len(r.Details) != 1 {
		t.Fatalf("Run() = %+v, want only the owned backend's session", r)
	}
	if err := c.Fix(ctx); err != nil {
		t.Fatal(err)
	}
	if !local.IsRunning("test-stale") || acp.IsRunning("test-stale") {
		t.Fatal("cleanup stopped the foreign backend's session or retained the owned orphan")
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
	c := NewOrphanSessionsCheck(&config.City{Agents: []config.Agent{{Name: "worker", Dir: "beta"}}}, "test", "", orphanProvider(sp, "test", nil))
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
	c := NewOrphanSessionsCheck(&config.City{Agents: []config.Agent{{Name: "worker"}, {Name: "core.worker", Dir: "repo"}}}, "test", "{{.City}}-{{.Agent}}", orphanProvider(sp, "test", nil))
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
	c := NewOrphanSessionsCheck(&config.City{Agents: []config.Agent{{Name: "worker"}}}, "test", "", orphanProvider(sp, "test", nil))
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
			socket := "default"
			if tt.scoped {
				socket = "test"
			}
			c := NewOrphanSessionsCheck(tt.cfg, "test", tt.template, orphanProvider(sp, socket, tt.metaErr))
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
