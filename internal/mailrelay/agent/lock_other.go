//go:build !unix

package agent

import "errors"

// lockDir is unavailable: the agent runs on Unix systems only.
func lockDir(string, bool) (func(), error) {
	return nil, errors.New("the mail relay agent runs on Unix systems only")
}
