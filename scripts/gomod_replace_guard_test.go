package scripts_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckGomodReplaceGuard verifies the check-gomod-replace script:
//   - FAILS on pseudo-version, local-path, and git-ref replace targets
//   - PASSES when no replace directives are present
//   - PASSES when all replaces point to released semver tags
//
// Regression guard for the 2026-06-11 incident where PR #3489 shipped a
// pseudo-version replace (`=> v1.0.5-0.20260611054652-dc0561af28e9`) that
// violated the public-project release policy. No automated check caught it.
func TestCheckGomodReplaceGuard(t *testing.T) {
	repoRoot := repoRoot(t)
	script := filepath.Join(repoRoot, "scripts", "check-gomod-replace.sh")

	if _, err := os.Stat(script); err != nil {
		t.Fatalf("check-gomod-replace.sh not found at %s: %v", script, err)
	}

	runScript := func(t *testing.T, content string) (output string, exitCode int) {
		t.Helper()
		dir := t.TempDir()
		gomod := filepath.Join(dir, "go.mod")
		if err := os.WriteFile(gomod, []byte(content), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		cmd := exec.Command("bash", script, gomod)
		out, err := cmd.CombinedOutput()
		if err != nil {
			ex := &exec.ExitError{}
			if errors.As(err, &ex) {
				return string(out), ex.ExitCode()
			}
			t.Fatalf("exec error: %v", err)
		}
		return string(out), 0
	}

	t.Run("passes_no_replace", func(t *testing.T) {
		gomod := "module github.com/example/mod\n\ngo 1.22\n"
		out, code := runScript(t, gomod)
		if code != 0 {
			t.Fatalf("expected exit 0 for clean go.mod, got %d\n%s", code, out)
		}
	})

	t.Run("passes_released_semver_replace", func(t *testing.T) {
		gomod := fmt.Sprintf("module github.com/example/mod\n\ngo 1.22\n\nreplace %s\n",
			"github.com/steveyegge/beads v1.0.4 => github.com/steveyegge/beads v1.0.5")
		out, code := runScript(t, gomod)
		if code != 0 {
			t.Fatalf("expected exit 0 for released semver replace, got %d\n%s", code, out)
		}
	})

	const forkPin = "v0.59.1-0.20261013092804-43ba1e2ebfb3"
	forkCases := []struct {
		name, directive string
		wantPass        bool
	}{
		{"fork_pin", "replace github.com/steveyegge/beads => github.com/aphexcx/beads " + forkPin, true},
		{"fork_block_tabs_comment", "replace (\n\tgithub.com/steveyegge/beads\t=>\tgithub.com/aphexcx/beads\t" + forkPin + " // schema lineage\n)", true},
		{"fork_release_tag", "replace github.com/steveyegge/beads => github.com/aphexcx/beads v1.5.0", false},
		{"fork_branch", "replace github.com/steveyegge/beads => github.com/aphexcx/beads main", false},
		{"fork_missing_version", "replace github.com/steveyegge/beads => github.com/aphexcx/beads", false},
		{"fork_short_timestamp", "replace github.com/steveyegge/beads => github.com/aphexcx/beads v0.59.1-0.20261013-43ba1e2ebfb3", false},
		{"fork_short_hash", "replace github.com/steveyegge/beads => github.com/aphexcx/beads v0.59.1-0.20261013092804-43ba1e2", false},
		{"fork_non_hex_hash", "replace github.com/steveyegge/beads => github.com/aphexcx/beads v0.59.1-0.20261013092804-43ba1e2ebfbz", false},
		{"fork_other_source", "replace github.com/example/beads => github.com/aphexcx/beads " + forkPin, false},
		{"fork_other_target", "replace github.com/steveyegge/beads => github.com/example/beads " + forkPin, false},
		{"fork_target_prefix", "replace github.com/steveyegge/beads => github.com/aphexcx/beads-extra " + forkPin, false},
		{"fork_source_version", "replace github.com/steveyegge/beads v1.3.1 => github.com/aphexcx/beads " + forkPin, false},
	}
	for _, tc := range forkCases {
		t.Run(tc.name, func(t *testing.T) {
			out, code := runScript(t, "module example.com/test\n\ngo 1.22\n\n"+tc.directive+"\n")
			if (code == 0) != tc.wantPass {
				t.Fatalf("exit = %d, want pass = %t\n%s", code, tc.wantPass, out)
			}
			if tc.wantPass && !strings.Contains(out, "schema lineage") {
				t.Fatalf("allowed fork pin must explain the schema lineage carve-out:\n%s", out)
			}
		})
	}

	pseudoVersionCases := []struct {
		name  string
		block string
	}{
		{
			"pseudo_version_no_source",
			"replace github.com/steveyegge/beads => github.com/steveyegge/beads v1.0.5-0.20260611054652-dc0561af28e9",
		},
		{
			"pseudo_version_with_source",
			"replace github.com/steveyegge/beads v1.0.5 => github.com/steveyegge/beads v1.0.5-0.20260611054652-dc0561af28e9",
		},
		{
			"pseudo_version_prerelease_form",
			"replace github.com/steveyegge/beads v1.0.5 => github.com/steveyegge/beads v1.0.5-beta.0.20260611054652-abcdef012345",
		},
	}
	for _, tc := range pseudoVersionCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			gomod := "module github.com/example/mod\n\ngo 1.22\n\n" + tc.block + "\n"
			out, code := runScript(t, gomod)
			if code == 0 {
				t.Fatalf("expected non-zero exit for pseudo-version replace %q, got 0\n%s", tc.block, out)
			}
			if out == "" {
				t.Fatal("expected failure message, got empty output")
			}
		})
	}

	gitRefCases := []struct {
		name  string
		block string
	}{
		{
			"git_ref_branch_name",
			"replace github.com/steveyegge/beads => github.com/steveyegge/beads main",
		},
		{
			"prerelease_label",
			"replace github.com/steveyegge/beads v1.0.4 => github.com/steveyegge/beads v1.0.5-rc1",
		},
	}
	for _, tc := range gitRefCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			gomod := "module github.com/example/mod\n\ngo 1.22\n\n" + tc.block + "\n"
			out, code := runScript(t, gomod)
			if code == 0 {
				t.Fatalf("expected non-zero exit for git-ref/prerelease replace %q, got 0\n%s", tc.block, out)
			}
			if out == "" {
				t.Fatal("expected failure message, got empty output")
			}
		})
	}

	localPathCases := []struct {
		name  string
		block string
	}{
		{
			"local_relative_path",
			"replace github.com/steveyegge/beads => ./local/beads",
		},
		{
			"local_parent_relative_path",
			"replace github.com/steveyegge/beads => ../beads",
		},
		{
			"local_absolute_path",
			"replace github.com/steveyegge/beads => /usr/local/beads",
		},
	}
	for _, tc := range localPathCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			gomod := "module github.com/example/mod\n\ngo 1.22\n\n" + tc.block + "\n"
			out, code := runScript(t, gomod)
			if code == 0 {
				t.Fatalf("expected non-zero exit for local-path replace %q, got 0\n%s", tc.block, out)
			}
			if out == "" {
				t.Fatal("expected failure message, got empty output")
			}
		})
	}

	multiLineBlockCases := []struct {
		name  string
		gomod string
	}{
		{
			"multi_line_block_pseudo_version",
			"module github.com/example/mod\n\ngo 1.22\n\nreplace (\n\tgithub.com/steveyegge/beads v1.0.5 => github.com/steveyegge/beads v1.0.5-0.20260611054652-dc0561af28e9\n)\n",
		},
		{
			"multi_line_block_local_path",
			"module github.com/example/mod\n\ngo 1.22\n\nreplace (\n\tgithub.com/steveyegge/beads => ./local/beads\n)\n",
		},
		{
			"multi_line_block_passes_released",
			"module github.com/example/mod\n\ngo 1.22\n\nreplace (\n\tgithub.com/steveyegge/beads v1.0.4 => github.com/steveyegge/beads v1.0.5\n)\n",
		},
	}
	for _, tc := range multiLineBlockCases {
		tc := tc
		wantFail := !strings.HasSuffix(tc.name, "_released")
		t.Run(tc.name, func(t *testing.T) {
			out, code := runScript(t, tc.gomod)
			if wantFail && code == 0 {
				t.Fatalf("expected non-zero exit for multi-line block case %q, got 0\n%s", tc.name, out)
			}
			if !wantFail && code != 0 {
				t.Fatalf("expected exit 0 for multi-line block released case %q, got %d\n%s", tc.name, code, out)
			}
		})
	}

	inlineCommentCases := []struct {
		name     string
		gomod    string
		wantFail bool
	}{
		{
			"inline_comment_pseudo_version",
			"module github.com/example/mod\n\ngo 1.22\n\nreplace github.com/steveyegge/beads v1.0.5 => github.com/steveyegge/beads v1.0.5-0.20260611054652-dc0561af28e9 // emergency fix\n",
			true,
		},
		{
			"inline_comment_git_ref",
			"module github.com/example/mod\n\ngo 1.22\n\nreplace github.com/steveyegge/beads => github.com/steveyegge/beads main // pin branch\n",
			true,
		},
		{
			"inline_comment_local_path",
			"module github.com/example/mod\n\ngo 1.22\n\nreplace github.com/steveyegge/beads => ./local/beads // dev\n",
			true,
		},
		{
			"inline_comment_released_semver",
			"module github.com/example/mod\n\ngo 1.22\n\nreplace github.com/steveyegge/beads v1.0.4 => github.com/steveyegge/beads v1.0.5 // bump\n",
			false,
		},
		{
			"multi_line_block_inline_comment_pseudo_version",
			"module github.com/example/mod\n\ngo 1.22\n\nreplace (\n\tgithub.com/steveyegge/beads v1.0.5 => github.com/steveyegge/beads v1.0.5-0.20260611054652-dc0561af28e9 // emergency fix\n)\n",
			true,
		},
		{
			"multi_line_block_inline_comment_released",
			"module github.com/example/mod\n\ngo 1.22\n\nreplace (\n\tgithub.com/steveyegge/beads v1.0.4 => github.com/steveyegge/beads v1.0.5 // bump\n)\n",
			false,
		},
	}
	for _, tc := range inlineCommentCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			out, code := runScript(t, tc.gomod)
			if tc.wantFail && code == 0 {
				t.Fatalf("expected non-zero exit for inline-comment case %q, got 0\n%s", tc.name, out)
			}
			if !tc.wantFail && code != 0 {
				t.Fatalf("expected exit 0 for inline-comment case %q, got %d\n%s", tc.name, code, out)
			}
		})
	}

	t.Run("failure_message_mentions_policy", func(t *testing.T) {
		gomod := "module github.com/example/mod\n\ngo 1.22\n\nreplace github.com/steveyegge/beads => ./local/beads\n"
		out, code := runScript(t, gomod)
		if code == 0 {
			t.Fatal("expected non-zero exit")
		}
		for _, want := range []string{"released", "human"} {
			if !strings.Contains(out, want) {
				t.Errorf("failure message should mention %q:\n%s", want, out)
			}
		}
	})
}

// TestCheckGomodReplaceGuardWiredIntoMakefile verifies that the guard
// is a registered .PHONY target and is called from the preflight check
// target so a CI run actually invokes it.
func TestCheckGomodReplaceGuardWiredIntoMakefile(t *testing.T) {
	repoRoot := repoRoot(t)

	makefile, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	content := string(makefile)

	for _, want := range []string{
		"check-gomod-replace",
		"scripts/check-gomod-replace.sh",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("Makefile missing %q", want)
		}
	}
}

// TestCheckGomodReplaceGuardWiredIntoCI verifies that the guard is wired
// into the preflight-static CI job so every PR is checked.
func TestCheckGomodReplaceGuardWiredIntoCI(t *testing.T) {
	repoRoot := repoRoot(t)

	workflow, err := os.ReadFile(filepath.Join(repoRoot, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	if !strings.Contains(string(workflow), "check-gomod-replace") {
		t.Error("ci.yml preflight-static job is missing the check-gomod-replace step")
	}
}
