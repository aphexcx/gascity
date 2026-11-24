package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/supervisor"
)

const bdCaptureFile = "GC_TEST_BD_CAPTURE_FILE"

// The test binary doubles as bd so the capture sees the exact exec arguments
// and environment, before a shell or TestMain changes either.
func maybeCaptureBdInvocation() {
	if filepath.Base(os.Args[0]) == "gc-bd-invocation-test" {
		bdBeadExists = fixtureBeadExists
		os.Exit(doBd(os.Args[1:], os.Stdout, os.Stderr))
	}
	dest := os.Getenv(bdCaptureFile)
	if dest == "" || filepath.Base(os.Args[0]) != "bd" {
		return
	}
	cwd, err := os.Getwd()
	if err == nil {
		// Coverage belongs to the test harness, including in re-executed children.
		env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
			return strings.HasPrefix(kv, "GOCOVERDIR=")
		})
		slices.Sort(env)
		var data []byte
		data, err = json.MarshalIndent(struct {
			Cwd       string
			Args, Env []string
		}{cwd, os.Args, env}, "", "  ")
		if err == nil {
			err = os.WriteFile(dest, append(data, '\n'), 0o600)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(98)
	}
	if _, err := fmt.Fprintln(os.Stdout, "fake bd"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(98)
	}
	fmt.Fprintln(os.Stderr, "fake bd diagnostic")
	os.Exit(0)
}

type bdInvocationFixture struct {
	root, city, rig, capture, executable string
	args                                 []string
}

func fixtureBeadExists(_ string, _ *config.City, target execStoreTarget, id string) bool {
	return target.RigName == "repo" && id == "repo-abc"
}

func writeBdFixtureFile(t testing.TB, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

var bdInvocationForms = []string{"city", "rig-cwd", "registered-rig", "rig-flag", "city-flag", "by-id"}

func newBdInvocationFixture(t testing.TB, form string) bdInvocationFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exe, err := resolveBdInvokingGCBinary()
	if err != nil {
		t.Fatal(err)
	}
	f := bdInvocationFixture{
		root: root, city: filepath.Join(root, "city"), rig: filepath.Join(root, "repo"),
		capture: filepath.Join(root, "capture"), executable: exe, args: []string{"version"},
	}
	for _, key := range liveEnvKeysForTests() {
		t.Setenv(key, "")
	}
	bin := filepath.Join(root, "bin")
	t.Setenv("HOME", root)
	t.Setenv("GC_HOME", filepath.Join(root, "gc-home"))
	t.Setenv("PATH", bin+":/usr/bin:/bin:/usr/sbin")
	t.Setenv("GC_CITY_PATH", f.city)
	t.Setenv(bdCaptureFile, f.capture)
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, filepath.Join(bin, "bd")); err != nil {
		t.Fatal(err)
	}
	body := "[workspace]\nname = \"demo\"\n"
	if form == "city-flag" {
		body = hostedBeadsCityTOML("https://beads.example.test", config.StorageAuthCredentialProvider, false)
	}
	if form == "rig-flag" {
		pinned := filepath.Join(root, "pinned", "bd")
		if err := os.Mkdir(filepath.Dir(pinned), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(exe, pinned); err != nil {
			t.Fatal(err)
		}
		body += fmt.Sprintf("\n[workspace.env]\nBD_BIN = %q\n", pinned)
	}
	meta := `{"backend":"doltlite"}`
	if form == "city-flag" {
		meta = completeStorageBindingJSON
	}
	writeBdFixtureFile(t, filepath.Join(f.city, ".beads", "metadata.json"), meta, 0o600)
	writeBdFixtureFile(t, filepath.Join(f.rig, ".beads", "metadata.json"), `{"backend":"doltlite"}`, 0o600)
	body += "\n[[rigs]]\nname = \"repo\"\nprefix = \"repo\"\n"
	if form == "registered-rig" {
		writeBdFixtureFile(t, filepath.Join(f.city, ".gc", "site.toml"), fmt.Sprintf("[[rig]]\nname = \"repo\"\npath = %q\n", f.rig), 0o600)
		reg := supervisor.NewRegistry(supervisor.RegistryPath())
		if err := reg.Register(f.city, "demo"); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GC_CITY_PATH", "")
	} else {
		body += fmt.Sprintf("path = %q\n", f.rig)
	}
	for i := 1; i < 7; i++ {
		body += fmt.Sprintf("\n[[rigs]]\nname = \"repo%d\"\npath = \"repo%d\"\n", i, i)
	}
	writeBdFixtureFile(t, filepath.Join(f.city, "city.toml"), body, 0o600)
	cwd := f.city
	switch form {
	case "rig-cwd", "registered-rig":
		cwd = f.rig
	case "rig-flag":
		f.args = []string{"--rig", "repo", "version"}
	case "city-flag":
		cwd = f.rig
		f.args = []string{"--city", f.city, "sql", "SELECT 1"}
	case "by-id":
		probe := bdBeadExists
		t.Cleanup(func() { bdBeadExists = probe })
		bdBeadExists = fixtureBeadExists
		f.args = []string{"show", "repo-abc"}
	}
	t.Chdir(cwd)
	return f
}

