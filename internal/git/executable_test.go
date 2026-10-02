package git

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// A failed probe must not prevent an import reload from seeing a repaired Git.
func TestExecutableResolutionRetriesFailures(t *testing.T) {
	for _, tc := range []struct {
		name, output, repairedPath string
		code                       int
		err                        error
	}{
		{"license accepted", "Xcode license", "/path/git", 69, errors.New("exit status 69")},
		{"fallback installed", "Xcode license", "/clt/git", 69, errors.New("exit status 69")},
		{"transient timeout", "", "/path/git", -1, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repaired := false
			probes, warnings := 0, 0
			resolve := cachedExecutableResolution("darwin", "/path/git", "/clt/git", func(path string) (string, int, error) {
				probes++
				if repaired && path == tc.repairedPath {
					return "git version 2.49", 0, nil
				}
				if path == "/path/git" {
					return tc.output, tc.code, tc.err
				}
				return "", -1, errors.New("no such file or directory")
			}, func(string) { warnings++ })
			if _, err := resolve(); err == nil {
				t.Fatal("first resolution succeeded with both executables unusable")
			}
			repaired = true
			got, err := resolve()
			if err != nil || got.Path != tc.repairedPath {
				t.Fatalf("resolution after repair = %+v, %v; want %s", got, err, tc.repairedPath)
			}
			probesAfterRepair := probes
			if cached, err := resolve(); err != nil || cached != got || probes != probesAfterRepair {
				t.Fatalf("successful resolution not cached: %+v, %v, probes %d -> %d", cached, err, probesAfterRepair, probes)
			}
			wantWarnings := 0
			if tc.repairedPath == "/clt/git" {
				wantWarnings = 1
			}
			if warnings != wantWarnings {
				t.Fatalf("warnings = %d, want %d", warnings, wantWarnings)
			}
		})
	}
}

// Concurrent import reads must share both the version probes and the warning.
func TestExecutableResolutionCached(t *testing.T) {
	var probes, warnings atomic.Int32
	resolve := cachedExecutableResolution("darwin", "/path/git", "/clt/git", func(path string) (string, int, error) {
		probes.Add(1)
		if path == "/path/git" {
			return "Xcode license", 69, errors.New("exit status 69")
		}
		return "git version 2.49", 0, nil
	}, func(string) { warnings.Add(1) })
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			got, err := resolve()
			if err != nil || got.Path != "/clt/git" {
				t.Errorf("cached result = %+v, %v", got, err)
			}
		})
	}
	wg.Wait()
	if probes.Load() != 2 || warnings.Load() != 1 {
		t.Fatalf("probes=%d warnings=%d; want 2 and 1", probes.Load(), warnings.Load())
	}
}

// The resolver must distinguish a broken executable from a repository failure,
// and may select the macOS fallback only for the Xcode license signature.
func TestResolveExecutable(t *testing.T) {
	for _, tc := range []struct {
		name, goos, output string
		code               int
		primaryErr         error
		fallbackErr        error
		wantPath           string
		wantError          bool
		wantFallback       bool
	}{
		{name: "healthy", goos: "darwin", output: "git version 2.50", wantPath: "/path/git"},
		{name: "license exit", goos: "darwin", code: 69, primaryErr: errors.New("exit status 69"), wantPath: "/clt/git", wantFallback: true},
		{name: "license text", goos: "darwin", code: 1, output: "You have not agreed to the Xcode license agreements.\nPlease accept them.", primaryErr: errors.New("exit status 1"), wantPath: "/clt/git", wantFallback: true},
		{name: "iOS license text", goos: "darwin", code: 1, output: "Agreeing to the Xcode/iOS license requires admin privileges.", primaryErr: errors.New("exit status 1"), wantPath: "/clt/git", wantFallback: true},
		{name: "fallback absent", goos: "darwin", code: 69, output: "Xcode license\nnot accepted", primaryErr: errors.New("exit status 69"), fallbackErr: errors.New("no such file or directory"), wantError: true, wantFallback: true},
		{name: "fallback broken", goos: "darwin", code: 69, primaryErr: errors.New("exit status 69"), fallbackErr: errors.New("permission denied"), wantError: true, wantFallback: true},
		{name: "other failure", goos: "darwin", code: 1, output: "bad executable", primaryErr: errors.New("exit status 1"), wantError: true},
		{name: "other platform", goos: "linux", code: 69, primaryErr: errors.New("exit status 69"), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fallbackCalls := 0
			probe := func(path string) (string, int, error) {
				if path == "/path/git" {
					return tc.output, tc.code, tc.primaryErr
				}
				if path != "/clt/git" {
					t.Fatalf("unexpected candidate %q", path)
				}
				fallbackCalls++
				return "git version 2.49", -1, tc.fallbackErr
			}
			got, err := resolveExecutable(tc.goos, "/path/git", "/clt/git", probe)
			if (err != nil) != tc.wantError || got.Path != tc.wantPath {
				t.Fatalf("got %+v, %v; want path %q, error %v", got, err, tc.wantPath, tc.wantError)
			}
			if (fallbackCalls == 1) != tc.wantFallback {
				t.Fatalf("fallback calls = %d, want fallback %v", fallbackCalls, tc.wantFallback)
			}
			if tc.wantFallback {
				diagnostic := got.Warning
				if err != nil {
					diagnostic = err.Error()
				}
				for _, part := range []string{"/path/git", "/clt/git", "sudo xcodebuild -license accept"} {
					if !strings.Contains(diagnostic, part) {
						t.Errorf("diagnostic %q missing %q", diagnostic, part)
					}
				}
				if strings.ContainsAny(diagnostic, "\n\r") {
					t.Errorf("multiline diagnostic: %q", diagnostic)
				}
			}
			if err != nil && !strings.Contains(err.Error(), "git itself is unusable") {
				t.Errorf("error does not name broken git: %v", err)
			}
		})
	}
}
