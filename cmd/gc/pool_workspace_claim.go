package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/git"
)

// filterHookWorkspaceCandidates restricts fresh pulls to the bead whose branch
// pre_start prepared, while preserving work already assigned to this session.
// Its last claim also identifies a shared continuation workspace until release.
// The boolean reports skipped candidates for the final drain's diagnostics.
func filterHookWorkspaceCandidates(candidates []beads.Bead, opts hookClaimOptions, ops hookClaimOps, stderr io.Writer) ([]beads.Bead, bool, error) {
	if hookClaimEnvValue(opts.Env, "GC_WORKTREE_CLAIM_GUARD") != "1" {
		return candidates, false, nil
	}
	trigger := hookClaimEnvValue(opts.Env, "GC_WORKTREE_BEAD_ID")
	branchProbe := ops.WorkspaceBranch
	if branchProbe == nil {
		branchProbe = hookWorkspaceBranch
	}
	branch, err := branchProbe(hookClaimEnvValue(opts.Env, "GC_DIR"))
	if err != nil {
		return nil, false, err
	}
	if branch == "" {
		return candidates, false, nil
	}
	currentClaim := ops.CurrentClaim
	if currentClaim == nil {
		currentClaim = hookWorkspaceCurrentClaim
	}
	last := ""
	if opts.SessionID != "" {
		last, err = currentClaim(opts.SessionID)
		if err != nil {
			return nil, false, fmt.Errorf("reading session claim: %w", err)
		}
	}
	matches := hookWorkspaceBranchBeads(branch, opts.WorkspaceConfig)
	owners := []string{trigger, last}
	for _, candidate := range candidates {
		owners = append(owners, candidate.ID)
	}
	for _, id := range owners {
		if id == "" {
			continue
		}
		// Match complete IDs, including dotted children: work-1 must not
		// bind work-10 or work-1.1; task/work-1-fix is a bead branch.
		pattern := regexp.MustCompile(`(^|[^[:alnum:].])(` + regexp.QuoteMeta(id) + `)([^[:alnum:].]|$)`)
		for _, match := range pattern.FindAllStringSubmatchIndex(branch, -1) {
			matches = append(matches, hookWorkspaceBeadMatch{id: id, start: match[4], end: match[5]})
		}
	}
	// A complete longer ID owns any nested shorter match (fleet-work-2
	// versus work-2). Distinct occurrences still represent ambiguous owners.
	slices.SortFunc(matches, func(a, b hookWorkspaceBeadMatch) int {
		if a.start != b.start {
			return a.start - b.start
		}
		return b.end - a.end
	})
	bound, end := "", -1
	for _, match := range matches {
		if match.end <= end {
			continue
		}
		if bound != "" && bound != match.id {
			return nil, false, fmt.Errorf("branch %q names both %s and %s", branch, bound, match.id)
		}
		bound, end = match.id, match.end
	}
	before := len(candidates)
	if bound != "" {
		candidates = slices.DeleteFunc(candidates, func(b beads.Bead) bool {
			return b.ID != bound && !hookClaimHasIdentity(b.Assignee, opts.IdentityCandidates)
		})
	}
	skipped := before - len(candidates)
	if skipped > 0 {
		fmt.Fprintf(stderr, "gc hook --claim: workspace bound to %s (branch %s); %d routed candidate(s) skipped\n", bound, branch, skipped) //nolint:errcheck
	}
	return candidates, skipped > 0, nil
}

type hookWorkspaceBeadMatch struct {
	id         string
	start, end int
}

// hookWorkspaceBranchBeads recognizes configured bead IDs without depending on
// an open-work query or a previous session's receipt. A replacement idle session
// can inherit an attached branch after its original bead has closed.
func hookWorkspaceBranchBeads(branch string, cfg *config.City) []hookWorkspaceBeadMatch {
	if cfg == nil {
		return nil
	}
	prefixes := []string{config.EffectiveHQPrefix(cfg)}
	for _, rig := range cfg.Rigs {
		prefixes = append(prefixes, rig.EffectivePrefix())
	}
	prefixes = slices.DeleteFunc(prefixes, func(p string) bool { return p == "" })
	if len(prefixes) == 0 {
		return nil
	}
	// Prefer a complete longer prefix over a shorter prefix nested inside it.
	slices.SortFunc(prefixes, func(a, b string) int { return len(b) - len(a) })
	for i := range prefixes {
		prefixes[i] = regexp.QuoteMeta(prefixes[i])
	}
	pattern := regexp.MustCompile(`(^|[^[:alnum:].])((?:` + strings.Join(prefixes, "|") + `)-[[:alnum:]]{1,12}(?:\.[[:alnum:]_-]+)*)`)
	var matches []hookWorkspaceBeadMatch
	for _, match := range pattern.FindAllStringSubmatchIndex(branch, -1) {
		end := match[5]
		// A truncated base or hierarchical child is not a complete bead ID.
		if end < len(branch) && strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.", rune(branch[end])) {
			continue
		}
		matches = append(matches, hookWorkspaceBeadMatch{id: branch[match[4]:end], start: match[4], end: end})
	}
	return matches
}

func hookWorkspaceCurrentClaim(sessionID string) (string, error) {
	if sessionID == "" {
		return "", nil
	}
	front, err := sessionCurrentClaimFrontDoor()
	if err != nil {
		return "", err
	}
	return front.CurrentClaimBeadID(sessionID)
}

// hookWorkspaceBranch distinguishes a released/non-Git directory from a probe
// failure. Missing paths, inaccessible repositories and unavailable Git must not
// silently authorize claiming into a workspace whose ownership is unknown.
func hookWorkspaceBranch(dir string) (string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("workspace %q: %w", dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace %q is not a directory", dir)
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimGitProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	cmd.Env = append(git.SanitizedEnv(), "LC_ALL=C")
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("probing workspace branch: %w", ctx.Err())
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// symbolic-ref -q exits 1 without diagnostics for detached HEAD.
			if exitErr.ExitCode() == 1 && len(exitErr.Stderr) == 0 {
				return "", nil
			}
			if exitErr.ExitCode() == 128 && strings.HasPrefix(string(exitErr.Stderr), "fatal: not a git repository (or any of the parent directories): .git") {
				return "", nil
			}
			return "", fmt.Errorf("probing workspace branch: %w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("probing workspace branch: %w", err)
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return "", fmt.Errorf("workspace branch probe returned no branch")
	}
	return branch, nil
}