func (f bdInvocationFixture) run(t testing.TB) {
	t.Helper()
	var out, diag bytes.Buffer
	if code := doBd(f.args, &out, &diag); code != 0 || out.String() != "fake bd\n" || !strings.HasSuffix(diag.String(), "fake bd diagnostic\n") {
		t.Fatalf("doBd(%q) = %d, stdout %q, stderr %q", f.args, code, out.String(), diag.String())
	}
}

func (f bdInvocationFixture) normalize(value string) string {
	return strings.NewReplacer(filepath.Join(f.root, "gc-bd-invocation-test"), "{gc}",
		filepath.Join(normalizePathForCompare(f.root), "gc-bd-invocation-test"), "{gc}",
		f.root, "{root}", normalizePathForCompare(f.root), "{root}",
		f.executable, "{gc}", normalizePathForCompare(f.executable), "{gc}").Replace(value)
}

// Run in a fresh process so a whole-environment comparison starts with
// identical input and empty readiness, routing and warning caches.
func (f bdInvocationFixture) runIsolated(t *testing.T) string {
	t.Helper()
	program := filepath.Join(f.root, "gc-bd-invocation-test")
	if err := os.Symlink(f.executable, program); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + f.root, "GC_HOME=" + filepath.Join(f.root, "gc-home"), "PATH=" + os.Getenv("PATH"), bdCaptureFile + "=" + f.capture}
	if city := os.Getenv("GC_CITY_PATH"); city != "" {
		env = append(env, "GC_CITY_PATH="+city)
	}
	if dir := os.Getenv("GOCOVERDIR"); dir != "" {
		env = append(env, "GOCOVERDIR="+dir)
	}
	out, diag, err := runGCBeadsBdCommand(t, env, program, f.args...)
	if err != nil || out != "fake bd\n" || !strings.HasSuffix(diag, "fake bd diagnostic\n") {
		t.Fatalf("doBd(%q): %v, stdout %q, stderr %q", f.args, err, out, diag)
	}
	return f.normalize(diag)
}

func bdConfigBuildCounts() [4]int64 {
	return [4]int64{loadCityConfigCalls.Load(), loadPrematerializedCityConfigCalls.Load(), hostedCredentialProbeLoads.Load(), cliStorageConfigReads.Load()}
}

