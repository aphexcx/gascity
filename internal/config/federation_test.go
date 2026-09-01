package config

import (
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestParseFederationIdentity(t *testing.T) {
	cfg, err := Parse([]byte("[federation]\nidentity = \"citadel\"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Federation.Identity != "citadel" {
		t.Fatalf("Federation.Identity = %q, want citadel", cfg.Federation.Identity)
	}
	label, ok := cfg.Federation.OwnerLabel()
	if !ok || label != "owner:citadel" {
		t.Fatalf("OwnerLabel() = (%q, %v), want (owner:citadel, true)", label, ok)
	}
}

func TestParseWithoutFederationLeavesTheOwnerLabelUnset(t *testing.T) {
	cfg, err := Parse([]byte("[workspace]\nname = \"demo\"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if label, ok := cfg.Federation.OwnerLabel(); ok || label != "" {
		t.Fatalf("OwnerLabel() = (%q, %v), want unset", label, ok)
	}
}

func TestParseRejectsAMalformedFederationIdentity(t *testing.T) {
	for _, bad := range []string{"Citadel", "-x", "city name", "under_score"} {
		_, err := Parse([]byte("[federation]\nidentity = " + strconv.Quote(bad) + "\n"))
		if err == nil || !strings.Contains(err.Error(), "federation.identity") {
			t.Errorf("Parse(identity=%q) error = %v, want a federation.identity config error", bad, err)
		}
	}
}

// TestLoadWithIncludesFederationIsLastWriterWins: a fragment that defines
// [federation] replaces the root's, and one that does not leaves it alone.
func TestLoadWithIncludesFederationIsLastWriterWins(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test"

[federation]
identity = "citadel"
`)
	fs.Files["/city/fragment.toml"] = []byte(`
[federation]
identity = "jadegate"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.Federation.Identity != "jadegate" {
		t.Fatalf("Federation.Identity = %q, want the fragment's jadegate", cfg.Federation.Identity)
	}

	fs.Files["/city/fragment.toml"] = []byte(`
[beads]
provider = "bd"
`)
	cfg, _, err = LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.Federation.Identity != "citadel" {
		t.Fatalf("Federation.Identity = %q, want the root's citadel kept", cfg.Federation.Identity)
	}
}
