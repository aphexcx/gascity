package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestBdOwnerLabelDirectoryMatchesScope(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, ".beads", "dolt")
	dir := filepath.Join(root, "work")
	response := func(path string) string {
		b, err := json.Marshal(struct {
			Path         string `json:"path"`
			DatabasePath string `json:"database_path"`
		}{filepath.Dir(path), path})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, tt := range []struct {
		name     string
		base     string
		selected string
		failCall int
		want     bool
		wantErr  string
	}{
		{"same store", response(store), response(store), 0, true, ""},
		{"normalized store", response(store), response(filepath.Dir(store) + "/./dolt"), 0, true, ""},
		{"different store", response(store), response(filepath.Join(dir, ".beads", "dolt")), 0, false, ""},
		{"redirected configured scope", response(filepath.Join(dir, ".beads", "dolt")), response(filepath.Join(dir, ".beads", "dolt")), 0, false, ""},
		{"baseline failure", "", "", 1, false, "probe unavailable"},
		{"selected failure", response(store), "", 3, false, "probe unavailable"},
		{"database probe failure", response(store), response(store), 2, false, "probe unavailable"},
		{"invalid baseline JSON", "not JSON", response(store), 0, false, "answer"},
		{"invalid selected JSON", response(store), "not JSON", 0, false, "answer"},
		{"missing path", response(store), "{}", 0, false, "database_path"},
		{"relative path", response(store), response(".beads/dolt"), 0, false, "database_path"},
		{"missing database name", response(store), response(store), 0, false, "database name"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls [][]string
			run := func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
				if _, ok := ctx.Deadline(); !ok {
					t.Error("store probes need a deadline")
				}
				calls = append(calls, args)
				if len(calls) == tt.failCall {
					if _, err := fmt.Fprint(stderr, "probe unavailable"); err != nil {
						t.Fatal(err)
					}
					return 1
				}
				payload := tt.base
				if slices.Contains(args, "--directory") {
					payload = tt.selected
				}
				if args[0] == "dolt" {
					payload = `{"database":"probe"}`
					if tt.name == "missing database name" {
						payload = `{}`
					}
				}
				if _, err := fmt.Fprint(stdout, payload); err != nil {
					t.Fatal(err)
				}
				return 0
			}
			got, err := bdOwnerLabelDirectoryMatchesScope(context.Background(), run, root, dir)
			if got != tt.want || (err != nil) != (tt.wantErr != "") || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("match=%v, err=%v; want match=%v, error containing %q", got, err, tt.want, tt.wantErr)
			}
			wantCalls := [][]string{
				{"where", "--json"},
				{"dolt", "show", "--json"},
				{"where", "--json", "--directory", dir},
				{"dolt", "show", "--json", "--directory", dir},
			}
			if len(calls) == 0 || len(calls) > len(wantCalls) {
				t.Fatalf("unexpected probes: %q", calls)
			}
			for i, call := range calls {
				if !slices.Equal(call, wantCalls[i]) {
					t.Errorf("probe %d = %q, want %q", i, call, wantCalls[i])
				}
			}
		})
	}
}

func TestBdOwnerLabelDirectoryRejectsSharedDataPath(t *testing.T) {
	root := t.TempDir()
	for _, sameProject := range []bool{false, true} {
		t.Run(fmt.Sprintf("same_project=%v", sameProject), func(t *testing.T) {
			run := func(_ context.Context, args []string, stdout, _ io.Writer) int {
				path := filepath.Join(root, ".beads")
				database := "first"
				if slices.Contains(args, "--directory") {
					database = "second"
					if !sameProject {
						path = filepath.Join(root, "nested", ".beads")
					}
				}
				// Dolt server projects can share their data directory while
				// selecting different SQL databases, including via a redirect.
				if err := json.NewEncoder(stdout).Encode(struct {
					Path         string `json:"path"`
					DatabasePath string `json:"database_path"`
					Database     string `json:"database"`
				}{path, filepath.Join(root, "shared-data"), database}); err != nil {
					t.Fatal(err)
				}
				return 0
			}
			matches, err := bdOwnerLabelDirectoryMatchesScope(context.Background(), run, root, filepath.Join(root, "nested"))
			if err != nil || matches {
				t.Fatalf("different SQL databases sharing a data directory: match=%v, err=%v", matches, err)
			}
		})
	}
}
