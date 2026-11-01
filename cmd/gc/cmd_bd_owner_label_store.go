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
// directory-selected store using bd's project, data path, and SQL database name.
// A data path alone can contain several Dolt server databases. The runner must
// use the create's binary, projected environment, and configured scope cwd.
// Explicit database overrides are handled before this probe. An unavailable or
// invalid answer cannot authorize a parent read from the configured store.
func bdOwnerLabelDirectoryMatchesScope(ctx context.Context, run bdWatchRunFunc, scopeRoot, directory string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	query := func(args []string, result any) error {
		var out, errOut bytes.Buffer
		if code := run(ctx, args, &out, &errOut); code != 0 {
			return fmt.Errorf("bd %s exited %d: %s", args[0], code, strings.TrimSpace(errOut.String()))
		}
		if err := json.Unmarshal(out.Bytes(), result); err != nil {
			return fmt.Errorf("reading bd %s answer: %w", args[0], err)
		}
		return nil
	}
	type identity struct {
		project, dataPath, database string
	}
	probe := func(selector []string) (identity, error) {
		var location struct {
			Path         string `json:"path"`
			DatabasePath string `json:"database_path"`
		}
		if err := query(append([]string{"where", "--json"}, selector...), &location); err != nil {
			return identity{}, err
		}
		if !filepath.IsAbs(location.Path) || !filepath.IsAbs(location.DatabasePath) {
			return identity{}, fmt.Errorf("bd where returned no absolute path/database_path: %q / %q", location.Path, location.DatabasePath)
		}
		var config struct {
			Database string `json:"database"`
		}
		if err := query(append([]string{"dolt", "show", "--json"}, selector...), &config); err != nil {
			return identity{}, err
		}
		if strings.TrimSpace(config.Database) == "" {
			return identity{}, fmt.Errorf("bd dolt show returned no database name")
		}
		return identity{normalizePathForCompare(location.Path), normalizePathForCompare(location.DatabasePath), config.Database}, nil
	}
	configured, err := probe(nil)
	if err != nil {
		return false, fmt.Errorf("configured store: %w", err)
	}
	// The diagnostic commands resolve redirects before loading database names.
	// A redirected configured scope can preserve source metadata on data commands
	// that these diagnostics do not report. Do not authorize a read through that
	// ambiguous configured scope; ordinary -C redirects into this scope are safe.
	if configured.project != normalizePathForCompare(filepath.Join(scopeRoot, ".beads")) {
		return false, nil
	}
	selected, err := probe([]string{"--directory", directory})
	if err != nil {
		return false, fmt.Errorf("directory-selected store: %w", err)
	}
	return configured == selected, nil
}
