package beadstest

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

// TestPinnedBeadsModuleDirResolvesThisBuildsOwnCache is the live half of the
// resolver's fence: the subtests in pinned_module_test.go feed it temporary
// directories, and this one asks it to resolve the cache the running test binary
// was actually linked from, because that is the resolution the pinned-cursor
// drift check depends on.
//
// It lives in a file of its own because it is the one test in this package that
// cannot run hermetically. PinnedBeadsModuleDir walks up from os.Getwd() for
// go.mod and then reads GOMODCACHE/GOPATH/GOENV/HOME; under a bazel test sandbox
// the working directory is a runfiles tree with no go.mod above it and the
// environment is scrubbed, so the resolver fails — and it is meant to fail
// rather than skip (that is what pinnedBeadsModuleDirOrFatal's recorder pins).
// Separating it lets internal/beads/beadstest/BUILD.bazel defer this file alone
// from the hermetic Bazel target while every other test in the package, this
// one included, keeps running in the Go suite.
func TestPinnedBeadsModuleDirResolvesThisBuildsOwnCache(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(RepositoryRoot(t), "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, replacement := range f.Replace {
		if replacement.Old.Path != PinnedBeadsModulePath {
			continue
		}
		if got := PinnedBeadsVersion(t); got != replacement.New.Version {
			t.Errorf("PinnedBeadsVersion = %q, want replacement version %q", got, replacement.New.Version)
		}
		path, err := module.EscapePath(replacement.New.Path)
		if err != nil {
			t.Fatal(err)
		}
		version, err := module.EscapeVersion(replacement.New.Version)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(goModuleCache(t), filepath.FromSlash(path)+"@"+version)
		if got := PinnedBeadsModuleDir(t); got != want {
			t.Errorf("PinnedBeadsModuleDir = %q, want replacement source %q", got, want)
		}
		return
	}
	if PinnedBeadsModuleDir(t) == "" {
		t.Fatal("PinnedBeadsModuleDir returned no source")
	}
}