func TestBdInvocationConfigBuilds(t *testing.T) {
	for _, form := range bdInvocationForms {
		t.Run(form, func(t *testing.T) {
			f := newBdInvocationFixture(t, form)
			before := bdConfigBuildCounts()
			f.run(t)
			delta := bdConfigBuildCounts()
			var total int64
			for i := range delta {
				delta[i] -= before[i]
				total += delta[i]
			}
			want := int64(1)
			if form == "registered-rig" {
				want++ // the site registry must establish which city owns the cwd
			}
			t.Logf("config builds %d (full, prematerialized, credential, routing = %v)", total, delta)
			if total != want {
				t.Errorf("config builds = %d, want %d", total, want)
			}
		})
	}
}

func TestBdInvocationCacheComparisons(t *testing.T) {
	f := newBdInvocationFixture(t, "city")
	if err := EnsureBuiltinRuntimeAssets(f.city, io.Discard); err != nil {
		t.Fatal(err)
	}
	builtinRuntimeReadyCache.Delete(normalizePathForCompare(f.city))
	counts := make(map[string]int)
	original := validateSyntheticCache
	t.Cleanup(func() { validateSyntheticCache = original })
	validateSyntheticCache = func(path, repo, commit string) error {
		counts[path]++
		return original(path, repo, commit)
	}
	f.run(t)
	if len(counts) == 0 {
		t.Fatal("readiness did not validate any materialized directory")
	}
	for path, n := range counts {
		if n != 1 {
			t.Errorf("cache directory %s compared %d times, want 1", filepath.Base(path), n)
		}
	}
}

func TestBdInvocationGoldens(t *testing.T) {
	dir, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, form := range bdInvocationForms {
		t.Run(form, func(t *testing.T) {
			f := newBdInvocationFixture(t, form)
			f.runIsolated(t)
			raw, err := os.ReadFile(f.capture)
			if err != nil {
				t.Fatal(err)
			}
			got := f.normalize(string(raw))
			path := filepath.Join(dir, "bd-"+form+".golden")
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("bd capture differs from %s:\n%s", filepath.Base(path), got)
			}
		})
	}
}

func TestBdInvocationHealthyDiagnostics(t *testing.T) {
	got := make(map[string]string)
	for _, form := range bdInvocationForms {
		t.Run(form, func(t *testing.T) {
			f := newBdInvocationFixture(t, form)
			got[form] = f.runIsolated(t)
		})
	}
	data, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "bd-diagnostics.golden")
	want, err := os.ReadFile(path)
	if err != nil || string(want) != string(data)+"\n" {
		t.Fatalf("healthy diagnostics differ: %v\n%s", err, data)
	}
}

func TestBdInvocationDegradedDiagnostics(t *testing.T) {
	ordinary, err := os.ReadFile(filepath.Join("testdata", "bd-diagnostics.golden"))
	if err != nil {
		t.Fatal(err)
	}
	var healthy map[string]string
	if err := json.Unmarshal(ordinary, &healthy); err != nil {
		t.Fatal(err)
	}
	wantCapture, err := os.ReadFile(filepath.Join("testdata", "bd-city.golden"))
	if err != nil {
		t.Fatal(err)
	}
	for _, form := range []string{"explicit", "implicit"} {
		t.Run(form, func(t *testing.T) {
			f := newBdInvocationFixture(t, "city")
			writeBdFixtureFile(t, filepath.Join(f.city, ".gc", "scripts"), "occupied", 0o600)
			if form == "explicit" {
				f.args = append([]string{"--city", f.city}, f.args...)
			}
			const warning = "warning: builtin pack cache refresh incomplete; using existing caches: writing gc-beads-bd shim: mkdir {root}/city/.gc/scripts: not a directory\n"
			if got := f.runIsolated(t); got != warning+healthy["city"] {
				t.Errorf("degraded stderr = %q, want one refresh warning followed by healthy diagnostics", got)
			}
			raw, err := os.ReadFile(f.capture)
			if err != nil || f.normalize(string(raw)) != string(wantCapture) {
				t.Errorf("degraded bd capture differs: %v\n%s", err, f.normalize(string(raw)))
			}
		})
	}
}

