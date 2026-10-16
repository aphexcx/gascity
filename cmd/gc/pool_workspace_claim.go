package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/git"
)

// filterHookWorkspaceCandidates keeps all claim tiers on the bead whose branch
// pre_start prepared. The launch marker is evidence to check, not a lifetime
// pin: after detach the session can claim its next continuation. Its last claim
// also participates so a closed continuation stays bound until release.
func filterHookWorkspaceCandidates(candidates []beads.Bead, opts hookClaimOptions, ops hookClaimOps) ([]beads.Bead, error) {
	if hookClaimEnvValue(opts.Env, "GC_WORKTREE_CLAIM_GUARD") != "1" {
		return candidates, nil
	}
	trigger := hookClaimEnvValue(opts.Env, "GC_WORKTREE_BEAD_ID")
	branchProbe := ops.WorkspaceBranch
	if branchProbe == nil {
		branchProbe = hookWorkspaceBranch
	}
	branch, err := branchProbe(hookClaimEnvValue(opts.Env, "GC_DIR"))
	if err != nil {
		return nil, err
	}
	if branch == "" {
		return candidates, nil
	}
	currentClaim := ops.CurrentClaim
	if currentClaim == nil {
		currentClaim = hookWorkspaceCurrentClaim
	}
	last, err := currentClaim(opts.SessionID)
	if err != nil {
		return nil, fmt.Errorf("reading session claim: %w", err)
	}
	owners := []string{trigger, last}
	for _, candidate := range candidates {
		owners = append(owners, candidate.ID)
	}
	bound := ""
	for _, id := range owners {
		if id == "" || id == bound {
			continue
		}
		// Same whole-token convention as the lane preparation script: work-1
		// must not bind work-10, but task/work-1-fix is a bead branch.
		if regexp.MustCompile(`(^|[^[:alnum:]])` + regexp.QuoteMeta(id) + `([^[:alnum:]]|$)`).MatchString(branch) {
			if bound != "" {
				return nil, fmt.Errorf("branch %q names both %s and %s", branch, bound, id)
			}
			bound = id
		}
	}
	if bound != "" {
		candidates = slices.DeleteFunc(candidates, func(b beads.Bead) bool { return b.ID != bound })
	}
	return candidates, nil
}

func hookWorkspaceCurrentClaim(sessionID string) (string, error) {
	if sessionID == "" {
		return "", fmt.Errorf("missing session ID for prepared workspace")
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
