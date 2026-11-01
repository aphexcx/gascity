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
			DatabasePath string `json:"database_path"`
		}{path})
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
		{"baseline failure", "", "", 1, false, "probe unavailable"},
		{"selected failure", response(store), "", 2, false, "probe unavailable"},
		{"invalid baseline JSON", "not JSON", response(store), 0, false, "database_path"},
		{"invalid selected JSON", response(store), "not JSON", 0, false, "database_path"},
		{"missing path", response(store), "{}", 0, false, "database_path"},
		{"relative path", response(store), response(".beads/dolt"), 0, false, "database_path"},
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
				if len(calls) == 1 {
					if _, err := fmt.Fprint(stdout, tt.base); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := fmt.Fprint(stdout, tt.selected); err != nil {
						t.Fatal(err)
					}
				}
				return 0
			}
			got, err := bdOwnerLabelDirectoryMatchesScope(context.Background(), run, dir)
			if got != tt.want || (err != nil) != (tt.wantErr != "") || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("match=%v, err=%v; want match=%v, error containing %q", got, err, tt.want, tt.wantErr)
			}
			if len(calls) == 0 || !slices.Equal(calls[0], []string{"info", "--json"}) {
				t.Fatalf("missing configured store probe: %q", calls)
			}
			if len(calls) > 1 && !slices.Equal(calls[1], []string{"info", "--json", "--directory", dir}) {
				t.Fatalf("wrong selected store probe: %q", calls)
			}
		})
	}
}
