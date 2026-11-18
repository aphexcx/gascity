package docsync

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

func TestVerifySkill(t *testing.T) {
	root := repoRoot()
	skill := filepath.Join(root, ".claude", "skills", "verify", "SKILL.md")
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	makefile := read(filepath.Join(root, "Makefile"))
	doc := read(skill)
	for _, link := range extractMarkdownLinks(doc) {
		if path := resolveLocalLink(root, skill, link); path != "" {
			if _, err := os.Stat(path); err != nil {
				t.Error(err)
			}
		}
	}
	// Parse fenced shell as shell, so a quoted '#' or '&&' stays in its word.
	inBlock := false
	var block strings.Builder
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "```") {
			if inBlock {
				for _, problem := range verifyCommandProblems(block.String(), root, makefile) {
					t.Error(problem)
				}
				block.Reset()
			}
			inBlock = !inBlock
		} else if inBlock {
			block.WriteString(line + "\n")
		}
	}
	if inBlock {
		t.Error("unclosed command fence")
	}

	t.Run("stale commands", func(t *testing.T) {
		const fixture = "make test-pkg PKG=./removed-package RUN='a # b && c' && make 'removed-target'"
		got := strings.Join(verifyCommandProblems(fixture, root, makefile), "\n")
		if !strings.Contains(got, "removed-package") || !strings.Contains(got, "missing Makefile target removed-target") ||
			strings.Contains(got, "unrecognized verification command") {
			t.Errorf("unexpected diagnostics for quoted fixture: %s", got)
		}
		if len(verifyCommandProblems("./bin/gc version", root, makefile)) == 0 {
			t.Error("accepted a smoke command that inherits the caller environment")
		}
	})

	t.Run("unsafe commands", func(t *testing.T) {
		for _, cmd := range []string{
			"scratch=.\nenv -i -C \"$scratch\" HOME=\"$scratch\" ./bin/gc version",
			"make docs-dev", "bazel run //cmd/gc -- start",
		} {
			if len(verifyCommandProblems(cmd, root, makefile)) == 0 {
				t.Errorf("accepted an unguarded startup: %s", cmd)
			}
		}
	})
}

func verifyCommandProblems(script, root, makefile string) []string {
	// This is the only block permitted to invoke the CLI. Pin the scratch
	// directory, cleanup and both child environments together.
	const smoke = `(
  set -eu
  scratch=$(mktemp -d /var/tmp/gc-verify.XXXXXXXX)
  trap 'rm -rf "$scratch"' EXIT
  make build BUILD_DIR="$scratch/bin"
  env -i -C "$scratch" HOME="$scratch" ./bin/gc version
  env -i -C "$scratch" HOME="$scratch" ./bin/gc --help
)`
	if strings.TrimSpace(script) == smoke {
		if !strings.Contains("\n"+makefile, "\nbuild:") {
			return []string{"missing Makefile target build"}
		}
		return nil
	}
	tree, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(script), "verify skill")
	if err != nil {
		return []string{err.Error()}
	}
	allowed := strings.Fields("fmt-check lint vet test-pkg check-docs dashboard-ci test")
	var problems []string
	cfg := &expand.Config{Env: expand.ListEnviron()}
	syntax.Walk(tree, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok {
			return true
		}
		if len(call.Assigns) > 0 || len(call.Args) < 2 || call.Args[0].Lit() != "make" {
			problems = append(problems, "unrecognized verification command or missing isolation guard: "+script)
			return true
		}
		targets := 0
		for _, word := range call.Args[1:] {
			arg, err := expand.Literal(cfg, word)
			if err != nil {
				problems = append(problems, err.Error())
				continue
			}
			switch {
			case strings.HasPrefix(arg, "PKG=./"):
				if _, err := os.Stat(filepath.Join(root, strings.TrimPrefix(arg, "PKG="))); err != nil {
					problems = append(problems, err.Error())
				}
			case strings.HasPrefix(arg, "RUN="), strings.HasPrefix(arg, "COUNT="):
			default:
				targets++
				if !strings.Contains("\n"+makefile, "\n"+arg+":") {
					problems = append(problems, "missing Makefile target "+arg)
				} else if !slices.Contains(allowed, arg) {
					problems = append(problems, "target outside measured verification commands: "+arg)
				}
			}
		}
		if targets == 0 {
			problems = append(problems, "make needs an explicit target")
		}
		return true
	})
	return problems
}
