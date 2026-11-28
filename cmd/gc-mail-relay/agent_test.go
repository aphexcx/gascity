package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func agentRoot(env map[string]string) (*cobra.Command, *bytes.Buffer) {
	root := &cobra.Command{Use: "gc-mail-relay", SilenceErrors: true, SilenceUsage: true}
	root.AddCommand(newAgentCommands(func(k string) string { return env[k] })...)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	return root, &out
}

// cityFiles writes a signing key with the given mode and a peers folder.
func cityFiles(t *testing.T, mode os.FileMode) (dir, key string) {
	t.Helper()
	dir = t.TempDir()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key = filepath.Join(dir, "signing.key")
	if err := os.WriteFile(key, []byte(base64.StdEncoding.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(key, mode); err != nil {
		t.Fatal(err)
	}
	peer, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "peers"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "peers", "hub.pub"), []byte(base64.StdEncoding.EncodeToString(peer)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, key
}

func settingArgs(dir, key string) []string {
	return []string{
		"--city=alpha", "--signing-key=" + key, "--encryption-keys-dir=" + filepath.Join(dir, "keys"),
		"--peers-dir=" + filepath.Join(dir, "peers"), "--state-dir=" + filepath.Join(dir, "state"),
		"--relay-url=https://relay.invalid", "--api-url=http://127.0.0.1:1", "--gc-city=alpha",
	}
}

// A3 at the command line: each verb exits non-zero with one plain line and
// does nothing when the signing key is open to group or others.
func TestAgentVerbsRefuseOpenSigningKey(t *testing.T) {
	for _, args := range [][]string{{"keygen"}, {"send", "hub/writer", "-s", "subject"}, {"run"}} {
		t.Run(args[0], func(t *testing.T) {
			dir, key := cityFiles(t, 0o644)
			before := digest(t, key)
			root, out := agentRoot(nil)
			root.SetArgs(append(args, settingArgs(dir, key)...))
			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), "signing key") || strings.Contains(err.Error(), "\n") {
				t.Fatalf("%s: %v", args[0], err)
			}
			if out.Len() != 0 {
				t.Fatalf("%s printed %q", args[0], out.String())
			}
			for _, made := range []string{"keys", "state"} {
				if _, err := os.Stat(filepath.Join(dir, made)); !os.IsNotExist(err) {
					t.Fatalf("%s made %s: %v", args[0], made, err)
				}
			}
			if digest(t, key) != before {
				t.Fatal("the signing key file changed")
			}
		})
	}
}

// The send verb exits non-zero for a subject the city's store could never
// take, naming the rule it broke, and makes nothing. The relay address is a
// closed loopback port, so nothing could leave the machine.
func TestSendVerbRefusesUnfileableSubject(t *testing.T) {
	for _, tc := range []struct{ name, subject, rule string }{
		{"blank", " ", "blank"},
		{"starts with a dash", "-1 from me", "dash"},
		{"over 500 bytes", strings.Repeat("s", 501), "500 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, key := cityFiles(t, 0o600)
			root, out := agentRoot(nil)
			args := append([]string{"send", "hub/writer", "--subject=" + tc.subject}, settingArgs(dir, key)...)
			root.SetArgs(append(args, "--relay-url=http://127.0.0.1:1"))
			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.rule) || strings.Contains(err.Error(), "\n") {
				t.Fatalf("send: %v, want one line naming %q", err, tc.rule)
			}
			if out.Len() != 0 {
				t.Fatalf("send printed %q", out.String())
			}
			if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
				t.Fatalf("send made its state folder: %v", err)
			}
		})
	}
}

// Settings reach keygen from a settings file named in the environment.
func TestKeygenCommandReadsSettingsFile(t *testing.T) {
	dir, key := cityFiles(t, 0o600)
	file := filepath.Join(dir, "agent.toml")
	content := "city = \"alpha\"\nsigning-key = \"" + key + "\"\nencryption-keys-dir = \"" + filepath.Join(dir, "keys") + "\"\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	root, out := agentRoot(map[string]string{"GC_MAIL_RELAY_CONFIG": file})
	root.SetArgs([]string{"keygen"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "public-key ") || !strings.Contains(out.String(), "key-id ") {
		t.Fatalf("keygen printed %q", out.String())
	}
	matches, err := filepath.Glob(filepath.Join(dir, "keys", "*.key"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("key files %v, %v", matches, err)
	}
}

func TestDefaultSender(t *testing.T) {
	if got := defaultSender(func(string) string { return "" }); got != "human" {
		t.Fatalf("no identity: %q", got)
	}
	env := map[string]string{"GC_SESSION_ID": "s-1", "GC_AGENT": "rig/worker"}
	if got := defaultSender(func(k string) string { return env[k] }); got != "rig/worker" {
		t.Fatalf("agent before session id: %q", got)
	}
}

func digest(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}
