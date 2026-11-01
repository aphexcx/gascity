package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// bdOwnerLabelDirectoryMatchesScope compares the configured store with the
// directory-selected store using bd's project, data path, backend, and (for Dolt)
// SQL database name.
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
		project, dataPath, backend, database string
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
			Backend  string `json:"backend"`
			Database string `json:"database"`
		}
		if err := query(append([]string{"dolt", "show", "--json"}, selector...), &config); err != nil {
			return identity{}, err
		}
		if strings.TrimSpace(config.Backend) == "" {
			return identity{}, fmt.Errorf("bd dolt show returned no backend")
		}
		// bd reports only the backend for non-Dolt storage bindings. Their
		// identity comes from the project and data path, without a SQL name.
		if config.Backend == "dolt" && strings.TrimSpace(config.Database) == "" {
			return identity{}, fmt.Errorf("bd dolt show returned no database name")
		}
		return identity{
			project:  normalizePathForCompare(location.Path),
			dataPath: normalizePathForCompare(location.DatabasePath),
			backend:  config.Backend,
			database: config.Database,
		}, nil
	}
	configured, err := probe(nil)
	if err != nil {
		return false, fmt.Errorf("configured store: %w", err)
	}
	// bd's data commands preserve a configured redirect source's raw database
	// name. Its no-store diagnostics resolve the target before reading that name.
	// Recover this one field from the known scope, leaving discovery to bd.
	if configured.backend == "dolt" && configured.project != normalizePathForCompare(filepath.Join(scopeRoot, ".beads")) {
		data, err := os.ReadFile(filepath.Join(scopeRoot, ".beads", "metadata.json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("reading configured redirect metadata: %w", err)
		}
		if err == nil {
			var source struct {
				Database string `json:"dolt_database"`
			}
			if err := json.Unmarshal(data, &source); err != nil {
				return false, fmt.Errorf("reading configured redirect database: %w", err)
			}
			if source.Database != "" {
				configured.database = source.Database
			}
		}
	}
	selected, err := probe([]string{"--directory", directory})
	if err != nil {
		return false, fmt.Errorf("directory-selected store: %w", err)
	}
	return configured == selected, nil
}
