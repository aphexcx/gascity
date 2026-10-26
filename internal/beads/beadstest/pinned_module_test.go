package beadstest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/module"
)

// TestPinnedBeadsModuleDirRefusesAnUnresolvedCache is the fence under the drift
// check's one silent-green path.
//
// The resolver is a hand-rolled copy of cmd/go's GOMODCACHE -> go env file ->
// GOPATH[0]/pkg/mod chain, kept inlined because shelling out to `go env` would
// grow the repo's shrink-only subprocess census. An inlined copy is only
// defensible if disagreeing with cmd/go is fatal, so that is asserted here
// rather than left to the one caller.
func TestPinnedBeadsModuleDirRefusesAnUnresolvedCache(t *testing.T) {
	t.Run("a cache that does not hold the module", func(t *testing.T) {
		if _, err := pinnedBeadsModuleDir(filepath.Join(t.TempDir(), "empty"), PinnedBeadsModulePath, "v1.3.0"); err == nil {
			t.Fatal("pinnedBeadsModuleDir accepted a cache with no pinned module in it; a skip here lets a resolver bug read as green")
		}
	})

	t.Run("a path that is not a directory", func(t *testing.T) {
		cache := t.TempDir()
		path := filepath.Join(cache, filepath.FromSlash(PinnedBeadsModulePath)+"@v1.3.0")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("not a module"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := pinnedBeadsModuleDir(cache, PinnedBeadsModulePath, "v1.3.0"); err == nil {
			t.Fatal("pinnedBeadsModuleDir accepted a file where the module source should be")
		}
	})

	t.Run("the unpacked module", func(t *testing.T) {
		cache := t.TempDir()
		want := filepath.Join(cache, filepath.FromSlash(PinnedBeadsModulePath)+"@v1.3.0")
		if err := os.MkdirAll(want, 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := pinnedBeadsModuleDir(cache, PinnedBeadsModulePath, "v1.3.0")
		if err != nil {
			t.Fatalf("pinnedBeadsModuleDir: %v", err)
		}
		if got != want {
			t.Fatalf("pinnedBeadsModuleDir = %q, want %q", got, want)
		}
	})
}

// TestPinnedBeadsModuleDirFailsRatherThanSkips pins the seam itself.
//
// The subtests above prove the resolver returns an error; this proves what the
// caller does with it. A revert of the t.Fatalf to a t.Skipf would leave every
// other test in this package green while the pinned-cursor drift check silently
// stopped running — which is the failure mode it was written against, and it was
// reproduced with nothing more than GOMODCACHE pointed somewhere else.
func TestPinnedBeadsModuleDirFailsRatherThanSkips(t *testing.T) {
	reporter := &recordingModuleDirReporter{}
	if dir := pinnedBeadsModuleDirOrFatal(reporter, filepath.Join(t.TempDir(), "empty"), PinnedBeadsModulePath, "v1.3.0"); dir != "" {
		t.Fatalf("an unresolved cache produced the directory %q", dir)
	}
	if len(reporter.skips) != 0 {
		t.Fatalf("an unresolved module cache was SKIPPED (%q); a skip lets the drift check go quiet and read as green", reporter.skips)
	}
	if len(reporter.fatals) != 1 {
		t.Fatalf("an unresolved module cache reported %d fatal(s), want exactly 1: %q", len(reporter.fatals), reporter.fatals)
	}
	for _, want := range []string{"GOMODCACHE", PinnedBeadsModulePath} {
		if !strings.Contains(reporter.fatals[0], want) {
			t.Errorf("the failure message does not name %q, so the operator cannot act on it:\n%s", want, reporter.fatals[0])
		}
	}
}

func TestPinnedBeadsModuleReplacement(t *testing.T) {
	const required = "require github.com/steveyegge/beads v1.3.1\n"
	const fork = "github.com/aphexcx/beads"
	const version = "v0.59.1-0.20261013092804-43ba1e2ebfb3"
	for _, tc := range []struct {
		name, directives string
		want             module.Version
	}{
		{"upstream", required, module.Version{Path: PinnedBeadsModulePath, Version: "v1.3.1"}},
		{"fork", required + "replace github.com/steveyegge/beads => " + fork + " " + version, module.Version{Path: fork, Version: version}},
		{"grouped_versioned", required + "replace (\n github.com/steveyegge/beads v1.3.1 => " + fork + " " + version + " // fork\n)", module.Version{Path: fork, Version: version}},
		{"other_version", required + "replace github.com/steveyegge/beads v1.3.0 => " + fork + " " + version, module.Version{Path: PinnedBeadsModulePath, Version: "v1.3.1"}},
		{"local", required + "replace github.com/steveyegge/beads => ../beads", module.Version{Path: "../beads"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pinnedBeadsModule([]byte("module example.com/test\n" + tc.directives + "\n"))
			if err != nil || got != tc.want {
				t.Fatalf("pinnedBeadsModule = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	for _, data := range []string{"not a module", "module example.com/test\n"} {
		if _, err := pinnedBeadsModule([]byte(data)); err == nil {
			t.Fatalf("accepted invalid or missing requirement: %q", data)
		}
	}
}

// recordingModuleDirReporter records what the resolver reports instead of
// failing or skipping the test that drives it. It does not call runtime.Goexit
// on Fatalf, so the caller returns normally and the zero value it hands back is
// asserted too.
type recordingModuleDirReporter struct {
	fatals []string
	skips  []string
}

func (r *recordingModuleDirReporter) Helper() {}

func (r *recordingModuleDirReporter) Fatalf(format string, args ...any) {
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
}

func (r *recordingModuleDirReporter) Skipf(format string, args ...any) {
	r.skips = append(r.skips, fmt.Sprintf(format, args...))
}
