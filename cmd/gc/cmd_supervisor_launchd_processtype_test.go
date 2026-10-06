package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// The launchd tests below never reach the real launchd or the real
// ~/Library/LaunchAgents: HOME and GC_HOME point at temporary directories and
// supervisorLaunchctlRun / supervisorLaunchdLoaded are stubbed (hq-a6cny).

const launchdProcessTypeLines = "    <key>ProcessType</key>\n    <string>Interactive</string>\n"

// launchdPlistTopLevelStrings decodes a rendered plist strictly and returns
// the string values of its top-level dict by key. A decode error fails the
// test, so every caller also proves the plist is well formed.
func launchdPlistTopLevelStrings(t *testing.T, content string) map[string][]string {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(content))
	dec.Strict = true
	out := map[string][]string{}
	var stack []string
	pendingKey := ""
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("rendered plist is not well formed: %v\n%s", err, content)
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			topLevel := len(stack) == 2 && stack[0] == "plist" && stack[1] == "dict"
			if !topLevel || (tok.Name.Local != "key" && tok.Name.Local != "string") {
				if topLevel {
					pendingKey = ""
				}
				stack = append(stack, tok.Name.Local)
				continue
			}
			var text string
			if err := dec.DecodeElement(&text, &tok); err != nil {
				t.Fatalf("decoding <%s> in plist: %v\n%s", tok.Name.Local, err, content)
			}
			if tok.Name.Local == "key" {
				pendingKey = text
				continue
			}
			if pendingKey != "" {
				out[pendingKey] = append(out[pendingKey], text)
			}
			pendingKey = ""
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		}
	}
	if len(stack) != 0 {
		t.Fatalf("rendered plist ends with unclosed elements %v\n%s", stack, content)
	}
	return out
}

func requireLaunchdInteractiveProcessType(t *testing.T, content string) {
	t.Helper()
	got := launchdPlistTopLevelStrings(t, content)["ProcessType"]
	if len(got) != 1 || got[0] != "Interactive" {
		t.Fatalf("top-level ProcessType = %q, want exactly one \"Interactive\"\n%s", got, content)
	}
}

// lintLaunchdPlistWithPlutil runs Apple's own parser over the rendered plist
// when it is available (macOS): plutil -lint, then plutil -extract to read
// ProcessType back out.
func lintLaunchdPlistWithPlutil(t *testing.T, content string) {
	t.Helper()
	if goruntime.GOOS != "darwin" {
		return
	}
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		return
	}
	path := filepath.Join(t.TempDir(), "supervisor.plist")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(plutil, "-lint", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint: %v: %s\n%s", err, out, content)
	}
	out, err := exec.Command(plutil, "-extract", "ProcessType", "raw", "-o", "-", path).CombinedOutput()
	if err != nil {
		t.Fatalf("plutil -extract ProcessType: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "Interactive" {
		t.Fatalf("plutil -extract ProcessType = %q, want Interactive", got)
	}
}

