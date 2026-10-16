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

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/git"
)

// filterHookWorkspaceCandidates restricts fresh pulls to the bead whose branch
// pre_start prepared, while preserving work already assigned to this session.
// Its last claim also identifies a shared continuation workspace until release.
// The boolean reports skipped candidates for the final drain's diagnostics.
func filterHookWorkspaceCandidates(candidates []beads.Bead, dir string, opts hookClaimOptions, ops hookClaimOps, stderr io.Writer) ([]beads.Bead, bool, error) {
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
	owners := []string{trigger, last}
	for _, candidate := range candidates {
		owners = append(owners, candidate.ID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookClaimMutationTimeout)
	defer cancel()
	// A root shared by several steps, or an ID repeated in the branch, needs
	// only one read per store leg's candidate set.
	type readResult struct {
		bead beads.Bead
		err  error
	}
	type readKey struct {
		id            string
		inferredOwner bool
	}
	reads := make(map[readKey]readResult)
	readBead := func(id string, inferredOwner bool) (beads.Bead, error) {
		key := readKey{id, inferredOwner}
		if result, ok := reads[key]; ok {
			return result.bead, result.err
		}
		bead, err := hookWorkspaceReadBead(ctx, dir, opts, ops, id, inferredOwner)
		reads[key] = readResult{bead, err}
		return bead, err
	}
	matches := hookWorkspaceBranchBeads(branch, opts.WorkspaceConfig)
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
		// Prefix syntax is only a hint: reusable branches such as gc-worker
		// must not bind to fictitious beads. Closed real beads still bind.
		// Only verified longer IDs suppress nested shorter matches.
		if !slices.Contains(owners, match.id) {
			if _, err := readBead(match.id, true); errors.Is(err, beads.ErrNotFound) {
				continue
			} else if err != nil {
				return nil, false, fmt.Errorf("verifying workspace owner %s: %w", match.id, err)
			}
		}
		if bound != "" && bound != match.id {
			return nil, false, fmt.Errorf("branch %q names both %s and %s", branch, bound, match.id)
		}
		bound, end = match.id, match.end
	}
	before := len(candidates)
	if bound != "" {
		kept := make([]beads.Bead, 0, len(candidates))
		var rootErrors []error
		for _, b := range candidates {
			if b.ID == bound || hookClaimHasIdentity(b.Assignee, opts.IdentityCandidates) {
				kept = append(kept, b)
				continue
			}
			// Drain steps intentionally run in their source member's worktree.
			// Use the same durable root relationship as resolveDrainSourceWorkDir;
			// unrelated steps remain excluded even when offered first.
			root := b
			if rootID := strings.TrimSpace(b.Metadata[beadmeta.RootBeadIDMetadataKey]); rootID != "" && rootID != b.ID {
				root, err = readBead(rootID, false)
				if err != nil {
					if !errors.Is(err, beads.ErrNotFound) {
						rootErrors = append(rootErrors, fmt.Errorf("reading workspace root %s for %s: %w", rootID, b.ID, err))
					}
					continue
				}
			}
			if strings.TrimSpace(root.Metadata[beadmeta.DrainMemberIDMetadataKey]) == bound {
				kept = append(kept, b)
			}
		}
		// A foreign step's missing root proves no drain relationship. Other
		// read failures must remain visible, but cannot strand owned work.
		if len(rootErrors) > 0 {
			if len(kept) == 0 {
				return nil, false, errors.Join(rootErrors...)
			}
			for _, rootErr := range rootErrors {
				fmt.Fprintf(stderr, "gc hook --claim: skipping candidate with unknown workspace relationship: %v\n", rootErr) //nolint:errcheck
			}
		}
		candidates = kept
	}
	skipped := before - len(candidates)
	if skipped > 0 {
		fmt.Fprintf(stderr, "gc hook --claim: workspace bound to %s (branch %s); %d routed candidate(s) skipped\n", bound, branch, skipped) //nolint:errcheck
	}
	return candidates, skipped > 0, nil
}

