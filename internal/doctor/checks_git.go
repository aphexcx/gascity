package doctor

import (
	"fmt"

	gitutil "github.com/gastownhall/gascity/internal/git"
)

// GitExecutableCheck verifies that Git runs and reports macOS license failures.
type GitExecutableCheck struct {
	resolve func() (gitutil.Executable, error)
}

// NewGitExecutableCheck creates the Git installation diagnostic.
func NewGitExecutableCheck() *GitExecutableCheck {
	return &GitExecutableCheck{resolve: gitutil.ResolveExecutable}
}

// Name preserves the existing doctor check selector.
func (c *GitExecutableCheck) Name() string { return "git-binary" }

// Run reports healthy Git, a working fallback, or an unusable installation.
func (c *GitExecutableCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	git, err := c.resolve()
	switch {
	case err != nil:
		r.Status = StatusError
		r.Message = err.Error()
	case git.Warning != "":
		r.Status = StatusWarning
		r.Message = git.Warning
	default:
		r.Status = StatusOK
		r.Message = fmt.Sprintf("%s (%s)", git.Path, git.Version)
	}
	return r
}

// CanFix returns false: license acceptance requires a human.
func (c *GitExecutableCheck) CanFix() bool { return false }

// Fix is a no-op; doctor never accepts a license or invokes sudo.
func (c *GitExecutableCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns false; this check runs on demand.
func (c *GitExecutableCheck) WarmupEligible() bool { return false }
