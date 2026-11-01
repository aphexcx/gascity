package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// bdOwnerLabelDirectoryMatchesScope compares the configured store with the
// directory-selected store using bd's effective database paths. The runner must
// use the create's binary, projected environment, and configured scope cwd.
// Explicit database overrides are handled before this probe. An unavailable or
// invalid answer cannot authorize a parent read from the configured store.
func bdOwnerLabelDirectoryMatchesScope(ctx context.Context, run bdWatchRunFunc, directory string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	probe := func(args []string) (string, error) {
		var out, errOut bytes.Buffer
		if code := run(ctx, args, &out, &errOut); code != 0 {
			return "", fmt.Errorf("bd info exited %d: %s", code, strings.TrimSpace(errOut.String()))
		}
		var info struct {
			DatabasePath string `json:"database_path"`
		}
		if err := json.Unmarshal(out.Bytes(), &info); err != nil {
			return "", fmt.Errorf("reading bd info database_path: %w", err)
		}
		if !filepath.IsAbs(info.DatabasePath) {
			return "", fmt.Errorf("bd info returned no absolute database_path: %q", info.DatabasePath)
		}
		return normalizePathForCompare(info.DatabasePath), nil
	}
	configured, err := probe([]string{"info", "--json"})
	if err != nil {
		return false, fmt.Errorf("configured store: %w", err)
	}
	selected, err := probe([]string{"info", "--json", "--directory", directory})
	if err != nil {
		return false, fmt.Errorf("directory-selected store: %w", err)
	}
	return configured == selected, nil
}
