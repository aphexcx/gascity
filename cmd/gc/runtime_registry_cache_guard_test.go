package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Walk the registrations, so a new provider needs an explicit construction
// fixture or an explained exemption before it can enter the runtime catalog.
func TestRuntimeRegistryLaunchGuard(t *testing.T) {
	r, err := runtimeRegistryForCity(&config.City{Runtimes: map[string]config.DiscoveredRuntime{
		"pack-runtime": {Name: "pack-runtime", Command: "provider-fixture", PackName: "guard-fixture"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	exempt := map[string]string{
		"fake": "in-memory test double does not execute configuration",
		"fail": "in-memory failure double does not execute configuration",
	}
	selections := map[string][]string{
		"tmux": {"tmux"}, "subprocess": {"subprocess"}, "acp": {"acp"},
		"herdr": {"herdr"}, "t3bridge": {"t3bridge"}, "k8s": {"k8s"},
		"hybrid": {"hybrid"}, "exec:": {"exec:provider-fixture"}, "ssh:": {"ssh:guard.invalid"},
		"pack-runtime": {"pack-runtime"},
	}
	for _, registration := range append(r.Names(), r.Prefixes()...) {
		t.Run(registration, func(t *testing.T) {
			if reason, ok := exempt[registration]; ok {
				t.Skip(reason)
			}
			names, ok := selections[registration]
			if !ok {
				t.Fatalf("registered provider %q has no guard fixture or exemption", registration)
			}
			for _, selection := range names {
				f := newPackCacheFixture(t, 1)
				if _, err := loadCityConfigWithoutBuiltinPackRefresh(f.city, io.Discard); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.caches[0], ".dirty"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
				t.Setenv("GC_CITY", "")
				t.Setenv("GC_CITY_PATH", "")
				backendMarker := filepath.Join(t.TempDir(), "backend-called")
				bin := filepath.Join(filepath.Dir(f.gitLog), "bin")
				for _, executable := range []string{"ssh", "tmux", "herdr", "provider-fixture"} {
					if err := os.WriteFile(filepath.Join(bin, executable), []byte(fmt.Sprintf("#!/bin/sh\nprintf called > %q\nexit 1\n", backendMarker)), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				var backendRequests atomic.Int32
				backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					backendRequests.Add(1)
					http.Error(w, "unexpected backend access", http.StatusForbidden)
				}))
				t.Cleanup(backend.Close)
				kubeconfig := filepath.Join(t.TempDir(), "config")
				if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: guard\n  cluster:\n    server: "+backend.URL+"\ncontexts:\n- name: guard\n  context:\n    cluster: guard\n    user: guard\ncurrent-context: guard\nusers:\n- name: guard\n  user: {}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("KUBECONFIG", kubeconfig)
				t.Setenv("GC_K8S_IMAGE", "guard-fixture:latest")
				t.Setenv("GC_EXEC_STATE_DIR", t.TempDir())
				p, err := r.New(selection, config.SessionConfig{RemoteMatch: "remote"}, "guard-city", f.city)
				if err != nil {
					t.Fatal(err)
				}
				cfg := runtime.Config{Command: fmt.Sprintf("printf called > %q", backendMarker), WorkDir: t.TempDir(), Env: map[string]string{"GC_STARTUP_ENVELOPE": "{"}}
				ctx := context.Background()
				check := func(operation string, err error) {
					t.Helper()
					if err == nil || !strings.Contains(err.Error(), `has local worktree changes; run "gc import install"`) {
						t.Errorf("%s did not refuse dirty import: %v", operation, err)
					}
					if _, err := os.Stat(backendMarker); !os.IsNotExist(err) {
						t.Errorf("%s touched backend before validation: %v", operation, err)
					}
					if calls := backendRequests.Load(); calls != 0 {
						t.Errorf("%s touched HTTP backend before validation: %d requests", operation, calls)
					}
				}
				for _, sessionName := range []string{"guard-local", "guard-remote"} {
					check("Start/"+sessionName, p.Start(ctx, sessionName, cfg))
					if relaunch, ok := p.(runtime.RelaunchProvider); ok {
						check("Relaunch/"+sessionName, relaunch.Relaunch(ctx, sessionName, cfg))
					}
				}
			}
		})
	}
}
