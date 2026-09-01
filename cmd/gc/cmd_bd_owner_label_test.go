package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestStripBdNoOwnerLabel: the gc-owned opt-out never reaches bd, in any of
// its spellings, and its value is honored — `=false` is not an opt-out.
func TestStripBdNoOwnerLabel(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    []string
		wantOut bool
	}{
		{"absent", []string{"create", "t"}, []string{"create", "t"}, false},
		{"bare", []string{"create", "t", "--no-owner-label"}, []string{"create", "t"}, true},
		{"inline true", []string{"create", "--no-owner-label=true", "t"}, []string{"create", "t"}, true},
		{"inline false is not an opt-out", []string{"create", "t", "--no-owner-label=false"}, []string{"create", "t"}, false},
		{"after -- it is bd's positional", []string{"create", "--", "--no-owner-label"}, []string{"create", "--", "--no-owner-label"}, false},
		{"ambiguous rest is left alone", []string{"create", "t", "--no-owner-label", "-p1"}, []string{"create", "t", "-p1"}, true},
		{"a value flag's value is not a flag", []string{"create", "t", "--description", "--no-owner-label"}, []string{"create", "t", "--description", "--no-owner-label"}, false},
		{"a malformed value is left for bd to reject", []string{"create", "t", "--no-owner-label=tru"}, []string{"create", "t", "--no-owner-label=tru"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := append([]string(nil), tt.args...)
			got, optOut := stripBdNoOwnerLabel(in)
			if !reflect.DeepEqual(got, tt.want) || optOut != tt.wantOut {
				t.Fatalf("stripBdNoOwnerLabel(%v) = (%v, %v), want (%v, %v)", tt.args, got, optOut, tt.want, tt.wantOut)
			}
			if !reflect.DeepEqual(in, tt.args) {
				t.Fatalf("input argv was mutated: %v", in)
			}
		})
	}
}

// TestBdLooksLikeCreate: the trigger for reading bd's output is generous on
// purpose (create or its alias anywhere before a `--`): a false positive only
// costs a scan of output that names no created bead, while a miss leaves a
// bead unlabeled.
func TestBdLooksLikeCreate(t *testing.T) {
	create := [][]string{
		{"create", "t"},
		{"new", "t"},
		{"--actor", "bob", "create", "t"},
		{"--database", "shared", "new", "t"},
		{"create", "--graph", "p.json"},
		// pflag's shorthand forms of bd's globals: a cluster of booleans, a
		// value attached to a shorthand (with or without `=`), a cluster that
		// ends in a value flag with its value attached, an inline boolean.
		{"-vv", "create", "t"},
		{"-C/tmp", "create", "t"},
		{"-C=/tmp", "create", "t"},
		{"-qC/tmp", "create", "t"},
		{"-v=false", "create", "t"},
		// cobra searches for the verb without consuming what a cluster's last
		// value flag will take: `bd -vC create t u` runs create with -C=t.
		{"-vC", "create", "t", "u"},
	}
	// A global flag's VALUE is never the verb: `--actor create show x` is a
	// show, and stamping what it prints would mutate a read.
	other := [][]string{{"list"}, {"show", "--", "create"}, {"--actor", "create", "show", "ci-1", "--json"}, {"-C", "create", "show", "ci-1"}, {"update", "x", "--add-label", "create"}, {}}
	// A leading flag neither global manifest knows hides the verb: gc cannot
	// tell a create from a show, so it must not stamp — only say so. A
	// shorthand gc does not know hides it wherever it sits in a cluster.
	hidden := [][]string{{"--future-flag", "x", "create", "t"}, {"--future-flag", "x", "list"}, {"-x", "create", "t"}, {"-vx", "create", "t"}, {"-vxC/tmp", "create", "t"}}
	for _, args := range create {
		if got := bdCreateVerdict(args); got != bdVerbCreate {
			t.Errorf("bdCreateVerdict(%v) = %v, want create", args, got)
		}
	}
	for _, args := range other {
		if got := bdCreateVerdict(args); got != bdVerbOther {
			t.Errorf("bdCreateVerdict(%v) = %v, want other", args, got)
		}
	}
	for _, args := range hidden {
		if got := bdCreateVerdict(args); got != bdVerbHidden {
			t.Errorf("bdCreateVerdict(%v) = %v, want hidden", args, got)
		}
	}
	if !bdHasBoolFlag([]string{"create", "t", "--dry-run"}, "--dry-run") || bdHasBoolFlag([]string{"create", "t", "--description", "--dry-run"}, "--dry-run") {
		t.Error("bdHasBoolFlag must skip a value flag's value")
	}
}