func recordingBdProvider(t *testing.T, f bdInvocationFixture, reachable bool) (log string, port int) {
	t.Helper()
	log = filepath.Join(f.root, "provider.log")
	script := filepath.Join(f.root, "gc-beads-bd.sh")
	writeBdFixtureFile(t, log, "", 0o600)
	writeBdFixtureFile(t, script, "#!/bin/sh\nprintf '%s\\n' \"$1\" >> \""+log+"\"\nexit 1\n", 0o700)
	writeBdFixtureFile(t, filepath.Join(f.city, "city.toml"), fmt.Sprintf("[workspace]\nname = \"demo\"\n[beads]\nprovider = %q\n[[rigs]]\nname = \"repo\"\npath = %q\n", "exec:"+script, f.rig), 0o600)
	for _, scope := range []string{f.city, f.rig} {
		writeBdFixtureFile(t, filepath.Join(scope, ".beads", "metadata.json"), `{"backend":"dolt","dolt_database":"demo"}`, 0o600)
	}
	if reachable {
		return log, writeReachableManagedDoltState(t, f.city)
	}
	port = reserveRandomTCPPort(t)
	if err := writeDoltState(f.city, doltRuntimeState{Running: true, PID: os.Getpid(), Port: port, DataDir: filepath.Join(f.city, ".beads", "dolt")}); err != nil {
		t.Fatal(err)
	}
	return log, port
}

func TestBdInvocationProviderLifecycle(t *testing.T) {
	for _, scope := range []string{"city", "rig-cwd"} {
		for _, tc := range []struct {
			args []string
			ops  string
		}{
			{[]string{"version"}, ""},
			{[]string{"help", "list"}, ""},
			{[]string{"--version"}, ""},
			{[]string{"--help"}, ""},
			{[]string{"-h", "--version"}, ""},
			{[]string{"--json", "version"}, ""},
			{[]string{"sql", "SELECT 1"}, "health\nrecover\n"},
			{[]string{"list"}, "health\nrecover\n"},
			{[]string{"future-command", "--help"}, "health\nrecover\n"},
		} {
			t.Run(scope+"/"+strings.Join(tc.args, "_"), func(t *testing.T) {
				f := newBdInvocationFixture(t, scope)
				log, _ := recordingBdProvider(t, f, false)
				f.args = tc.args
				f.run(t)
				got, err := os.ReadFile(log)
				if err != nil {
					t.Fatal(err)
				}
				if tc.ops == "" && len(got) != 0 || !strings.HasPrefix(string(got), tc.ops) {
					t.Fatalf("provider operations = %q; want empty or starting sequence specified by %q", got, tc.ops)
				}
			})
		}
	}
	for _, state := range []string{"reachable", "provider-state", "recover-publishes-endpoint"} {
		t.Run(state, func(t *testing.T) {
			f := newBdInvocationFixture(t, "city")
			log, port := recordingBdProvider(t, f, state == "reachable")
			wantOps := ""
			if state != "reachable" {
				port = writeReachableProviderManagedDoltState(t, f.city)
			}
			if state == "recover-publishes-endpoint" {
				// Make the reachable endpoint discoverable only after recover.
				path := providerManagedDoltStatePath(f.city)
				staged := path + ".staged"
				if err := os.Rename(path, staged); err != nil {
					t.Fatal(err)
				}
				script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$1\" >> %q\n[ \"$1\" = recover ] && mv %q %q\n", log, staged, path)
				writeBdFixtureFile(t, filepath.Join(f.root, "gc-beads-bd.sh"), script, 0o700)
				f.args = []string{"list"}
				wantOps = "health\nrecover\n"
			}
			printer := filepath.Join(f.root, "ports", "bd")
			writeBdFixtureFile(t, printer, "#!/bin/sh\nprintf '%s %s' \"$GC_DOLT_PORT\" \"$BEADS_DOLT_SERVER_PORT\"\n", 0o700)
			t.Setenv("PATH", filepath.Dir(printer)+":"+os.Getenv("PATH"))
			var stdout, stderr bytes.Buffer
			if code := doBd(f.args, &stdout, &stderr); code != 0 || stdout.String() != fmt.Sprintf("%d %d", port, port) {
				t.Fatalf("gc bd %v = %d, projected ports %q, want %d on both, stderr %q", f.args, code, stdout.String(), port, stderr.String())
			}
			if got := currentManagedDoltPort(f.city); got != strconv.Itoa(port) {
				t.Errorf("published port = %q, want %d", got, port)
			}
			if got, err := os.ReadFile(log); err != nil || string(got) != wantOps {
				t.Fatalf("provider operations = %q, %v; want %q", got, err, wantOps)
			}
		})
	}
	t.Run("health-owner", func(t *testing.T) {
		f := newBdInvocationFixture(t, "city")
		log, _ := recordingBdProvider(t, f, false)
		var stderr bytes.Buffer
		code := doBeadsHealth(true, false, io.Discard, &stderr)
		if got, err := os.ReadFile(log); code != 1 || err != nil || string(got) != "health\nrecover\n" {
			t.Fatalf("gc beads health = %d, log %q, err %v, stderr %q", code, got, err, stderr.String())
		}
	})
}

