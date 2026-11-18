package docsync

import (
	"fmt"
	"go/build"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

const verifySmoke = `(
  set -eu
  scratch=$(mktemp -d /var/tmp/gc-verify.XXXXXXXX)
  trap 'rm -rf "$scratch"' EXIT
  make build BUILD_DIR="$scratch/bin"
  env -i -C "$scratch" HOME="$scratch" ./bin/gc version
  env -i -C "$scratch" HOME="$scratch" ./bin/gc --help
)`

// These blocks have a different purpose from an unconditional Make command.
// Match the complete block so dropping a guard or masking its status fails.
const verifyLinter = `(
  set -eu
  . scripts/lib/go-toolchain.sh
  gc_go_toolchain "$PWD"
  version=$(awk '$1 == "GOLANGCI_LINT_VERSION" {print $3}' Makefile)
  compiler=$(go env GOVERSION)
  linter="$(go env GOPATH)/bin/gascity-tools/golangci-lint-v$version-$compiler/golangci-lint"
  test -x "$linter"
  go version -m "$linter" | awk -v compiler="$compiler" -v version="v$version" '
    NR == 1 && $NF == compiler { toolchain = 1 }
    $1 == "mod" && $2 == "github.com/golangci/golangci-lint/v2" && $3 == version { module = 1 }
    END { exit !(toolchain && module) }'
)`

const verifyIsolated = `make test-pkg PKG=./test/docsync RUN='^TestVerifySkill$' COUNT=5`

var verifyCommands = strings.Fields("build check-hooks fmt-check lint vet test-pkg check-docs spec-ci dashboard-ci check-bd-parent-grammar test-fast-parallel")

var failureRules = []string{
	"Five isolated passes leave the full command's failed exit status in the READY.",
	"Use “load-only” only if the whole package passes on head and independent evidence ties the failure to load.",
	"Treat a failure reproduced in the package on head as the change's own unless the same package command fails the same way on the unchanged base.",
	"Include both outputs in the READY. If the base passes, differs, or cannot be tested, stop and report the head failure.",
	"Never rerun the suite until it passes.",
}

var (
	makeRule        = regexp.MustCompile(`(?m)^([a-z][a-z0-9-]*):`)
	phonyLine       = regexp.MustCompile(`(?m)^\.PHONY:[ \t]*([^#\n]*)`)
	verifyFence     = regexp.MustCompile("^([ \\t]*)(`{3,}|~{3,})(.*)$")
	verifySpan      = regexp.MustCompile("`([^`]+)`")
	makeReference   = regexp.MustCompile(`\bmake[ \t]+([a-z][a-z0-9-]*)`)
	scriptReference = regexp.MustCompile(`(?:^|[\s(])([\w./-]+\.(?:sh|mjs|py))\b`)
)

func TestVerifySkill(t *testing.T) {
	root := repoRoot()
	skill := filepath.Join(root, ".claude/skills/verify/SKILL.md")
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	doc := read(skill)
	makefile := strings.ReplaceAll(read(filepath.Join(root, "Makefile")), "\\\n", " ")
	rules := make(map[string]bool)
	for _, match := range makeRule.FindAllStringSubmatch(makefile, -1) {
		rules[match[1]] = true
	}

	version := regexp.MustCompile(`(?m)^go ([0-9.]+)$`).FindStringSubmatch(read(filepath.Join(root, "go.mod")))
	if len(version) != 2 || !strings.Contains(doc, "Go "+version[1]+" ") {
		t.Error("document the Go version selected by go.mod")
	}
	for _, sentence := range failureRules {
		if !strings.Contains(doc, sentence) {
			t.Errorf("missing failure rule: %s", sentence)
		}
	}

	classified := make(map[string]string)
	seenCommands := make(map[string]bool)
	var sawLinterCheck, sawIsolated bool
	classify := func(target, class string) {
		if !rules[target] {
			t.Errorf("missing Makefile target %q", target)
		}
		if prev, ok := classified[target]; ok {
			t.Errorf("%s classified twice: %s and %s", target, prev, class)
		}
		classified[target] = class
	}
	// Only shell examples are commands. Honor the opening marker and length
	// when skipping other languages, so example output cannot become a command.
	var marker, language string
	var lines []string
	for _, line := range strings.Split(doc, "\n") {
		match := verifyFence.FindStringSubmatch(line)
		if marker == "" {
			if match == nil {
				checkVerifyProse(t, root, skill, line, rules)
				cells := strings.Split(line, "|")
				if len(cells) == 5 {
					class := strings.TrimSpace(cells[1])
					if class == "NEVER" || class == "Harmless" {
						for _, name := range verifySpan.FindAllStringSubmatch(cells[2], -1) {
							classify(name[1], class)
						}
						if strings.TrimSpace(cells[3]) == "" {
							t.Errorf("missing effect: %s", line)
						}
					}
				}
				continue
			}
			marker, language = match[2], strings.TrimSpace(match[3])
			if verifyShell(language) && (match[1] != "" || marker[0] != '`') {
				t.Errorf("use a top-level backtick fence for shell commands: %s", line)
			}
			continue
		}
		if match == nil || match[2][0] != marker[0] || len(match[2]) < len(marker) || strings.TrimSpace(match[3]) != "" {
			lines = append(lines, line)
			continue
		}
		if verifyShell(language) {
			script := strings.Join(lines, "\n")
			switch script {
			case verifyLinter:
				sawLinterCheck = true
			case verifyIsolated:
				sawIsolated = true
			default:
				target, err := verifyCommand(root, script)
				if err != nil {
					t.Error(err)
				} else {
					classify(target, "Command of record")
					seenCommands[target] = true
				}
			}
		}
		lines, marker = nil, ""
	}
	if marker != "" {
		t.Error("unclosed verification fence")
	}
	if !sawLinterCheck || !sawIsolated {
		t.Error("missing pinned linter check or five-run diagnostic")
	}
	for _, target := range verifyCommands {
		if !seenCommands[target] {
			t.Errorf("missing required verification block: %s", target)
		}
	}
	for _, match := range phonyLine.FindAllStringSubmatch(makefile, -1) {
		for _, target := range strings.Fields(match[1]) {
			if classified[target] == "" {
				t.Errorf("unclassified .PHONY target: %s", target)
			}
		}
	}
	for target := range rules {
		if classified[target] == "" {
			t.Errorf("unclassified Makefile target: %s", target)
		}
	}
}

func verifyShell(language string) bool {
	words := strings.Fields(language)
	return len(words) == 0 || slices.Contains([]string{"sh", "bash", "shell", "zsh", "console"}, words[0])
}

func checkVerifyProse(t *testing.T, root, skill, line string, rules map[string]bool) {
	t.Helper()
	checkPath := func(path string) {
		if _, err := os.Stat(path); err != nil {
			t.Error(err)
		}
	}
	checkTarget := func(name string) {
		if !rules[name] {
			t.Errorf("missing Makefile target %q", name)
		}
	}
	for _, link := range extractMarkdownLinks(line) {
		if !isExternalLink(link) {
			if path := resolveLocalLink(root, skill, link); path != "" {
				checkPath(path)
			}
		}
	}
	line = markdownLinkRE.ReplaceAllString(line, " ")
	for _, span := range verifySpan.FindAllStringSubmatch(line, -1) {
		name := span[1]
		switch {
		case strings.HasPrefix(name, "gc "):
			t.Errorf("CLI invocations belong in the guarded smoke block: %s", name)
		case strings.HasPrefix(name, "make "):
			for _, target := range strings.Fields(name)[1:] {
				checkTarget(target)
			}
		case strings.Contains(name, "/") || scriptReference.MatchString(name):
			if !filepath.IsLocal(name) {
				t.Errorf("expected a repository-relative path: %s", name)
			} else {
				checkPath(filepath.Join(root, name))
			}
		}
	}
	line = verifySpan.ReplaceAllString(line, " ")
	for _, match := range makeReference.FindAllStringSubmatch(line, -1) {
		checkTarget(match[1])
	}
	for _, match := range scriptReference.FindAllStringSubmatch(line, -1) {
		checkPath(filepath.Join(root, match[1]))
	}
}

func verifyCommand(root, script string) (string, error) {
	if script == verifySmoke {
		return "build", nil
	}
	if script == "command -v oapi-codegen && make spec-ci" {
		return "spec-ci", nil
	}
	// Validate the whole statement. Inspecting nested calls alone loses
	// control flow, status masking, redirects and background execution.
	tree, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(script), "verify skill")
	if err != nil {
		return "", err
	}
	if len(tree.Stmts) != 1 {
		return "", fmt.Errorf("expected one foreground make command: %s", script)
	}
	stmt := tree.Stmts[0]
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || stmt.Negated || stmt.Background || stmt.Coprocess || len(stmt.Redirs) > 0 ||
		len(call.Assigns) > 0 || len(call.Args) < 2 {
		return "", fmt.Errorf("expected an unconditional make command: %s", script)
	}
	args := make([]string, len(call.Args))
	for i, word := range call.Args {
		value, err := verifyLiteral(word)
		if err != nil {
			return "", err
		}
		args[i] = value
	}
	if args[0] != "make" {
		return "", fmt.Errorf("unexpected command: %s", script)
	}
	target := args[1]
	if target == "build" || target == "spec-ci" || !slices.Contains(verifyCommands, target) {
		return "", fmt.Errorf("unmeasured or unguarded verification command: %s", script)
	}
	if target != "test-pkg" {
		if len(call.Args) != 2 {
			return "", fmt.Errorf("unexpected arguments: %s", script)
		}
		return target, nil
	}
	if len(args) != 5 || args[3] != "RUN=." || args[4] != "COUNT=1" {
		return "", fmt.Errorf("whole-package check needs PKG=./package RUN=. COUNT=1: %s", script)
	}
	path, ok := strings.CutPrefix(args[2], "PKG=./")
	if !ok || !filepath.IsLocal(path) {
		return "", fmt.Errorf("invalid package argument: %s", script)
	}
	if _, err := build.Default.ImportDir(filepath.Join(root, path), 0); err != nil {
		return "", fmt.Errorf("test-pkg %s: %w", path, err)
	}
	return target, nil
}

// Quoting is fine, but arguments must not depend on variables or substitutions.
func verifyLiteral(word *syntax.Word) (string, error) {
	literal := true
	syntax.Walk(word, func(node syntax.Node) bool {
		switch node.(type) {
		case nil, *syntax.Word, *syntax.Lit, *syntax.SglQuoted, *syntax.DblQuoted:
		default:
			literal = false
		}
		return literal
	})
	if !literal {
		return "", fmt.Errorf("expected a literal argument at %s", word.Pos())
	}
	return expand.Literal(&expand.Config{Env: expand.ListEnviron()}, word)
}