// TestBdCreatedIDs pins id extraction to bd 1.2.2's own output shapes: the
// single create line, --json object, --silent bare id, the graph and markdown
// batch listings and their JSON forms, and a dry run that created nothing.
func TestBdCreatedIDs(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want []string
	}{
		{"single create", "\n✓ Created issue: hw-a1b — Fix the thing\n  Type:     task\n", []string{"hw-a1b"}},
		{"single create without a title", "✓ Created issue: hw-a1b\n", []string{"hw-a1b"}},
		{"hierarchical child id", "✓ Created issue: ci-root.1 — sub\n", []string{"ci-root.1"}},
		{"json object", "{\n  \"id\": \"hw-a1b\",\n  \"title\": \"t\"\n}\n", []string{"hw-a1b"}},
		{"json dry run has no id", "{\n  \"id\": \"\",\n  \"title\": \"t\"\n}\n", nil},
		{"silent bare id", "hw-a1b\n", []string{"hw-a1b"}},
		{"graph text", "Created 2 issues\n  root -> hw-r00\n  step -> hw-s01\n", []string{"hw-r00", "hw-s01"}},
		{"graph json", "{\"ids\":{\"root\":\"hw-r00\",\"step\":\"hw-s01\"}}\n", []string{"hw-r00", "hw-s01"}},
		{"markdown text", "✓ Created 2 issues from issues.md:\n  hw-m01: First [P2, task]\n  hw-m02: Second [P1, bug]\n", []string{"hw-m01", "hw-m02"}},
		{"markdown json", "[{\"id\":\"hw-m01\"},{\"id\":\"hw-m02\"}]\n", []string{"hw-m01", "hw-m02"}},
		{"text dry run", "⚠ [DRY RUN] Would create issue:\n  ID: hw-xyz\n  Title: t\n", nil},
		{"unrelated output", "some other command output\n", nil},
		{"empty", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bdCreatedIDs(tt.out)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("bdCreatedIDs(%q) = %v, want %v", tt.out, got, tt.want)
			}
		})
	}
}

// createFakeBd is a bd that creates by printing bd's own success line, answers
// show --json with the labels the test chose (LABELS env, comma-separated),
// and records every invocation. update never fails.
const createFakeBd = `#!/bin/sh
printf '%s\n' "$*" >> "${CAPTURE_PATH}"
# bd's leading globals sit before the verb (a cluster like -vv, an attached value like -C/path).
while [ $# -gt 0 ]; do case "$1" in -*) shift ;; *) break ;; esac; done
case "$1" in
  create|new)
    if [ "${CREATE_OUT:-}" != "" ]; then printf '%s\n' "${CREATE_OUT}"; else printf '\n✓ Created issue: demo-1 — t\n  Type:     task\n'; fi
    exit "${CREATE_EXIT:-0}" ;;
  show)
    labels=""
    src="${LABELS:-}"
    if [ "$3" = "demo-0" ]; then src="hold:mayor,owner:jadegate"; fi
    if [ "$src" != "" ]; then labels=$(printf '%s' "$src" | awk -F, '{for(i=1;i<=NF;i++){printf "%s\"%s\"", (i>1?",":""), $i}}'); fi
    parent=""
    if [ "$3" != "demo-0" ] && [ "${PARENT:-}" != "" ]; then parent=",\"parent\":\"${PARENT}\""; fi
    printf '[{"id":"%s","title":"t","status":"open","issue_type":"task","created_at":"2026-09-01T00:00:00Z","labels":[%s]%s}]\n' "$3" "$labels" "$parent" ;;
esac
`

func fakeBdCalls(t *testing.T, capture string) []string {
	t.Helper()
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			calls = append(calls, line)
		}
	}
	return calls
}

const federatedDemoCityTOML = "[workspace]\nname = \"demo\"\n\n[federation]\nidentity = \"citadel\"\n"