func TestBdInvocationRoutingConfigParity(t *testing.T) {
	for _, mode := range []string{"single", "relocated", "refused"} {
		t.Run(mode, func(t *testing.T) {
			var city string
			switch mode {
			case "single":
				city = oneShotCLICity(t, "")
			case "relocated":
				city, _ = migratedOneShotCLICity(t)
			case "refused":
				city = oneShotCLICity(t, filepath.Join(t.TempDir(), "store"))
				source := stubInfraMigrationSource(t)
				mustCreateInfraBead(t, source, beads.Bead{Title: "session", Type: "session"})
			}
			stderr := captureCLIStorageStderr(t)
			observe := func() string {
				routes := cliStorageRoutes(city)
				if routes == nil {
					return "work"
				}
				classes := make([]string, 0, len(routes.stores))
				for class := range routes.stores {
					classes = append(classes, class.String())
				}
				slices.Sort(classes)
				return routes.binding + "/" + strings.Join(classes, ",")
			}
			want := observe()
			remedy := stderr.String()
			if err := closeCLIStorageRoutes(); err != nil {
				t.Fatal(err)
			}
			stderr.Reset()
			// Enter the by-ID door through the command, then inspect the memo
			// it filled. A missing subject is sufficient to exercise routing.
			before := cliStorageConfigReads.Load()
			doBd([]string{"--city", city, "show", "demo-abc"}, io.Discard, io.Discard)
			if n := cliStorageConfigReads.Load() - before; n != 0 {
				t.Errorf("by-id command performed %d raw config reads; want 0", n)
			}
			got := observe()
			if cliStorageConfigReads.Load() != before || got != want || stderr.String() != remedy {
				t.Errorf("by-id routes %q, stderr %q; want shared routes %q, stderr %q", got, stderr, want, remedy)
			}
		})
	}
}

func BenchmarkGcBdVersion(b *testing.B) {
	f := newBdInvocationFixture(b, "city")
	// Use a tiny version printer so the benchmark measures the wrapper.
	bin := filepath.Join(f.root, "version-bin", "bd")
	writeBdFixtureFile(b, bin, "#!/bin/sh\necho bd-test\n", 0o700)
	b.Setenv("PATH", filepath.Dir(bin)+":/usr/bin:/bin")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		builtinRuntimeReadyCache.Delete(normalizePathForCompare(f.city))
		if code := doBd(f.args, io.Discard, io.Discard); code != 0 {
			b.Fatalf("gc bd version exit %d", code)
		}
	}
}
