package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
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
		// gf-d7b (the gf-c7n re-gate's Minor at cmd_bd_owner_label.go:136): a
		// repeated opt-out resolves as every other boolean flag does — the
		// last occurrence wins — so an explicit final =false restores the label.
		{"a repeated opt-out resolves to its last value (false last)", []string{"create", "t", "--no-owner-label", "--no-owner-label=false"}, []string{"create", "t"}, false},
		{"a repeated opt-out resolves to its last value (bare last)", []string{"create", "t", "--no-owner-label=false", "--no-owner-label"}, []string{"create", "t"}, true},
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
	hidden := [][]string{
		{"--future-flag", "x", "create", "t"},
		{"--future-flag", "x", "list"},
		{"-x", "create", "t"},
		{"-vx", "create", "t"},
		{"-vxC/tmp", "create", "t"},
		// A bare `=` after a boolean shorthand is not an inline value: pflag
		// reads the `=` as the next shorthand, one it does not know.
		{"-v=", "create", "t"},
		{"-vv=", "create", "t"},
	}
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
		{"title text is not a success line", "  Title: Fix Created issue: hw-old\n", nil},
		{"batch title contains a success line", "✓ Created 1 issues from issues.md:\n  hw-new: Fix Created issue: hw-old [P2, task]\n", []string{"hw-new"}},
		{"batch title contains a listing header", "✓ Created 2 issues from issues.md:\n  hw-new: Created 1 issues from other.md: [P2, task]\n  hw-next: Next [P2, task]\n", []string{"hw-new", "hw-next"}},
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
    # bd 1.3.1 applies --labels to the bead(s) it creates (and dedups them
    # against a parent's copied labels); the created bead's show reports them
    # below. IGNORE_CREATE_LABELS=1 plays a bd that dropped the flag.
    if [ "${IGNORE_CREATE_LABELS:-}" = "" ]; then
      for a in "$@"; do case "$a" in --labels=*) printf '%s\n' "${a#--labels=}" >> "${CAPTURE_PATH}.labels" ;; esac; done
    fi
    # A batch create reads its rows from the LAST --graph / --file value (pflag);
    # the file bd would read is kept for the test (gc removes its copy after).
    for a in "$@"; do case "$a" in --graph=*) cat "${a#--graph=}" > "${CAPTURE_PATH}.plan" ;; --file=*) cat "${a#--file=}" > "${CAPTURE_PATH}.md" ;; esac; done
    if [ "${CREATE_OUT:-}" != "" ]; then printf '%s\n' "${CREATE_OUT}"; else printf '\n✓ Created issue: demo-1 — t\n  Type:     task\n'; fi
    exit "${CREATE_EXIT:-0}" ;;
  show)
    # bd show resolves a unique abbreviation to the bead it abbreviates and
    # reports THAT id (utils.ResolvePartialID); PARENT_ABBREV is one for demo-0.
    if [ "${PARENT_ABBREV:-}" != "" ] && [ "$3" = "${PARENT_ABBREV}" ]; then set -- "$1" "$2" demo-0; fi
    if [ "$3" = "demo-0" ] && [ "${PARENT_UNREADABLE:-}" != "" ]; then printf 'Error: dolt server unavailable\n' >&2; exit 1; fi
    labels=""
    src="${LABELS:-}"
    if [ "$3" = "demo-0" ]; then src="hold:mayor,owner:jadegate"; fi
    if [ "$3" != "demo-0" ] && [ -f "${CAPTURE_PATH}.labels" ]; then rec=$(paste -sd, "${CAPTURE_PATH}.labels"); src="${src:+$src,}$rec"; fi
    if [ "$src" != "" ]; then labels=$(printf '%s' "$src" | awk -F, '{for(i=1;i<=NF;i++){if(!seen[$i]++){printf "%s\"%s\"", (n++?",":""), $i}}}'); fi
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
// door of gp-0uj in its create-time shape (gf-pu2 finding 1g): the argv
// reaches bd as typed (minus the gc-owned opt-out) plus the owner label the
// create carries — this city's, or the parent's for a child of another
// city's bead — inserted right after the verb, so bd never publishes the bead
// unowned; the bead bd reports created is then read back and repaired only
// where the create could not carry the label (a batch file gc cannot read)
// or bd ignored it. An owner the operator named is respected and nothing is
// carried.
func TestGcBdCreateStampsTheCreatedBead(t *testing.T) {
	const stamp = "update --json demo-1 --add-label owner:citadel"
	tests := []struct {
		name         string
		city         string
		args         []string
		labels       string // what show --json reports on the created bead
		createOut    string // override bd's create output
		wantArgv     string
		wantStamp    bool
		wantNote     string
		ignoreLabels bool // a bd that dropped the carried flag
	}{
		{"plain create carries the label, no follow-up write", federatedDemoCityTOML, []string{"create", "t"}, "", "", "create --labels=owner:citadel t", false, "", false},
		{"the new alias is a create", federatedDemoCityTOML, []string{"new", "t"}, "", "", "new --labels=owner:citadel t", false, "", false},
		{"flags gc does not know are bd's business", federatedDemoCityTOML, []string{"create", "t", "-p1", "--future-flag", "x"}, "", "", "create --labels=owner:citadel t -p1 --future-flag x", false, "", false},
		{"json output is parsed", federatedDemoCityTOML, []string{"create", "--json", "t"}, "", `{"id":"demo-1","title":"t"}`, "create --labels=owner:citadel --json t", false, "", false},
		{"explicit owner respected, nothing carried", federatedDemoCityTOML, []string{"create", "t", "-l", "owner:jadegate"}, "owner:jadegate", "", "create t -l owner:jadegate", false, "", false},
		{"explicit owner in a comma list respected", federatedDemoCityTOML, []string{"create", "t", "--labels", "pool:x,owner:jadegate"}, "pool:x,owner:jadegate", "", "create t --labels pool:x,owner:jadegate", false, "", false},
		{"explicit owner attached to the shorthand respected", federatedDemoCityTOML, []string{"create", "t", "-lowner:jadegate"}, "owner:jadegate", "", "create t -lowner:jadegate", false, "", false},
		{"explicit owner attached to the shorthand with = respected", federatedDemoCityTOML, []string{"create", "t", "-l=owner:jadegate"}, "owner:jadegate", "", "create t -l=owner:jadegate", false, "", false},
		{"explicit owner behind a boolean shorthand in a cluster respected", federatedDemoCityTOML, []string{"create", "t", "-qlowner:jadegate"}, "owner:jadegate", "", "create t -qlowner:jadegate", false, "", false},
		{"explicit owner in a quoted CSV field respected", federatedDemoCityTOML, []string{"create", "t", "--labels", "\"owner:jadegate\",pool:x"}, "owner:jadegate,pool:x", "", "create t --labels \"owner:jadegate\",pool:x", false, "", false},
		{"no-inherit-labels makes the child this city's", federatedDemoCityTOML, []string{"create", "child", "--parent", "demo-0", "--no-inherit-labels"}, "", "", "create --labels=owner:citadel child --parent demo-0 --no-inherit-labels", false, "", false},
		{"no-inherit-labels=false keeps the parent's lane", federatedDemoCityTOML, []string{"create", "child", "--parent", "demo-0", "--no-inherit-labels=false"}, "hold:mayor,owner:jadegate", "", "create --labels=owner:jadegate child --parent demo-0 --no-inherit-labels=false", false, "", false},
		{"a repeated inheritance boolean resolves to its last value", federatedDemoCityTOML, []string{"create", "child", "--parent=demo-0", "--no-inherit-labels", "--no-inherit-labels=false"}, "hold:mayor,owner:jadegate", "", "create --labels=owner:jadegate child --parent=demo-0 --no-inherit-labels --no-inherit-labels=false", false, "", false},
		{"a description alias value that spells a parent is a description", federatedDemoCityTOML, []string{"create", "t", "--body", "--parent=p-1"}, "", "", "create --labels=owner:citadel t --body --parent=p-1", false, "", false},
		{"the -m alias value that spells a parent is a description", federatedDemoCityTOML, []string{"create", "t", "-m", "--parent=p-1"}, "", "", "create --labels=owner:citadel t -m --parent=p-1", false, "", false},
		{"a child of a foreign parent carries the parent's owner", federatedDemoCityTOML, []string{"create", "child", "--parent", "demo-0"}, "hold:mayor,owner:jadegate", "", "create --labels=owner:jadegate child --parent demo-0", false, "", false},
		{"a child of a foreign parent carries the parent's owner (inline parent)", federatedDemoCityTOML, []string{"create", "child", "--parent=demo-0"}, "hold:mayor,owner:jadegate", "", "create --labels=owner:jadegate child --parent=demo-0", false, "", false},
		// gf-d7b finding 1n (the gf-c7n re-gate's Major at
		// cmd_bd_owner_label.go:375): bd's create also makes the new bead a
		// child through `--deps parent-child:<id>` (cmd/bd create_deps.go
		// parseDepSpec; create_atomic.go addDepSpecEdges), in every spelling
		// bd accepts — inline or as the next token, one of a CSV list, a
		// repeated flag, whitespace around the type and the id — and such a
		// child of a foreign parent carries the PARENT's owner, read first.
		{"a child under a foreign parent through --deps parent-child carries the parent's owner", federatedDemoCityTOML, []string{"create", "child", "--deps", "parent-child:demo-0"}, "hold:mayor,owner:jadegate", "", "create --labels=owner:jadegate child --deps parent-child:demo-0", false, "", false},
		{"a --deps parent-child parent given inline", federatedDemoCityTOML, []string{"create", "child", "--deps=parent-child:demo-0"}, "hold:mayor,owner:jadegate", "", "create --labels=owner:jadegate child --deps=parent-child:demo-0", false, "", false},
		{"a --deps parent-child parent inside a CSV list", federatedDemoCityTOML, []string{"create", "child", "--deps", "blocked-by:demo-9,parent-child:demo-0"}, "hold:mayor,owner:jadegate", "", "create --labels=owner:jadegate child --deps blocked-by:demo-9,parent-child:demo-0", false, "", false},
		{"a --deps parent-child parent on a repeated flag", federatedDemoCityTOML, []string{"create", "child", "--deps", "demo-9", "--deps", "parent-child:demo-0"}, "hold:mayor,owner:jadegate", "", "create --labels=owner:jadegate child --deps demo-9 --deps parent-child:demo-0", false, "", false},
		{"a --deps parent-child parent with bd's whitespace trimming", federatedDemoCityTOML, []string{"create", "child", "--deps", "parent-child: demo-0"}, "hold:mayor,owner:jadegate", "", "create --labels=owner:jadegate child --deps parent-child: demo-0", false, "", false},
		{"--parent and a --deps parent-child naming the same parent agree", federatedDemoCityTOML, []string{"create", "child", "--parent", "demo-0", "--deps", "parent-child:demo-0"}, "hold:mayor,owner:jadegate", "", "create --labels=owner:jadegate child --parent demo-0 --deps parent-child:demo-0", false, "", false},
		{"no-inherit-labels makes a --deps parent-child child this city's", federatedDemoCityTOML, []string{"create", "child", "--deps", "parent-child:demo-0", "--no-inherit-labels"}, "", "", "create --labels=owner:citadel child --deps parent-child:demo-0 --no-inherit-labels", false, "", false},
		{"a --deps edge of another type is not a parent", federatedDemoCityTOML, []string{"create", "t", "--deps", "blocked-by:demo-0"}, "", "", "create --labels=owner:citadel t --deps blocked-by:demo-0", false, "", false},
		{"a bare --deps id is not a parent", federatedDemoCityTOML, []string{"create", "t", "--deps", "demo-0"}, "", "", "create --labels=owner:citadel t --deps demo-0", false, "", false},
		{"a value that spells a --deps parent is a value", federatedDemoCityTOML, []string{"create", "t", "--description", "--deps=parent-child:demo-0"}, "", "", "create --labels=owner:citadel t --description --deps=parent-child:demo-0", false, "", false},
		// The lane's codex round 2 on 1n: bd's create resolves every --deps
		// target through its partial-id resolver before wiring the edge, so
		// an abbreviation that uniquely names the foreign parent is that
		// parent — read as bd resolves it — and the argv reaches bd as typed.
		{"an abbreviated --deps parent-child target is read as bd resolves it", federatedDemoCityTOML, []string{"create", "child", "--deps", "parent-child:d0"}, "hold:mayor,owner:jadegate", "", "create --labels=owner:jadegate child --deps parent-child:d0", false, "", false},
		{"an external --deps parent-child target is bd's to wire and reads nothing", federatedDemoCityTOML, []string{"create", "t", "--deps", "parent-child:external:gh-1"}, "", "", "create --labels=owner:citadel t --deps parent-child:external:gh-1", false, "", false},
		// The re-gate's Minor at cmd_bd_owner_label.go:136: the last opt-out wins.
		{"a repeated opt-out resolves to its last value (false last)", federatedDemoCityTOML, []string{"create", "t", "--no-owner-label", "--no-owner-label=false"}, "", "", "create --labels=owner:citadel t", false, "", false},
		{"a repeated opt-out resolves to its last value (bare last)", federatedDemoCityTOML, []string{"create", "t", "--no-owner-label=false", "--no-owner-label"}, "", "", "create t", false, "", false},
		{"opt-out stripped, nothing carried, no stamp", federatedDemoCityTOML, []string{"create", "t", "--no-owner-label"}, "", "", "create t", false, "", false},
		{"inline false opt-out is not an opt-out", federatedDemoCityTOML, []string{"create", "t", "--no-owner-label=false"}, "", "", "create --labels=owner:citadel t", false, "", false},
		{"dry run creates nothing and carries nothing", federatedDemoCityTOML, []string{"create", "t", "--dry-run"}, "", "⚠ [DRY RUN] Would create issue:\n  ID: demo-1\n  Title: t", "create t --dry-run", false, "", false},
		{"identity unset is byte-identical and silent", "[workspace]\nname = \"demo\"\n", []string{"create", "t", "-l", "foo"}, "", "", "create t -l foo", false, "", false},
		{"no id in the output needs no notice when the create carried the label", federatedDemoCityTOML, []string{"create", "t", "--silent"}, "", "created something, format unknown", "create --labels=owner:citadel t --silent", false, "", false},
		{"a hidden verb is never stamped, only said", federatedDemoCityTOML, []string{"--future-flag", "x", "create", "t"}, "", "", "--future-flag x create t", false, bdOwnerLabelHiddenVerbNotice, false},
		{"a value that spells the opt-out is a value", federatedDemoCityTOML, []string{"create", "t", "--description", "--no-owner-label"}, "", "", "create --labels=owner:citadel t --description --no-owner-label", false, "", false},
		{"an explicit owner beats an inherited one", federatedDemoCityTOML, []string{"create", "child", "--parent", "demo-0", "-l", "owner:boomtown"}, "owner:boomtown,owner:jadegate", "", "create child --parent demo-0 -l owner:boomtown", false, "", false},
		{"a shorthand cluster before the verb carries the label", federatedDemoCityTOML, []string{"-vv", "create", "t"}, "", "", "-vv create --labels=owner:citadel t", false, "", false},
		{"an attached shorthand value before the verb carries the label", federatedDemoCityTOML, []string{"-C/tmp", "create", "t"}, "", "", "-C/tmp create --labels=owner:citadel t", false, "", false},
		{"the label precedes a -- terminator", federatedDemoCityTOML, []string{"create", "--", "t"}, "", "", "create --labels=owner:citadel -- t", false, "", false},
		// The batch files below do not exist in the scope: a batch gc cannot
		// read reaches bd as typed and is repaired after (the batches gc CAN
		// read carry the owner in the file: TestGcBdCreateCarriesTheOwnerInABatch).
		{"a graph plan gc cannot read is handed over as typed and repaired after", federatedDemoCityTOML, []string{"create", "--graph", "plan.json"}, "", "Created 1 issues\n  root -> demo-1", "create --graph plan.json", true, "", false},
		{"a markdown batch gc cannot read is handed over as typed and repaired after", federatedDemoCityTOML, []string{"create", "--file", "issues.md"}, "", "✓ Created 1 issues from issues.md:\n  demo-1: First [P2, task]", "create --file issues.md", true, "", false},
		{"a markdown batch by shorthand is seen as a batch", federatedDemoCityTOML, []string{"create", "-f", "issues.md"}, "", "✓ Created 1 issues from issues.md:\n  demo-1: First [P2, task]", "create -f issues.md", true, "", false},
		{"a markdown batch behind a boolean shorthand in a cluster is seen", federatedDemoCityTOML, []string{"create", "-qf", "issues.md"}, "", "✓ Created 1 issues from issues.md:\n  demo-1: First [P2, task]", "create -qf issues.md", true, "", false},
		{"a bd that dropped the carried label is repaired after", federatedDemoCityTOML, []string{"create", "t"}, "", "", "create --labels=owner:citadel t", true, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture := filepath.Join(t.TempDir(), "gc-bd-args.txt")
			fakeBdCityTestSetup(t, tt.city, createFakeBd)
			t.Setenv("CAPTURE_PATH", capture)
			t.Setenv("LABELS", tt.labels)
			t.Setenv("CREATE_OUT", tt.createOut)
			t.Setenv("CREATE_EXIT", "")
			t.Setenv("PARENT_UNREADABLE", "")
			t.Setenv("PARENT_ABBREV", "d0")
			if tt.ignoreLabels {
				t.Setenv("IGNORE_CREATE_LABELS", "1")
			} else {
				t.Setenv("IGNORE_CREATE_LABELS", "")
			}
			parent := ""
			if slices.ContainsFunc(tt.args, func(a string) bool {
				return strings.HasPrefix(a, "--parent") || (strings.Contains(a, "parent-child") && (strings.Contains(a, "demo-0") || strings.HasSuffix(a, ":d0")))
			}) {
				parent = "demo-0"
			}
			t.Setenv("PARENT", parent)

			var stdout, stderr bytes.Buffer
			if got := doBd(tt.args, &stdout, &stderr); got != 0 {
				t.Fatalf("doBd(%v) = %d, want 0; stderr=%q", tt.args, got, stderr.String())
			}
			calls := fakeBdCalls(t, capture)
			// The create is the first call that is not a read: a child's
			// parent is read before it, for the owner it carries.
			createAt := slices.IndexFunc(calls, func(c string) bool { return !strings.HasPrefix(c, "show ") })
			if createAt < 0 || calls[createAt] != tt.wantArgv {
				t.Fatalf("bd received %q, want the create %q", calls, tt.wantArgv)
			}
			if parent != "" && createAt == 0 && strings.Contains(tt.wantArgv, "--labels=owner:jadegate") {
				t.Fatalf("the parent's owner was carried without reading the parent first: calls=%q", calls)
			}
			if createAt != 0 && strings.Contains(tt.wantArgv, "--labels=owner:citadel") {
				t.Fatalf("a create carrying this city's owner read a parent first: calls=%q", calls)
			}
			stamped := false
			for _, c := range calls[createAt+1:] {
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

// TestGcBdCreateStampsEveryBeadOfABatch: a --file create names several
// beads; when the batch's rows carry the owner (the markdown rewrite) but bd
// came back with beads that carry none (a bd that dropped the per-issue
// labels), each bead is repaired on its own terms after the create, as before.
func TestGcBdCreateStampsEveryBeadOfABatch(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "gc-bd-args.txt")
	fakeBdCityTestSetup(t, federatedDemoCityTOML, createFakeBd)
	if err := os.WriteFile(filepath.Join(os.Getenv("GC_CITY_PATH"), "issues.md"), []byte("## First\n\n## Second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("LABELS", "")
	t.Setenv("CREATE_OUT", "✓ Created 2 issues from issues.md:\n  demo-1: Fix Created issue: demo-old [P2, task]\n  demo-2: Created 1 issues from other.md: [P1, bug]")

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"create", "--file", "issues.md"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd = %d, want 0; stderr=%q", got, stderr.String())
	}
	calls := fakeBdCalls(t, capture)
	if len(calls) == 0 || !strings.HasPrefix(calls[0], "create --file issues.md --file=") {
		t.Fatalf("bd received %q, want the batch create pointed at the rewritten file first", calls)
	}
	joined := strings.Join(calls, "\n")
	if strings.Contains(joined, "demo-old") {
		t.Fatalf("title redirected owner stamping: %q", calls)
	}
	for _, want := range []string{"update --json demo-1 --add-label owner:citadel", "update --json demo-2 --add-label owner:citadel"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("calls = %q, want %q", calls, want)
		}
	}
}

// TestGcBdCreateSaysSoWhenAFailedCreateNamesNoBead: a create that could not
// carry the label (a batch whose file gc could not read), exited non-zero and
// whose output names no bead is said out loud — a batch can persist beads
// before failing, in a shape gc does not read — and the exit code stays
// bd's. Nothing is stamped: there is no id to stamp. A create that carried
// the label needs no notice: whatever bd persisted carries it.
func TestGcBdCreateSaysSoWhenAFailedCreateNamesNoBead(t *testing.T) {
	for _, tt := range []struct {
		name     string
		args     []string
		wantNote bool
	}{
		{"an unreadable graph plan carried nothing", []string{"create", "--graph", "plan.json"}, true},
		{"an unreadable markdown batch carried nothing", []string{"create", "--file", "issues.md"}, true},
		{"a single create carried the label", []string{"create", "t", "--silent"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			capture := filepath.Join(t.TempDir(), "gc-bd-args.txt")
			fakeBdCityTestSetup(t, federatedDemoCityTOML, createFakeBd)
			t.Setenv("CAPTURE_PATH", capture)
			t.Setenv("LABELS", "")
			t.Setenv("CREATE_OUT", "created something, format unknown")
			t.Setenv("CREATE_EXIT", "3")

			var stdout, stderr bytes.Buffer
			if got := doBd(tt.args, &stdout, &stderr); got != 3 {
				t.Fatalf("doBd = %d, want bd's exit code 3; stderr=%q", got, stderr.String())
			}
			if strings.Contains(stderr.String(), bdOwnerLabelNotAppliedOnFailureNotice) != tt.wantNote {
				t.Fatalf("stderr = %q, notice present = %v, want %v", stderr.String(), !tt.wantNote, tt.wantNote)
			}
			if calls := strings.Join(fakeBdCalls(t, capture), "\n"); strings.Contains(calls, "update") {
				t.Fatalf("calls = %q, want no stamp write without a created id", calls)
			}
		})
	}
}

// TestGcBdCreateStampsWhatBdReportedEvenOnFailure: a batch that persisted
// some beads and then failed still names them; a batch gc could not read
// carries nothing, so each is repaired, and the exit code stays bd's.
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
	calls := strings.Join(fakeBdCalls(t, capture), "\n")
	if !strings.Contains(calls, "create --file issues.md") {
		t.Fatalf("calls = %q, want the batch create as typed", calls)
	}
	if !strings.Contains(calls, "update --json demo-1 --add-label owner:citadel") {
		t.Fatalf("calls = %q, want the reported bead labeled despite the failure", calls)
	}
}

// TestGcBdCreateRefusesAnUnreadableParent: a child's owner is its parent's,
// read before the create; a parent that cannot be read is not an unowned
// one (finding 1d), so the create is refused — nothing reaches bd's create.
func TestGcBdCreateRefusesAnUnreadableParent(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "gc-bd-args.txt")
	fakeBdCityTestSetup(t, federatedDemoCityTOML, createFakeBd)
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("LABELS", "")
	t.Setenv("CREATE_OUT", "")
	t.Setenv("PARENT", "demo-0")
	t.Setenv("PARENT_UNREADABLE", "1")

	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"create", "child", "--parent", "demo-0"}, &stdout, &stderr); got != 1 {
		t.Fatalf("doBd = %d, want 1 (refused); stderr=%q", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "refusing to create") || !strings.Contains(stderr.String(), "demo-0") {
		t.Fatalf("stderr = %q, want a refusal naming the parent", stderr.String())
	}
	for _, c := range fakeBdCalls(t, capture) {
		if !strings.HasPrefix(c, "show ") {
			t.Fatalf("bd ran %q; nothing but the parent read may run for a refused create", c)
		}
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing from a refused create", stdout.String())
	}
}