// TestGcBdCreateStampsTheCreatedBead is the acceptance table for the argv
// door of gp-0uj in its post-create shape: the argv reaches bd as typed (minus
// the gc-owned opt-out), and the bead bd reports created is labeled through
// the store unless it already carries an owner — its own, or its parent's.
func TestGcBdCreateStampsTheCreatedBead(t *testing.T) {
	const stamp = "update --json demo-1 --add-label owner:citadel"
	tests := []struct {
		name      string
		city      string
		args      []string
		labels    string // what show --json reports on the created bead
		createOut string // override bd's create output
		wantArgv  string
		wantStamp bool
		wantNote  string
	}{
		{"plain create is stamped", federatedDemoCityTOML, []string{"create", "t"}, "", "", "create t", true, ""},
		{"the new alias is a create", federatedDemoCityTOML, []string{"new", "t"}, "", "", "new t", true, ""},
		{"flags gc does not know are bd's business", federatedDemoCityTOML, []string{"create", "t", "-p1", "--future-flag", "x"}, "", "", "create t -p1 --future-flag x", true, ""},
		{"json output is parsed", federatedDemoCityTOML, []string{"create", "--json", "t"}, "", `{"id":"demo-1","title":"t"}`, "create --json t", true, ""},
		{"explicit owner respected", federatedDemoCityTOML, []string{"create", "t", "-l", "owner:jadegate"}, "owner:jadegate", "", "create t -l owner:jadegate", false, ""},
		{"inherited owner respected", federatedDemoCityTOML, []string{"create", "child", "--parent", "demo-0"}, "hold:mayor,owner:jadegate", "", "create child --parent demo-0", false, ""},
		{"opt-out stripped, no stamp", federatedDemoCityTOML, []string{"create", "t", "--no-owner-label"}, "", "", "create t", false, ""},
		{"inline false opt-out is not an opt-out", federatedDemoCityTOML, []string{"create", "t", "--no-owner-label=false"}, "", "", "create t", true, ""},
		{"dry run creates nothing", federatedDemoCityTOML, []string{"create", "t", "--dry-run"}, "", "⚠ [DRY RUN] Would create issue:\n  ID: demo-1\n  Title: t", "create t --dry-run", false, ""},
		{"identity unset is byte-identical and silent", "[workspace]\nname = \"demo\"\n", []string{"create", "t", "-l", "foo"}, "", "", "create t -l foo", false, ""},
		{"no id in the output is said out loud", federatedDemoCityTOML, []string{"create", "t", "--silent"}, "", "created something, format unknown", "create t --silent", false, bdOwnerLabelNotAppliedNotice},
		{"a hidden verb is never stamped, only said", federatedDemoCityTOML, []string{"--future-flag", "x", "create", "t"}, "", "", "--future-flag x create t", false, bdOwnerLabelHiddenVerbNotice},
		{"a value that spells the opt-out is a value", federatedDemoCityTOML, []string{"create", "t", "--description", "--no-owner-label"}, "", "", "create t --description --no-owner-label", true, ""},
		{"an explicit owner beats an inherited one", federatedDemoCityTOML, []string{"create", "child", "--parent", "demo-0", "-l", "owner:boomtown"}, "owner:boomtown,owner:jadegate", "", "create child --parent demo-0 -l owner:boomtown", false, ""},
		{"a shorthand cluster before the verb is stamped", federatedDemoCityTOML, []string{"-vv", "create", "t"}, "", "", "-vv create t", true, ""},
		{"an attached shorthand value before the verb is stamped", federatedDemoCityTOML, []string{"-C/tmp", "create", "t"}, "", "", "-C/tmp create t", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture := filepath.Join(t.TempDir(), "gc-bd-args.txt")
			fakeBdCityTestSetup(t, tt.city, createFakeBd)
			t.Setenv("CAPTURE_PATH", capture)
			t.Setenv("LABELS", tt.labels)
			t.Setenv("CREATE_OUT", tt.createOut)
			t.Setenv("CREATE_EXIT", "")
			parent := ""
			if slices.Contains(tt.args, "--parent") {
				parent = "demo-0"
			}
			t.Setenv("PARENT", parent)

			var stdout, stderr bytes.Buffer
			if got := doBd(tt.args, &stdout, &stderr); got != 0 {
				t.Fatalf("doBd(%v) = %d, want 0; stderr=%q", tt.args, got, stderr.String())
			}
			calls := fakeBdCalls(t, capture)
			if len(calls) == 0 || calls[0] != tt.wantArgv {
				t.Fatalf("bd received %q first, want %q", calls, tt.wantArgv)
			}
			stamped := false
			for _, c := range calls[1:] {
				if c == stamp {
					stamped = true
				}
			}
			if stamped != tt.wantStamp {
				t.Fatalf("stamp write present = %v, want %v; calls=%q", stamped, tt.wantStamp, calls)
			}
			if strings.Contains(tt.labels, "owner:boomtown") {
				if joined := strings.Join(calls, "\n"); !strings.Contains(joined, "update --json demo-1 --remove-label owner:jadegate") {
					t.Fatalf("calls = %q, want the inherited owner removed", calls)
				}
			}
			if tt.wantNote == "" && strings.Contains(stderr.String(), "owner label") {
				t.Fatalf("unexpected owner-label notice: %q", stderr.String())
			}
			if tt.wantNote != "" && !strings.Contains(stderr.String(), tt.wantNote) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tt.wantNote)
			}
			// bd's own output still reaches the operator untouched (the fake
			// only prints when it actually runs its create branch).
			if tt.createOut == "" && (tt.args[0] == "create" || tt.args[0] == "new") && !strings.Contains(stdout.String(), "Created issue: demo-1") {
				t.Fatalf("stdout = %q, want bd's create line passed through", stdout.String())
			}
		})
	}
}

