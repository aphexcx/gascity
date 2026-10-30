package hooks

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
)

func TestAppendCodexTrustPreservesConfig(t *testing.T) {
	// The incident's MCP transport is valid TOML but rejected by Codex's
	// config/batchWrite. Trust must not rewrite or validate unrelated settings.
	original := []byte("# operator comment\n[mcp_servers.node_repl]\ntransport = 'unknown-app-transport'\n")
	record := codexTrustRecord{Key: "/lane/.codex/hooks.json:session_start:0:0", Hash: "sha256:" + strings.Repeat("a", 64)}
	got, count, err := appendCodexTrust(original, []codexTrustRecord{record})
	if err != nil || count != 1 {
		t.Fatalf("append: count=%d err=%v", count, err)
	}
	if !bytes.HasPrefix(got, original) {
		t.Fatal("existing user configuration was rewritten")
	}
	var parsed struct {
		Hooks struct {
			State map[string]struct {
				TrustedHash string `toml:"trusted_hash"`
			}
		}
	}
	if _, err := toml.Decode(string(got), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Hooks.State[record.Key].TrustedHash != record.Hash {
		t.Fatal("missing exact Codex trust record")
	}
	again, count, err := appendCodexTrust(got, []codexTrustRecord{record})
	if err != nil || count != 0 || !bytes.Equal(again, got) {
		t.Fatalf("repeat must be a byte-preserving no-op: count=%d err=%v", count, err)
	}
}

func TestAppendCodexTrustPreservesExistingDecision(t *testing.T) {
	record := codexTrustRecord{Key: "/lane/.codex/hooks.json:session_start:0:0", Hash: "sha256:" + strings.Repeat("a", 64)}
	for _, existing := range []string{
		"[hooks.state.\"/lane/.codex/hooks.json:session_start:0:0\"]\nenabled = false\n",
		"[hooks.state.\"/lane/.codex/hooks.json:session_start:0:0\"]\ntrusted_hash = 'sha256:old'\n",
		"[broken",
	} {
		got, count, err := appendCodexTrust([]byte(existing), []codexTrustRecord{record})
		if err == nil || count != 0 || !bytes.Equal(got, []byte(existing)) {
			t.Fatalf("must preserve existing decision/malformed config: count=%d err=%v got=%q", count, err, got)
		}
	}
}

func TestManagedCodexTrustRequiresExactBundledDefinition(t *testing.T) {
	work := t.TempDir()
	raw, err := core.PackFS.ReadFile("overlay/per-provider/codex/.codex/hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]struct{ Hooks []struct{ Command string } }
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	command := doc.Hooks["SessionStart"][0].Hooks[0].Command
	original := codexHook{Key: "managed", CurrentHash: "sha256:" + strings.Repeat("a", 64), SourcePath: filepath.Join(work, ".codex", "hooks.json"), EventName: "sessionStart", HandlerType: "command", Command: command, Enabled: true, TimeoutSec: 600}
	for _, kind := range []string{"bundled", "appended shell", "user hook", "disabled", "async", "timeout", "matcher"} {
		t.Run(kind, func(t *testing.T) {
			hook := original
			switch kind {
			case "appended shell":
				hook.Command += "; echo custom"
			case "user hook":
				hook.SourcePath = "/home/.codex/hooks.json"
			case "disabled":
				hook.Enabled = false
			case "async":
				hook.Async = true
			case "timeout":
				hook.TimeoutSec = 1
			case "matcher":
				matcher := "custom"
				hook.Matcher = &matcher
			}
			payload, err := json.Marshal(map[string]any{"data": []any{map[string]any{"cwd": work, "hooks": []codexHook{hook}}}})
			if err != nil {
				t.Fatal(err)
			}
			var listed codexHooksList
			if err := json.Unmarshal(payload, &listed); err != nil {
				t.Fatal(err)
			}
			records, err := managedCodexTrustRecords(listed, t.TempDir(), work)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if kind == "bundled" {
				want = 1
			}
			if len(records) != want {
				t.Fatalf("trusted %d definitions, want %d", len(records), want)
			}
		})
	}
	if _, err := managedCodexTrustRecords(codexHooksList{}, t.TempDir(), work); err == nil {
		t.Fatal("missing inspection must fail")
	}
}

func TestCodexInspectionKeepsLauncherAndDropsRuntimeFlags(t *testing.T) {
	got, err := codexInspectionArgs(`CODEX_HOME='/account home' '/wrapper path/codex' -p city --dangerously-bypass-approvals-and-sandbox --model gpt-6-astra -c 'features.hooks=true'`)
	want := []string{"CODEX_HOME=/account home", "/wrapper path/codex", "-c", "features.hooks=true", "app-server"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%q err=%v", got, err)
	}
	for _, invalid := range []string{"", "codex --profile", "cd /lane && codex", "'unclosed"} {
		if _, err := codexInspectionArgs(invalid); err == nil {
			t.Fatalf("accepted unsupported launcher %q", invalid)
		}
	}
}
