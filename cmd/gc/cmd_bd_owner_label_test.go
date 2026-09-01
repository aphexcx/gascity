package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestRewriteBdCreateOwnerLabel is the spec for the owner-label argv rewrite:
// one row per argv shape, with the argv bd must receive.
func TestRewriteBdCreateOwnerLabel(t *testing.T) {
	tests := []struct {
		name          string
		owner         string
		args          []string
		want          []string
		wantAmbiguous bool
	}{
		{"bare create is stamped", "owner:citadel", []string{"create", "t"}, []string{"create", "t", "--labels", "owner:citadel"}, false},
		{"user labels come first", "owner:citadel", []string{"create", "t", "-l", "foo"}, []string{"create", "t", "-l", "foo", "--labels", "owner:citadel"}, false},
		{"leading global flags are kept", "owner:citadel", []string{"--actor", "bob", "create", "t"}, []string{"--actor", "bob", "create", "t", "--labels", "owner:citadel"}, false},
		{"flags before the title are fine", "owner:citadel", []string{"create", "--json", "-t", "bug", "-p", "1", "t"}, []string{"create", "--json", "-t", "bug", "-p", "1", "t", "--labels", "owner:citadel"}, false},
		{"an explicit owner via -l is respected", "owner:citadel", []string{"create", "t", "-l", "owner:jadegate"}, []string{"create", "t", "-l", "owner:jadegate"}, false},
		{"an explicit owner inside a comma list is respected", "owner:citadel", []string{"create", "t", "--labels=a,owner:jadegate"}, []string{"create", "t", "--labels=a,owner:jadegate"}, false},
		{"an explicit owner via the hidden --label alias is respected", "owner:citadel", []string{"create", "t", "--label", "owner:citadel"}, []string{"create", "t", "--label", "owner:citadel"}, false},
		{"the alias without an owner is still stamped", "owner:citadel", []string{"create", "t", "--label", "foo"}, []string{"create", "t", "--label", "foo", "--labels", "owner:citadel"}, false},
		{"--no-owner-label opts out and is stripped", "owner:citadel", []string{"create", "t", "--no-owner-label"}, []string{"create", "t"}, false},
		{"--no-owner-label is stripped even with no identity", "", []string{"create", "--no-owner-label", "t"}, []string{"create", "t"}, false},
		{"no identity leaves argv byte-identical", "", []string{"create", "t", "-l", "foo"}, []string{"create", "t", "-l", "foo"}, false},
		{"the stamp lands before a -- terminator", "owner:citadel", []string{"create", "--", "t"}, []string{"create", "--labels", "owner:citadel", "--", "t"}, false},
		{"an unknown flag is ambiguous and untouched", "owner:citadel", []string{"create", "t", "--weird", "x"}, []string{"create", "t", "--weird", "x"}, true},
		{"an attached short value is ambiguous and untouched", "owner:citadel", []string{"create", "t", "-lfoo"}, []string{"create", "t", "-lfoo"}, true},
		{"the opt-out is stripped even when the rest is ambiguous", "owner:citadel", []string{"create", "t", "--no-owner-label", "-p1"}, []string{"create", "t", "-p1"}, true},
		{"bd 1.2.2 globals before the verb are known", "owner:citadel", []string{"--database", "shared", "create", "t"}, []string{"--database", "shared", "create", "t", "--labels", "owner:citadel"}, false},
		{"an unknown global before a create is ambiguous", "owner:citadel", []string{"--future-flag", "x", "create", "t"}, []string{"--future-flag", "x", "create", "t"}, true},
		{"an unknown global before another verb is not create", "owner:citadel", []string{"--future-flag", "x", "list"}, []string{"--future-flag", "x", "list"}, false},
		{"other verbs pass through", "owner:citadel", []string{"update", "x", "--add-label", "y"}, []string{"update", "x", "--add-label", "y"}, false},
		{"no verb passes through", "owner:citadel", []string{"--json"}, []string{"--json"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := append([]string(nil), tt.args...)
			got, ambiguous := rewriteBdCreateOwnerLabel(in, tt.owner)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("rewriteBdCreateOwnerLabel(%v, %q) = %v, want %v", tt.args, tt.owner, got, tt.want)
			}
			if ambiguous != tt.wantAmbiguous {
				t.Fatalf("ambiguous = %v, want %v", ambiguous, tt.wantAmbiguous)
			}
			if !reflect.DeepEqual(in, tt.args) {
				t.Fatalf("input argv was mutated: %v", in)
			}
		})
	}
}