// hookWorkspaceReadBead resolves ownership through the claim's existing store
// routes. A closed owner can live outside the leg currently offering work.
// Inferred owners also treat bd's partial-ID ambiguity as an exact miss; roots
// retain that error because their relationship is explicitly recorded.
func hookWorkspaceReadBead(ctx context.Context, dir string, opts hookClaimOptions, ops hookClaimOps, id string, inferredOwner bool) (beads.Bead, error) {
	read := ops.ReadWorkMeta
	if read == nil {
		read = hookReadClaimedBeadWithBdStore
	}
	ambiguous := false
	notFound := func(err error) bool {
		if errors.Is(err, beads.ErrNotFound) {
			return true
		}
		if inferredOwner && hookWorkspaceAmbiguousPartialID(err, id) {
			ambiguous = true
			return true
		}
		return false
	}
	bead, err := read(ctx, dir, opts.Env, id, opts.Assignee)
	if !notFound(err) {
		return bead, err
	}
	current := hookStore{dir: dir, env: opts.Env}
	for _, leg := range ops.WorkLegs {
		if sameHookStore(leg, current) || (len(leg.env) == 0 && strings.TrimSpace(leg.dir) == strings.TrimSpace(dir)) {
			continue
		}
		bead, legErr := read(ctx, leg.dir, leg.env, id, opts.Assignee)
		if !notFound(legErr) {
			return bead, legErr
		}
	}
	// The class-route wrapper only escalates typed not-found, so ambiguity
	// may have suppressed its binding probe. All work legs have now proved
	// an exact miss; check that same binding before declaring no owner.
	if ambiguous && ops.ClassRoute != nil {
		held, err := ops.ClassRoute.holds(id)
		if err != nil {
			return beads.Bead{}, err
		}
		if held {
			return ops.ClassRoute.graph.Get(id)
		}
	}
	return beads.Bead{}, fmt.Errorf("workspace bead %q: %w", id, beads.ErrNotFound)
}

// bd resolves exact IDs before reporting this partial-ID ambiguity. Keep this
// compatibility check local to inferred owners until BdStore exposes a typed
// exact-miss error for ambiguity; infrastructure errors must not become misses.
func hookWorkspaceAmbiguousPartialID(err error, id string) bool {
	if err == nil {
		return false
	}
	pattern := regexp.MustCompile(`(?m)(?:^|: )ambiguous issue ID: ` + regexp.QuoteMeta(fmt.Sprintf("%q", id)) + ` matches [1-9][0-9]* issues: \[[^\r\n]*\]\nUse more characters to disambiguate(?:\r?\n|$)`)
	return pattern.MatchString(err.Error())
}

type hookWorkspaceBeadMatch struct {
	id         string
	start, end int
}

// hookWorkspaceBranchBeads extracts possible configured bead IDs. The caller
// verifies IDs absent from its trigger, receipt and candidates against the store.
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
	var matches []hookWorkspaceBeadMatch
	// Scan every token boundary: a nonexistent longer ID must not hide a real
	// nested owner or a dotted child followed by a branch-name suffix.
	// Wisps have their own base-ID form in the same store.
	const tokenChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789."
	for _, prefix := range prefixes {
		for _, kind := range []string{"", "-wisp"} {
			pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(prefix+kind) + `-[[:alnum:]]{1,12}(?:\.[[:alnum:]_-]+)*$`)
			for start := range len(branch) {
				if start > 0 && strings.ContainsRune(tokenChars, rune(branch[start-1])) {
					continue
				}
				for end := start + 1; end <= len(branch); end++ {
					// A truncated base or hierarchical child is not a complete bead ID.
					if end < len(branch) && strings.ContainsRune(tokenChars, rune(branch[end])) {
						continue
					}
					if pattern.MatchString(branch[start:end]) {
						matches = append(matches, hookWorkspaceBeadMatch{id: branch[start:end], start: start, end: end})
					}
				}
			}
		}
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
