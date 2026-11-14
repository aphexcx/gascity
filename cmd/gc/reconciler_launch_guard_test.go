package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// This guard is run unchanged at the original base and at the candidate head.
// Normalize the temporary city and test executable paths before comparing launch
// and fingerprint inputs; every environment key and value remains in the digest.
func TestReconcilerLaunchBaseGuard(t *testing.T) {
	cityPath := t.TempDir()
	for key := range passthroughEnv() {
		t.Setenv(key, "")
	}
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", filepath.Join(cityPath, "home"))
	t.Setenv("GC_HOME", filepath.Join(cityPath, "gc-home"))
	t.Setenv("USER", "fixture")
	t.Setenv("LOGNAME", "fixture")
	t.Setenv("LANG", "C")
	t.Setenv("GC_TEST_AMBIENT_SWEEP", "ambient")
	writePhase0InterfaceCity(t, cityPath, `[workspace]
name = "launch-city"
[beads]
provider = "file"
[agent_defaults]
upstream = "u"
[upstreams.u]
base_url = "https://model.example.invalid"
auth_token = "invented-test-token"
base_url_env = "TEST_MODEL_BASE_URL"
auth_token_env = "TEST_MODEL_AUTH_TOKEN"
[[agent]]
name = "runner"
provider = "test-launch"
[agent.env]
TEST_AGENT_ENV = "agent"
[providers.test-launch]
command = "/bin/echo"
args = ["--model", "example-model"]
prompt_mode = "none"
[providers.test-launch.env]
TEST_PROVIDER_ENV = "provider"
`)
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	info := session.Info{ID: "test-session", Template: "runner", SessionName: "runner", SessionNameMetadata: "runner", SessionOrigin: "named", ConfiguredNamedSession: true}
	params := newAgentBuildParams(cfg.EffectiveCityName(), cityPath, cfg, runtime.NewFake(), time.Unix(0, 0).UTC(), beads.NewMemStore(), io.Discard)
	tp, err := resolveTemplateForSessionBeadInfo(params, &cfg.Agents[0], "runner", nil, info)
	if err != nil {
		t.Fatal(err)
	}
	launch, _, err := templateParamsToConfigWithDelivery(tp)
	if err != nil {
		t.Fatal(err)
	}
	if launch.Env["GC_TEST_AMBIENT_SWEEP"] != "ambient" || launch.Env["TEST_MODEL_AUTH_TOKEN"] != "invented-test-token" {
		t.Fatal("fixture lost ambient or upstream inputs")
	}
	if _, exists := tp.Env["GC_STARTUP_PROMPT_DELIVERED"]; exists {
		t.Fatal("template resolution gained a startup-prompt environment key")
	}
	normalize := func(value any) []byte {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		physical, err := filepath.EvalSymlinks(cityPath)
		if err != nil {
			t.Fatal(err)
		}
		text = strings.ReplaceAll(text, physical, "<city>")
		text = strings.ReplaceAll(text, cityPath, "<city>")
		text = strings.ReplaceAll(text, executable, "<test-binary>")
		return []byte(strings.ReplaceAll(text, filepath.Dir(executable), "<test-bin-dir>"))
	}
	launchBytes := normalize(launch)
	fingerprintInputs := normalize(sessionCoreConfigForHashInfo(tp, info))
	t.Logf("launch_sha256=%x fingerprint_inputs_sha256=%x", sha256.Sum256(launchBytes), sha256.Sum256(fingerprintInputs))
	// The base launch with its fixture timestamp in UTC. The startup nudge
	// includes that timestamp, so the fixture must not inherit time.Local.
	const wantDigest = "a1498e8e8d01b4511974d6b044f750948033f7faafef55431cb6a573bdde847d"
	for name, data := range map[string][]byte{"launch": launchBytes, "fingerprint inputs": fingerprintInputs} {
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != wantDigest {
			t.Errorf("%s changed from base: digest=%s, want %s", name, got, wantDigest)
		}
	}
	var fingerprintConfig runtime.Config
	if err := json.Unmarshal(fingerprintInputs, &fingerprintConfig); err != nil {
		t.Fatal(err)
	}
	t.Logf("core=%s provision=%s launch=%s live=%s", runtime.CoreFingerprint(fingerprintConfig), runtime.ProvisionFingerprint(fingerprintConfig), runtime.LaunchFingerprint(fingerprintConfig), runtime.LiveFingerprint(fingerprintConfig))
}