// TestGcBdCreateStampsEveryBeadOfABatch: a --file or --graph create names
// several beads; each one is labeled on its own terms.
func TestGcBdCreateStampsEveryBeadOfABatch(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "gc-bd-args.txt")
	fakeBdCityTestSetup(t, federatedDemoCityTOML, createFakeBd)
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("LABELS", "")
	t.Setenv("CREATE_OUT", "✓ Created 2 issues from issues.md:\n  demo-1: First [P2, task]\n  demo-2: Second [P1, bug]")

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"create", "--file", "issues.md"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd = %d, want 0; stderr=%q", got, stderr.String())
	}
	calls := strings.Join(fakeBdCalls(t, capture), "\n")
	for _, want := range []string{"update --json demo-1 --add-label owner:citadel", "update --json demo-2 --add-label owner:citadel"} {
		if !strings.Contains(calls, want) {
			t.Fatalf("calls = %q, want %q", calls, want)
		}
	}
}

// TestGcBdCreateSaysSoWhenAFailedCreateNamesNoBead: a create that exited
// non-zero and whose output names no bead is said out loud too — a batch can
// persist beads before failing, in a shape gc does not read — and the exit
// code stays bd's. Nothing is stamped: there is no id to stamp.
func TestGcBdCreateSaysSoWhenAFailedCreateNamesNoBead(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "gc-bd-args.txt")
	fakeBdCityTestSetup(t, federatedDemoCityTOML, createFakeBd)
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("LABELS", "")
	t.Setenv("CREATE_OUT", "created something, format unknown")
	t.Setenv("CREATE_EXIT", "3")

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"create", "--file", "issues.md"}, &stdout, &stderr); got != 3 {
		t.Fatalf("doBd = %d, want bd's exit code 3; stderr=%q", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), bdOwnerLabelNotAppliedOnFailureNotice) {
		t.Fatalf("stderr = %q, want %q", stderr.String(), bdOwnerLabelNotAppliedOnFailureNotice)
	}
	if calls := strings.Join(fakeBdCalls(t, capture), "\n"); strings.Contains(calls, "update") {
		t.Fatalf("calls = %q, want no stamp write without a created id", calls)
	}
}

// TestGcBdCreateStampsWhatBdReportedEvenOnFailure: a batch that persisted
// some beads and then failed still names them; they are labeled and the
// exit code stays bd's.
func TestGcBdCreateStampsWhatBdReportedEvenOnFailure(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "gc-bd-args.txt")
	fakeBdCityTestSetup(t, federatedDemoCityTOML, createFakeBd)
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("LABELS", "")
	t.Setenv("CREATE_OUT", "✓ Created issue: demo-1 — first")
	t.Setenv("CREATE_EXIT", "3")

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"create", "--file", "issues.md"}, &stdout, &stderr); got != 3 {
		t.Fatalf("doBd = %d, want bd's exit code 3; stderr=%q", got, stderr.String())
	}
	if calls := strings.Join(fakeBdCalls(t, capture), "\n"); !strings.Contains(calls, "update --json demo-1 --add-label owner:citadel") {
		t.Fatalf("calls = %q, want the reported bead labeled despite the failure", calls)
	}
}
