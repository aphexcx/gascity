package config

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// A mail_wake_from entry may name a peer city by the id of its registered key.
func TestMailWakeFromPeerCityEntry(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "city.toml", `
[workspace]
name = "beta"
[[agent]]
name = "writer"
[[agent]]
name = "reader"
mail_wake_from = ["human", "writer", "city:alpha", "city:a.b-c_1"]
`)
	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("a list naming a peer city did not load: %v", err)
	}
	if err := ValidateAgents(cfg.Agents); err != nil {
		t.Fatalf("a list naming a peer city did not validate: %v", err)
	}
	reader := FindAgent(cfg, "reader")
	if reader == nil || !slices.Equal(reader.MailWakeFrom, []string{"human", "writer", "city:alpha", "city:a.b-c_1"}) {
		t.Fatalf("loaded list: %+v", reader)
	}

	// The key id passes the rule a trusted key file's name passes.
	for _, entry := range []string{"city:", "city:a:b", "city:a,b", "city: alpha", "city:alpha ", "city:a\nb", "city:a\rb", "city:a\x00b"} {
		if err := ValidateAgents([]Agent{{Name: "reader", MailWakeFrom: []string{entry}}}); err == nil {
			t.Errorf("accepted mail_wake_from entry %q", entry)
		}
	}

	// No agent can take a peer entry's name, so an entry is never ambiguous.
	if err := ValidateAgents([]Agent{{Name: "city:alpha"}}); err == nil {
		t.Error("accepted an agent named like a peer entry")
	}
}

// An imported pack's peer entry is not a pack-local agent name.
func TestImportedMailWakePeerEntryStaysAsWritten(t *testing.T) {
	for _, binding := range []string{"imports.b", "rigs.imports.b"} {
		t.Run(binding, func(t *testing.T) {
			dir := t.TempDir()
			city, pack := filepath.Join(dir, "city"), filepath.Join(dir, "pack")
			writeTestFile(t, city, "city.toml", `
[workspace]
name = "example"
[[rigs]]
name = "demo"
path = "/tmp/demo"
[`+binding+`]
source = "../pack"
`)
			writeTestFile(t, pack, "pack.toml", "[pack]\nname = \"b\"\nschema = 2\n")
			writeTestFile(t, filepath.Join(pack, "agents", "source"), "agent.toml", "scope = \"city\"\nstart_command = \"true\"\n")
			for name, scope := range map[string]string{"reader": "rig", "observer": "city"} {
				writeTestFile(t, filepath.Join(pack, "agents", name), "agent.toml",
					"scope = \""+scope+"\"\nstart_command = \"true\"\nmail_wake_from = [\"source\", \"city:alpha\", \"human\"]\n")
			}
			cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(city, "city.toml"))
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateAgents(cfg.Agents); err != nil {
				t.Fatal(err)
			}
			got := 0
			for _, a := range cfg.Agents {
				if a.QualifiedName() != "demo/b.reader" && a.QualifiedName() != "b.observer" {
					continue
				}
				got++
				if !slices.Equal(a.MailWakeFrom, []string{"b.source", "city:alpha", "human"}) {
					t.Errorf("%s admits %q", a.QualifiedName(), a.MailWakeFrom)
				}
			}
			if got != 2 {
				t.Fatalf("found %d imported recipients", got)
			}
		})
	}
}
