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
		name       string
		owner      string
		args       []string
		want       []string
		wantNotice string
	}{
		{"bare create is stamped", "owner:citadel", []string{"create", "t"}, []string{"create", "t", "--labels", "owner:citadel"}, ""},
		{"user labels come first", "owner:citadel", []string{"create", "t", "-l", "foo"}, []string{"create", "t", "-l", "foo", "--labels", "owner:citadel"}, ""},
		{"leading global flags are kept", "owner:citadel", []string{"--actor", "bob", "create", "t"}, []string{"--actor", "bob", "create", "t", "--labels", "owner:citadel"}, ""},
		{"flags before the title are fine", "owner:citadel", []string{"create", "--json", "-t", "bug", "-p", "1", "t"}, []string{"create", "--json", "-t", "bug", "-p", "1", "t", "--labels", "owner:citadel"}, ""},
		{"an explicit owner via -l is respected", "owner:citadel", []string{"create", "t", "-l", "owner:jadegate"}, []string{"create", "t", "-l", "owner:jadegate"}, ""},
		{"an explicit owner inside a comma list is respected", "owner:citadel", []string{"create", "t", "--labels=a,owner:jadegate"}, []string{"create", "t", "--labels=a,owner:jadegate"}, ""},
		{"an explicit owner via the hidden --label alias is respected", "owner:citadel", []string{"create", "t", "--label", "owner:citadel"}, []string{"create", "t", "--label", "owner:citadel"}, ""},
		{"the alias without an owner is still stamped", "owner:citadel", []string{"create", "t", "--label", "foo"}, []string{"create", "t", "--label", "foo", "--labels", "owner:citadel"}, ""},
		{"--no-owner-label opts out and is stripped", "owner:citadel", []string{"create", "t", "--no-owner-label"}, []string{"create", "t"}, ""},
		{"--no-owner-label is stripped even with no identity", "", []string{"create", "--no-owner-label", "t"}, []string{"create", "t"}, ""},
		{"an ambiguous argv with no identity is silent but still stripped", "", []string{"create", "t", "--no-owner-label", "-p1"}, []string{"create", "t", "-p1"}, ""},
		{"no identity leaves argv byte-identical", "", []string{"create", "t", "-l", "foo"}, []string{"create", "t", "-l", "foo"}, ""},
		{"the stamp lands before a -- terminator", "owner:citadel", []string{"create", "--", "t"}, []string{"create", "--labels", "owner:citadel", "--", "t"}, ""},
		{"an unknown flag is ambiguous and untouched", "owner:citadel", []string{"create", "t", "--weird", "x"}, []string{"create", "t", "--weird", "x"}, bdOwnerLabelNotInjectedNotice},
		{"an attached short value is ambiguous and untouched", "owner:citadel", []string{"create", "t", "-lfoo"}, []string{"create", "t", "-lfoo"}, bdOwnerLabelNotInjectedNotice},
		{"the opt-out is stripped even when the rest is ambiguous", "owner:citadel", []string{"create", "t", "--no-owner-label", "-p1"}, []string{"create", "t", "-p1"}, bdOwnerLabelNotInjectedNotice},
		{"bd 1.2.2 globals before the verb are known", "owner:citadel", []string{"--database", "shared", "create", "t"}, []string{"--database", "shared", "create", "t", "--labels", "owner:citadel"}, ""},
		{"an unknown global before a create is ambiguous", "owner:citadel", []string{"--future-flag", "x", "create", "t"}, []string{"--future-flag", "x", "create", "t"}, bdOwnerLabelNotInjectedNotice},
		{"an unknown global before another verb is not create", "owner:citadel", []string{"--future-flag", "x", "list"}, []string{"--future-flag", "x", "list"}, ""},
		{"bd 1.2.2 create flags are known", "owner:citadel", []string{"create", "t", "--storage-class", "unversioned", "--allow-empty-description"}, []string{"create", "t", "--storage-class", "unversioned", "--allow-empty-description", "--labels", "owner:citadel"}, ""},
		{"a graph create supplies its own fields", "owner:citadel", []string{"create", "--graph", "plan.json"}, []string{"create", "--graph", "plan.json"}, bdOwnerLabelBatchNotice},
		{"a markdown create supplies its own fields", "owner:citadel", []string{"create", "--file", "issues.md", "--no-owner-label"}, []string{"create", "--file", "issues.md"}, bdOwnerLabelBatchNotice},
		{"a stdin create supplies its own fields", "owner:citadel", []string{"create", "--stdin"}, []string{"create", "--stdin"}, bdOwnerLabelBatchNotice},
		{"a batch create with no identity is silent", "", []string{"create", "--graph", "plan.json"}, []string{"create", "--graph", "plan.json"}, ""},
		{"other verbs pass through", "owner:citadel", []string{"update", "x", "--add-label", "y"}, []string{"update", "x", "--add-label", "y"}, ""},
		{"no verb passes through", "owner:citadel", []string{"--json"}, []string{"--json"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := append([]string(nil), tt.args...)
			got, notice := rewriteBdCreateOwnerLabel(in, tt.owner)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("rewriteBdCreateOwnerLabel(%v, %q) = %v, want %v", tt.args, tt.owner, got, tt.want)
			}
			if notice != tt.wantNotice {
				t.Fatalf("notice = %q, want %q", notice, tt.wantNotice)
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

// TestGcBdCreateAmbiguousArgvStillStripsTheOptOut: whatever the scanner made
// of the rest, bd never sees the gc-only flag.
func TestGcBdCreateAmbiguousArgvStillStripsTheOptOut(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "gc-bd-args.txt")
	fakeBdCityTestSetup(t, federatedDemoCityTOML, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"${CAPTURE_PATH}\"\n")
	t.Setenv("CAPTURE_PATH", capture)

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"create", "t", "-p1", "--no-owner-label"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd = %d, want 0; stderr=%q", got, stderr.String())
	}
	calls := createWriteCalls(t, capture)
	if len(calls) != 1 || calls[0] != "create t -p1" {
		t.Fatalf("bd received %q, want the argv with only the opt-out stripped", calls)
	}
	if !strings.Contains(stderr.String(), bdOwnerLabelNotInjectedNotice) {
		t.Fatalf("stderr = %q, want the ambiguous-argv notice", stderr.String())
	}
}

// TestGcBdCreateBatchModesPassThroughWithANotice: --graph/--file/--stdin
// creates carry their fields in the file, where bd never reads --labels, so
// the argv is forwarded untouched and the operator is told to label per item.
func TestGcBdCreateBatchModesPassThroughWithANotice(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "gc-bd-args.txt")
	fakeBdCityTestSetup(t, federatedDemoCityTOML, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"${CAPTURE_PATH}\"\n")
	t.Setenv("CAPTURE_PATH", capture)

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"create", "--graph", "plan.json"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd = %d, want 0; stderr=%q", got, stderr.String())
	}
	calls := createWriteCalls(t, capture)
	if len(calls) != 1 || calls[0] != "create --graph plan.json" {
		t.Fatalf("bd received %q, want the batch argv unchanged", calls)
	}
	if !strings.Contains(stderr.String(), bdOwnerLabelBatchNotice) {
		t.Fatalf("stderr = %q, want the batch-create notice", stderr.String())
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
