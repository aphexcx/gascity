package doctor

import (
	"errors"
	"strings"
	"testing"

	gitutil "github.com/gastownhall/gascity/internal/git"
)

func TestGitExecutableCheck(t *testing.T) {
	for _, tc := range []struct {
		name   string
		git    gitutil.Executable
		err    error
		status CheckStatus
		text   string
	}{
		{"healthy", gitutil.Executable{Path: "/usr/bin/git", Version: "git version 2.50"}, nil, StatusOK, "git version 2.50"},
		{"fallback", gitutil.Executable{Path: "/clt/git", Warning: "Xcode license unaccepted; using Command Line Tools fallback /clt/git"}, nil, StatusWarning, "Xcode license"},
		{"unusable", gitutil.Executable{}, errors.New("git itself is unusable: Xcode license; Command Line Tools fallback absent"), StatusError, "fallback absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := &GitExecutableCheck{resolve: func() (gitutil.Executable, error) { return tc.git, tc.err }}
			got := check.Run(nil)
			if got.Name != "git-binary" || got.Status != tc.status || !strings.Contains(got.Message, tc.text) {
				t.Fatalf("result = %+v", got)
			}
			if check.CanFix() {
				t.Fatal("doctor must never accept a license automatically")
			}
		})
	}
}