func TestRenderSupervisorLaunchdTemplateSetsInteractiveProcessType(t *testing.T) {
	content, err := renderSupervisorTemplate(supervisorLaunchdTemplate, &supervisorServiceData{
		GCPath:        "/usr/local/bin/gc",
		LogPath:       "/home/user/.gc/supervisor.log",
		GCHome:        "/home/user/.gc",
		XDGRuntimeDir: "/tmp/gc-run",
		LaunchdLabel:  defaultSupervisorLaunchdLabel,
		Path:          "/usr/local/bin:/usr/bin:/bin",
		ExtraEnv: []supervisorServiceEnvVar{
			{Name: "ANTHROPIC_API_KEY", Value: `sk-&<"'>`},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, launchdProcessTypeLines) {
		t.Fatalf("launchd template missing %q:\n%s", launchdProcessTypeLines, content)
	}
	requireLaunchdInteractiveProcessType(t, content)
	lintLaunchdPlistWithPlutil(t, content)
}

// TestInstallSupervisorLaunchdRegenerationKeepsProcessType covers the path
// that used to drop a hand-applied key: gc start and gc supervisor start
// regenerate the plist from the template (ensureSupervisorRunning ->
// doSupervisorInstall -> installSupervisorLaunchd). Whatever plist is on disk
// beforehand, the regenerated one carries ProcessType=Interactive, and a
// second regeneration keeps it.
func TestInstallSupervisorLaunchdRegenerationKeepsProcessType(t *testing.T) {
	cases := []struct {
		name string
		// seed returns the plist on disk before the install, given what the
		// template renders today; "" means no plist.
		seed func(rendered string) string
	}{
		{name: "fresh install", seed: func(string) string { return "" }},
		{name: "upgrade from a plist without the key", seed: func(rendered string) string {
			return strings.Replace(rendered, launchdProcessTypeLines, "", 1)
		}},
		{name: "plist with the key hand-appended", seed: func(rendered string) string {
			keyless := strings.Replace(rendered, launchdProcessTypeLines, "", 1)
			end := strings.LastIndex(keyless, "</dict>")
			return keyless[:end] + "\t<key>ProcessType</key>\n\t<string>Interactive</string>\n" + keyless[end:]
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			homeDir := t.TempDir()
			gcHome := filepath.Join(t.TempDir(), "isolated-home")
			t.Setenv("HOME", homeDir)
			t.Setenv("GC_HOME", gcHome)

			data := &supervisorServiceData{
				GCPath:       "/tmp/gc-same",
				LogPath:      filepath.Join(gcHome, "supervisor.log"),
				GCHome:       gcHome,
				LaunchdLabel: supervisorLaunchdLabel(),
				Path:         "/usr/local/bin:/usr/bin:/bin",
			}
			rendered, err := renderSupervisorTemplate(supervisorLaunchdTemplate, data)
			if err != nil {
				t.Fatal(err)
			}
			path := supervisorLaunchdPlistPath()
			if !strings.HasPrefix(path, homeDir+string(filepath.Separator)) {
				t.Fatalf("plist path %q is outside the temporary HOME %q", path, homeDir)
			}
			if seed := tc.seed(rendered); seed != "" {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			oldRun, oldAlive := supervisorLaunchctlRun, supervisorAliveHook
			var calls []string
			supervisorLaunchctlRun = func(args ...string) error {
				calls = append(calls, strings.Join(args, " "))
				return nil
			}
			supervisorAliveHook = func() int { return 0 }
			t.Cleanup(func() {
				supervisorLaunchctlRun, supervisorAliveHook = oldRun, oldAlive
			})

			readPlist := func() string {
				t.Helper()
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("ReadFile(%q): %v", path, err)
				}
				return string(got)
			}

			var stdout, stderr bytes.Buffer
			if code := installSupervisorLaunchd(data, &stdout, &stderr); code != 0 {
				t.Fatalf("installSupervisorLaunchd code = %d, want 0; stderr=%q", code, stderr.String())
			}
			first := readPlist()
			if first != rendered {
				t.Fatalf("installed plist differs from the rendered template\n--- got ---\n%s\n--- want ---\n%s", first, rendered)
			}
			requireLaunchdInteractiveProcessType(t, first)
			lintLaunchdPlistWithPlutil(t, first)
			if len(calls) == 0 {
				t.Fatal("launchctl was not called; want the regenerated plist reloaded")
			}

			// A later regeneration, e.g. after an upgrade changes PATH, still
			// renders the key.
			data.Path = "/opt/homebrew/bin:/usr/bin:/bin"
			supervisorAliveHook = func() int { return 4242 }
			if code := installSupervisorLaunchd(data, &stdout, &stderr); code != 0 {
				t.Fatalf("second installSupervisorLaunchd code = %d, want 0; stderr=%q", code, stderr.String())
			}
			second := readPlist()
			if !strings.Contains(second, "<string>/opt/homebrew/bin:/usr/bin:/bin</string>") {
				t.Fatalf("second regeneration did not rewrite PATH:\n%s", second)
			}
			requireLaunchdInteractiveProcessType(t, second)
		})
	}
}

func TestLaunchdPrintSpawnType(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
	}{
		{name: "interactive", out: "gui/501/com.gascity.supervisor = {\n\tactive count = 1\n\tstate = running\n\tspawn type = interactive (4)\n}\n", want: "interactive"},
		{name: "daemon", out: "gui/501/com.gascity.supervisor = {\n\tstate = running\n\tspawn type = daemon (3)\n}\n", want: "daemon"},
		{name: "absent", out: "gui/501/com.gascity.supervisor = {\n\tstate = running\n}\n", want: ""},
		{name: "empty", out: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := launchdPrintSpawnType([]byte(tc.out)); got != tc.want {
				t.Fatalf("launchdPrintSpawnType = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProbeSupervisorLaunchdSpawnType(t *testing.T) {
	old := supervisorLaunchdLoaded
	t.Cleanup(func() { supervisorLaunchdLoaded = old })

	supervisorLaunchdLoaded = func(label string) (bool, bool, string) {
		return true, false, label + " = {\n\tspawn type = daemon (3)\n}"
	}
	got := probeSupervisorLaunchdSpawnType("com.gascity.supervisor")
	want := doctor.SupervisorLaunchdSpawnType{Label: "com.gascity.supervisor", Loaded: true, SpawnType: "daemon"}
	if got != want {
		t.Fatalf("probe = %+v, want %+v", got, want)
	}

	supervisorLaunchdLoaded = func(string) (bool, bool, string) {
		return false, true, "Could not find service"
	}
	got = probeSupervisorLaunchdSpawnType("com.gascity.supervisor")
	want = doctor.SupervisorLaunchdSpawnType{Label: "com.gascity.supervisor"}
	if got != want {
		t.Fatalf("probe for an absent job = %+v, want %+v", got, want)
	}
}

func TestBuildDoctorChecksRegistersLaunchdSpawnTypeCheckWithProbe(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_DOLT", "skip")
	daemon := &doctor.SupervisorLaunchdSpawnType{Label: defaultSupervisorLaunchdLabel, Loaded: true, SpawnType: "daemon"}

	build := func(cfg *config.City, probe *doctor.SupervisorLaunchdSpawnType) []doctor.Check {
		return buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
			SkipCityDoltCheck:    true,
			SkipManagedDoltCheck: true,
			LaunchdSpawnType:     probe,
		})
	}
	find := func(checks []doctor.Check) doctor.Check {
		for _, c := range checks {
			if c.Name() == "supervisor-launchd-spawn-type" {
				return c
			}
		}
		return nil
	}

	noAgents := &config.City{Workspace: config.Workspace{Name: "demo"}}
	if c := find(build(noAgents, nil)); c != nil {
		t.Fatal("supervisor-launchd-spawn-type registered without a probe (off macOS)")
	}

	names := doctorCheckNames(build(noAgents, daemon))
	ownership := doctorCheckIndex(names, "supervisor-unit-ownership")
	if got := doctorCheckIndex(names, "supervisor-launchd-spawn-type"); ownership < 0 || got != ownership+1 {
		t.Fatalf("supervisor-launchd-spawn-type index = %d, want right after supervisor-unit-ownership at %d; names=%v", got, ownership, names)
	}
	if r := find(build(noAgents, daemon)).Run(&doctor.CheckContext{CityPath: cityDir}); r.Status != doctor.StatusOK {
		t.Fatalf("daemon spawn type with no agents: status = %v, want OK; message=%q", r.Status, r.Message)
	}

	withAgents := &config.City{Workspace: config.Workspace{Name: "demo"}, Agents: []config.Agent{{Name: "worker"}}}
	if r := find(build(withAgents, daemon)).Run(&doctor.CheckContext{CityPath: cityDir}); r.Status != doctor.StatusWarning {
		t.Fatalf("daemon spawn type with agents: status = %v, want warning; message=%q", r.Status, r.Message)
	}
}