// createWriteCalls returns the fake bd's captured `create ...` invocations.
func createWriteCalls(t *testing.T, capture string) []string {
	t.Helper()
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "create ") {
			calls = append(calls, line)
		}
	}
	return calls
}

const federatedDemoCityTOML = "[workspace]\nname = \"demo\"\n\n[federation]\nidentity = \"citadel\"\n"

// TestGcBdCreateStampsTheOwnerLabelEndToEnd pins what the real bd receives
// from `gc bd create` on a city with [federation] identity set: the acceptance
// table for gp-0uj item 1.
func TestGcBdCreateStampsTheOwnerLabelEndToEnd(t *testing.T) {
	tests := []struct {
		name string
		city string
		args []string
		want string
	}{
		{"stamped", federatedDemoCityTOML, []string{"create", "t"}, "create t --labels owner:citadel"},
		{"user labels first", federatedDemoCityTOML, []string{"create", "t", "-l", "foo"}, "create t -l foo --labels owner:citadel"},
		{"explicit foreign owner respected", federatedDemoCityTOML, []string{"create", "t", "-l", "owner:jadegate"}, "create t -l owner:jadegate"},
		{"opt-out stripped", federatedDemoCityTOML, []string{"create", "t", "--no-owner-label"}, "create t"},
		{"identity unset is byte-identical", "[workspace]\nname = \"demo\"\n", []string{"create", "t", "-l", "foo"}, "create t -l foo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture := filepath.Join(t.TempDir(), "gc-bd-args.txt")
			fakeBdCityTestSetup(t, tt.city, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"${CAPTURE_PATH}\"\n")
			t.Setenv("CAPTURE_PATH", capture)

			var stdout, stderr bytes.Buffer
			if got := doBd(tt.args, &stdout, &stderr); got != 0 {
				t.Fatalf("doBd(%v) = %d, want 0; stderr=%q", tt.args, got, stderr.String())
			}
			calls := createWriteCalls(t, capture)
			if len(calls) != 1 || calls[0] != tt.want {
				t.Fatalf("bd received %q, want [%q]", calls, tt.want)
			}
			if strings.Contains(stderr.String(), "owner label") {
				t.Fatalf("unexpected owner-label notice on stderr: %q", stderr.String())
			}
		})
	}
}

// TestGcBdCreateAmbiguousArgvPassesThroughWithANotice pins the fail-safe: an
// argv the scanner cannot parse is forwarded unchanged (never refused, never
// stamped) and the operator is told once, on stderr.
func TestGcBdCreateAmbiguousArgvPassesThroughWithANotice(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "gc-bd-args.txt")
	fakeBdCityTestSetup(t, federatedDemoCityTOML, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"${CAPTURE_PATH}\"\n")
	t.Setenv("CAPTURE_PATH", capture)

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"create", "t", "--weird", "x"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd = %d, want 0 (bd decides, not gc); stderr=%q", got, stderr.String())
	}
	calls := createWriteCalls(t, capture)
	if len(calls) != 1 || calls[0] != "create t --weird x" {
		t.Fatalf("bd received %q, want the argv unchanged", calls)
	}
	if !strings.Contains(stderr.String(), "gc bd: owner label not injected (ambiguous argv)") {
		t.Fatalf("stderr = %q, want the one-line owner-label notice", stderr.String())
	}
}