// TestGcBdCreateRefusesParentsInDifferentLanes (gf-d7b finding 1n): a single
// create whose parents — the --parent and every --deps parent-child — sit in
// different lanes names no one lane, as a plan node's parents do (finding 1l,
// beads.GraphPlanOwnerLabels): the create is refused before bd runs. An
// unowned parent is in THIS city's lane. A --deps parent-child parent that
// cannot be read refuses the create like an unreadable --parent (finding 1d).
func TestGcBdCreateRefusesParentsInDifferentLanes(t *testing.T) {
	for _, tt := range []struct {
		name       string
		labels     string // what show reports on every parent but demo-0 (owner:jadegate)
		unreadable string
		args       []string
		want       string
	}{
		{"--parent in one lane, a --deps parent-child parent in another", "owner:boomtown", "", []string{"create", "child", "--parent", "demo-0", "--deps", "parent-child:demo-2"}, "different lanes"},
		{"two --deps parent-child parents in different lanes", "owner:boomtown", "", []string{"create", "child", "--deps", "parent-child:demo-0,parent-child:demo-2"}, "different lanes"},
		{"a foreign parent beside an unowned one (this city's lane)", "", "", []string{"create", "child", "--parent", "demo-0", "--deps", "parent-child:demo-3"}, "different lanes"},
		{"a --deps parent-child parent that cannot be read", "", "1", []string{"create", "child", "--deps", "parent-child:demo-0"}, "demo-0"},
		// bd's create takes --parent literally (no partial-id resolution), so
		// an abbreviation there is a read that resolved to another bead: the
		// refusal the exact-id guard always gave (not a parent bd would wire).
		{"an abbreviated --parent is exact to bd and refused when bd resolves it elsewhere", "", "", []string{"create", "child", "--parent", "d0"}, `"d0"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			capture := filepath.Join(t.TempDir(), "gc-bd-args.txt")
			fakeBdCityTestSetup(t, federatedDemoCityTOML, createFakeBd)
			t.Setenv("CAPTURE_PATH", capture)
			t.Setenv("LABELS", tt.labels)
			t.Setenv("CREATE_OUT", "")
			t.Setenv("PARENT", "demo-0")
			t.Setenv("PARENT_UNREADABLE", tt.unreadable)
			t.Setenv("PARENT_ABBREV", "d0")

			var stdout, stderr bytes.Buffer
			if got := doBd(tt.args, &stdout, &stderr); got != 1 {
				t.Fatalf("doBd(%v) = %d, want 1 (refused); stderr=%q", tt.args, got, stderr.String())
			}
			if !strings.Contains(stderr.String(), "refusing to create") || !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("stderr = %q, want a refusal containing %q", stderr.String(), tt.want)
			}
			for _, c := range fakeBdCalls(t, capture) {
				if !strings.HasPrefix(c, "show ") {
					t.Fatalf("bd ran %q; nothing but the parent reads may run for a refused create", c)
				}
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want nothing from a refused create", stdout.String())
			}
		})
	}
}

// TestBdCreateParents pins the parent set a single create names, read the
// way bd's create wires it (gf-d7b finding 1n): the --parent value first,
// then every --deps entry of type parent-child in each spelling bd's
// parseDepSpec accepts (cmd/bd create_deps.go) — pflag's CSV split of each
// value, a repeated flag appending, the entry and its two halves TrimSpace'd
// around the first colon; a bare id or another type is no parent; a value
// that spells the flag is a value; after `--` everything is positional. A
// --deps parent is one bd RESOLVES (its partial-id resolver) before wiring,
// a --parent one bd takes literally; an external: target names no bead here.
func TestBdCreateParents(t *testing.T) {
	exact := func(id string) bdCreateParent { return bdCreateParent{ID: id} }
	dep := func(id string) bdCreateParent { return bdCreateParent{ID: id, Resolved: true} }
	for _, tt := range []struct {
		args []string
		want []bdCreateParent
	}{
		{[]string{"create", "t"}, nil},
		{[]string{"create", "t", "--parent", "p-1"}, []bdCreateParent{exact("p-1")}},
		{[]string{"create", "t", "--parent=p-1"}, []bdCreateParent{exact("p-1")}},
		{[]string{"create", "t", "--deps", "parent-child:p-1"}, []bdCreateParent{dep("p-1")}},
		{[]string{"create", "t", "--deps=parent-child:p-1"}, []bdCreateParent{dep("p-1")}},
		{[]string{"create", "t", "--deps", " parent-child : p-1 "}, []bdCreateParent{dep("p-1")}},
		{[]string{"create", "t", "--deps", "blocked-by:p-9,parent-child:p-1,discovered-from:p-8"}, []bdCreateParent{dep("p-1")}},
		{[]string{"create", "t", "--deps", "\"parent-child:p-1\",p-9"}, []bdCreateParent{dep("p-1")}},
		{[]string{"create", "t", "--deps", "p-9", "--deps", "parent-child:p-1", "--deps", "parent-child:p-2"}, []bdCreateParent{dep("p-1"), dep("p-2")}},
		{[]string{"create", "t", "--parent", "p-1", "--deps", "parent-child:p-2"}, []bdCreateParent{exact("p-1"), dep("p-2")}},
		{[]string{"create", "t", "--parent", "p-1", "--deps", "parent-child:p-1"}, []bdCreateParent{exact("p-1")}},
		{[]string{"create", "t", "--deps", "parent-child:a3f8"}, []bdCreateParent{dep("a3f8")}},
		{[]string{"create", "t", "--deps", "parent-child:external:gh-1"}, nil},
		{[]string{"create", "t", "--deps", "p-1"}, nil},
		{[]string{"create", "t", "--deps", "blocks:p-1"}, nil},
		{[]string{"create", "t", "--deps", "parent-child:"}, nil},
		{[]string{"create", "t", "--description", "--deps=parent-child:p-1"}, nil},
		{[]string{"create", "t", "--", "--deps", "parent-child:p-1"}, nil},
		{[]string{"create", "t", "--deps"}, nil},
	} {
		if got := bdCreateParents(tt.args); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("bdCreateParents(%q) = %+v, want %+v", tt.args, got, tt.want)
		}
	}
}

// TestBdCreateOwnerLabelResolvesADepsParentThroughBd (the lane's codex round
// 3 on finding 1n): the store's live handle reads an id EXACTLY whatever the
// store — the native Dolt and DoltLite readers answer an abbreviation with a
// plain not-found, never the bd-backed store's collision report — so a
// --deps target is resolved first through the scoped bd's own resolver, as
// bd's create resolves it, and the id bd names is the one read. A --parent
// is read as typed, as bd's create wires it. A target bd cannot resolve
// refuses the create.
func TestBdCreateOwnerLabelResolvesADepsParentThroughBd(t *testing.T) {
	exactRead := func(id string) (beads.Bead, error) {
		if id == "demo-0" {
			return beads.Bead{ID: "demo-0", Labels: []string{"hold:mayor", "owner:jadegate"}}, nil
		}
		return beads.Bead{}, beads.ErrNotFound // an exact reader: an abbreviation is nobody
	}
	var asked []string
	resolve := func(id string) (string, error) {
		asked = append(asked, id)
		if id == "d0" {
			return "demo-0", nil
		}
		return "", errors.New("no issue found matching " + id)
	}
	label, carry, err := bdCreateOwnerLabel(exactRead, resolve, []string{"create", "child", "--deps", "parent-child:d0"}, "owner:citadel")
	if err != nil || !carry || label != "owner:jadegate" {
		t.Fatalf("a --deps abbreviation: (%q, %v, %v), want owner:jadegate carried", label, carry, err)
	}
	if !reflect.DeepEqual(asked, []string{"d0"}) {
		t.Fatalf("bd was asked to resolve %q, want [d0]", asked)
	}
	// An exact id goes through bd's resolver too (bd's create resolves every
	// --deps target) and reads the same bead.
	asked = nil
	if label, carry, err := bdCreateOwnerLabel(exactRead, func(id string) (string, error) { asked = append(asked, id); return id, nil }, []string{"create", "child", "--deps", "parent-child:demo-0"}, "owner:citadel"); err != nil || !carry || label != "owner:jadegate" || !reflect.DeepEqual(asked, []string{"demo-0"}) {
		t.Fatalf("an exact --deps id: (%q, %v, %v) asked=%q", label, carry, err, asked)
	}
	// A --parent is never resolved: bd's create takes it as typed.
	asked = nil
	if _, _, err := bdCreateOwnerLabel(exactRead, resolve, []string{"create", "child", "--parent", "d0"}, "owner:citadel"); err == nil || len(asked) != 0 {
		t.Fatalf("an abbreviated --parent: err=%v asked=%q, want a refusal with nothing resolved", err, asked)
	}
	// A target bd cannot resolve refuses the create, naming it.
	if _, _, err := bdCreateOwnerLabel(exactRead, resolve, []string{"create", "child", "--deps", "parent-child:zz"}, "owner:citadel"); err == nil || !strings.Contains(err.Error(), `"zz"`) {
		t.Fatalf("an unresolvable --deps target: err=%v, want a refusal naming zz", err)
	}
	// With no resolver (a database override reads nothing anyway) the id is read as typed.
	if _, _, err := bdCreateOwnerLabel(exactRead, nil, []string{"create", "child", "--deps", "parent-child:d0"}, "owner:citadel"); err == nil {
		t.Fatalf("no resolver: err=%v, want the exact read's refusal", err)
	}
}

// TestBdResolveCreateDepTarget pins the resolver's bd call: `show --json
// <target>` positionally behind the create's leading globals (so the same
// database answers), the first row's id as the answer, bd's non-zero exit
// or an answer without a bead as the error.
func TestBdResolveCreateDepTarget(t *testing.T) {
	var got [][]string
	run := func(_ context.Context, args []string, stdout, stderr io.Writer) int {
		got = append(got, args)
		switch args[len(args)-1] {
		case "d0":
			io.WriteString(stdout, "[{\"id\":\"demo-0\",\"title\":\"t\",\"labels\":[\"owner:jadegate\"]}]\n") //nolint:errcheck // test
			return 0
		case "zz":
			io.WriteString(stderr, "Error: no issue found matching \"zz\"\n") //nolint:errcheck // test
			return 1
		default:
			io.WriteString(stdout, "[]\n") //nolint:errcheck // test
			return 0
		}
	}
	id, err := bdResolveCreateDepTarget(context.Background(), run, []string{"--actor", "bob"}, "d0")
	if err != nil || id != "demo-0" {
		t.Fatalf("resolve d0 = (%q, %v), want demo-0", id, err)
	}
	if want := []string{"--actor", "bob", "show", "--json", "d0"}; !reflect.DeepEqual(got[0], want) {
		t.Fatalf("bd ran %q, want %q", got[0], want)
	}
	if _, err := bdResolveCreateDepTarget(context.Background(), run, nil, "zz"); err == nil || !strings.Contains(err.Error(), "no issue found matching") {
		t.Fatalf("resolve zz: err=%v, want bd's stderr in the error", err)
	}
	if _, err := bdResolveCreateDepTarget(context.Background(), run, nil, "empty"); err == nil {
		t.Fatal("an answer without a bead must be an error")
	}
}

// TestBdCreateArgvWithLabel pins the shape of the carried label: inserted
// right after the verb, whatever precedes it, before positionals and a `--`;
// and the label flags bd reads are read the same way here.
func TestBdCreateArgvWithLabel(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"create", "t"}, "create --labels=owner:citadel t"},
		{[]string{"new", "t", "-p1"}, "new --labels=owner:citadel t -p1"},
		{[]string{"--actor", "bob", "create", "t"}, "--actor bob create --labels=owner:citadel t"},
		{[]string{"-vC/tmp", "create", "t"}, "-vC/tmp create --labels=owner:citadel t"},
		{[]string{"create", "--", "--not-a-flag"}, "create --labels=owner:citadel -- --not-a-flag"},
		{[]string{"--future-flag", "x", "create", "t"}, "--future-flag x create t"}, // hidden verb: unchanged
	} {
		if got := strings.Join(bdCreateArgvWithLabel(tt.args, "owner:citadel"), " "); got != tt.want {
			t.Errorf("bdCreateArgvWithLabel(%v) = %q, want %q", tt.args, got, tt.want)
		}
	}
	for _, tt := range []struct {
		args []string
		want []string
	}{
		{[]string{"create", "t", "-l", "a,b"}, []string{"a", "b"}},
		{[]string{"create", "t", "--labels=x", "--label", "y"}, []string{"x", "y"}},
		{[]string{"create", "t", "--description", "-l"}, nil},
		{[]string{"create", "t", "--", "-l", "z"}, nil},
		{[]string{"create", "t", "-lowner:jadegate"}, []string{"owner:jadegate"}},
		{[]string{"create", "t", "-l=a,b"}, []string{"a", "b"}},
		{[]string{"create", "t", "-qlowner:jadegate"}, []string{"owner:jadegate"}},
		{[]string{"create", "t", "-ql", "owner:jadegate"}, []string{"owner:jadegate"}},
		{[]string{"create", "t", "--labels", "\"owner:jadegate\",\"a,b\""}, []string{"owner:jadegate", "a,b"}},
		{[]string{"create", "t", "-qv", "-l", "x"}, []string{"x"}},
	} {
		if got := bdCreateLabelValues(tt.args); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("bdCreateLabelValues(%v) = %v, want %v", tt.args, got, tt.want)
		}
	}
	if got := bdCreateValueFlag([]string{"create", "t", "--parent", "demo-0"}, "--parent"); got != "demo-0" {
		t.Errorf("bdCreateValueFlag(--parent x) = %q", got)
	}
	if got := bdCreateValueFlag([]string{"create", "t", "--parent=demo-0"}, "--parent"); got != "demo-0" {
		t.Errorf("bdCreateValueFlag(--parent=x) = %q", got)
	}
	if got := bdCreateValueFlag([]string{"create", "t", "--description", "--parent"}, "--parent"); got != "" {
		t.Errorf("bdCreateValueFlag(a value spelling the flag) = %q, want empty", got)
	}
	if got := bdCreateValueFlag([]string{"create", "-qf", "issues.md"}, "-f"); got != "issues.md" {
		t.Errorf("bdCreateValueFlag(-qf issues.md) = %q, want the next token", got)
	}
	if !bdHasBoolFlag([]string{"create", "t", "-q", "--no-inherit-labels"}, "--no-inherit-labels") || bdHasBoolFlag([]string{"create", "t", "--no-inherit-labels=false"}, "--no-inherit-labels") {
		t.Error("bdHasBoolFlag must honor an inline false")
	}
	if bdHasBoolFlag([]string{"create", "t", "--dry-run", "--dry-run=false"}, "--dry-run") || !bdHasBoolFlag([]string{"create", "t", "--dry-run=false", "--dry-run"}, "--dry-run") {
		t.Error("bdHasBoolFlag must resolve a repeated flag to its last value")
	}
	for _, alias := range []string{"--body", "--message", "-m", "--description-file"} {
		if got := bdCreateValueFlag([]string{"create", "t", alias, "--parent=p-1"}, "--parent"); got != "" {
			t.Errorf("bdCreateValueFlag after %s = %q, want the alias's value skipped", alias, got)
		}
	}
}

