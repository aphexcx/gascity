//go:build !linux && !darwin

package proctable

import (
	"errors"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ScanBySessionID is unavailable on platforms without process environment
// scanning support.
func ScanBySessionID(string) ([]runtime.LiveRuntime, error) {
	return []runtime.LiveRuntime{}, nil
}

func scanBySessionID(string) ([]runtime.LiveRuntime, error) {
	return nil, errors.New("session process lookup is unavailable on this platform")
}

// IsScanRoot reports false on platforms without process environment scanning
// support.
func IsScanRoot(int) bool {
	return false
}