// Database overrides reach bd unchanged, and must never redirect a follow-up
// owner write into the scope's default store using an unrelated local ID. A
// create without a parent still carries this city's label (no store is
// needed for that); a create under a parent carries nothing, because the
// parent cannot be read from the configured store, and says so.
func TestGcBdCreateDatabaseOverrideDoesNotStampDefaultStore(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		envKey   string
		wantArgv string
		wantNote bool
	}{
		{"database before verb", []string{"--database", "other", "create", "t"}, "", "--database other create --labels=owner:citadel t", false},
		{"database inline", []string{"create", "t", "--database=other"}, "", "create --labels=owner:citadel t --database=other", false},
		{"db before verb", []string{"--db", "other.db", "create", "t"}, "", "--db other.db create --labels=owner:citadel t", false},
		{"db inline", []string{"create", "t", "--db=other.db"}, "", "create --labels=owner:citadel t --db=other.db", false},
		{"BEADS_DB", []string{"create", "t"}, "BEADS_DB", "create --labels=owner:citadel t", false},
		{"BD_DB", []string{"create", "t"}, "BD_DB", "create --labels=owner:citadel t", false},
		{"a child under an override carries nothing and is said", []string{"create", "t", "--parent", "demo-0", "--database=other"}, "", "create t --parent demo-0 --database=other", true},
		{"repo selector", []string{"create", "t", "--repo", "/other/repo"}, "", "create --labels=owner:citadel t --repo /other/repo", false},
		{"a child under a repo selector carries nothing and is said", []string{"create", "t", "--repo", "/other/repo", "--parent", "other-123"}, "", "create t --repo /other/repo --parent other-123", true},
		{"global selector before verb", []string{"--global", "create", "t"}, "", "--global create --labels=owner:citadel t", false},
		{"a child under the global selector carries nothing and is said", []string{"--global", "create", "t", "--parent", "other-123"}, "", "--global create t --parent other-123", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture := filepath.Join(t.TempDir(), "gc-bd-args.txt")
			// Return a colliding local ID and record any follow-up read or write.
			fakeBdCityTestSetup(t, federatedDemoCityTOML, `#!/bin/sh
printf '%s\n' "$*" >> "${CAPTURE_PATH}"
printf '✓ Created issue: demo-1 — t\n'
`)
			t.Setenv("CAPTURE_PATH", capture)
			if tt.envKey != "" {
				t.Setenv(tt.envKey, "other.db")
			}
			var stdout, stderr bytes.Buffer
			if code := doBd(tt.args, &stdout, &stderr); code != 0 {
				t.Fatalf("doBd = %d; stderr=%q", code, stderr.String())
			}
			calls := fakeBdCalls(t, capture)
			if len(calls) != 1 || calls[0] != tt.wantArgv {
				t.Fatalf("bd calls = %q, want only the create %q", calls, tt.wantArgv)
			}
			if got := strings.Contains(stderr.String(), "owner label not applied") && strings.Contains(stderr.String(), "database"); got != tt.wantNote {
				t.Fatalf("stderr=%q, owner-label database-selection notice present = %v, want %v", stderr.String(), got, tt.wantNote)
			}
			if !strings.Contains(stdout.String(), "Created issue: demo-1") {
				t.Fatalf("create output lost: %q", stdout.String())
			}
		})
	}
}

func TestBdOwnerLabelDatabaseOverride(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  []string
		want bool
	}{
		{"server database override", []string{"create", "t"}, []string{"BEADS_DOLT_SERVER_DATABASE=other"}, true},
		{"projected empty override", []string{"create", "t"}, []string{"BEADS_DOLT_SERVER_DATABASE="}, false},
		{"scope directory is not an override", []string{"create", "t"}, []string{"BEADS_DIR=/city/.beads"}, false},
		{"description is not a flag", []string{"create", "t", "--description", "--database"}, nil, false},
		{"positional is not a flag", []string{"create", "--", "--db=other"}, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bdOwnerLabelDatabaseOverride(tt.args, tt.env); got != tt.want {
				t.Fatalf("database override = %v, want %v", got, tt.want)
			}
		})
	}
}
