package lanectx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLaneDiagnosticIsBoundedAndAppendOnly(t *testing.T) {
	lane := t.TempDir()
	env := map[string]string{"GC_DIR": lane, "GC_PRESTART_LOG": filepath.Join(lane, ".gc", "pre-start.log")}
	chunk := strings.Repeat("x", 32*1024)
	for i := 0; i < 150; i++ {
		if err := AppendPreStartDiagnostic(env, chunk, nil); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(env["GC_PRESTART_LOG"])
	if err != nil {
		t.Fatal(err)
	}
	if len(before) > 1024*1024 || !strings.Contains(string(before), "capacity reached") {
		t.Fatalf("diagnostic must stop at a marked 1 MiB cap; length=%d", len(before))
	}
	if err := AppendPreStartDiagnostic(env, "after cap", nil); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(env["GC_PRESTART_LOG"])
	if err != nil || string(after) != string(before) {
		t.Fatalf("full log was replaced or grew: error=%v, length=%d", err, len(after))
	}
}

func TestLaneArtifactsRejectEscapingSymlinks(t *testing.T) {
	for _, leaf := range []string{".gc", ".gc/pre-start.log", ".gc/worker-env.sh"} {
		t.Run(leaf, func(t *testing.T) {
			lane := t.TempDir()
			outside := t.TempDir()
			if leaf != ".gc" {
				if err := os.Mkdir(filepath.Join(lane, ".gc"), 0o700); err != nil {
					t.Fatal(err)
				}
				outside = filepath.Join(outside, "untouched")
				if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(outside, filepath.Join(lane, leaf)); err != nil {
				t.Fatal(err)
			}
			env := map[string]string{"GC_DIR": lane, "GC_PRESTART_LOG": filepath.Join(lane, ".gc", "pre-start.log")}
			if leaf == ".gc/worker-env.sh" {
				// Atomic replacement may replace the link itself, but never its target.
				if err := WriteLaneEnvironment(env); err != nil {
					t.Fatal(err)
				}
			} else if err := AppendPreStartDiagnostic(env, "outside write", nil); err == nil {
				t.Fatal("escaping diagnostic path was accepted")
			}
			if leaf != ".gc" {
				got, err := os.ReadFile(outside)
				if err != nil || string(got) != "keep" {
					t.Fatalf("outside target changed: %q, %v", got, err)
				}
			}
		})
	}
}

func TestLaneArtifactsRejectRigRoot(t *testing.T) {
	root := t.TempDir()
	env := map[string]string{"GC_DIR": root, "GC_RIG_ROOT": root, "GC_PRESTART_LOG": filepath.Join(root, ".gc", "pre-start.log")}
	if err := WriteLaneEnvironment(env); err == nil {
		t.Fatal("must not write a lane artifact in the rig root")
	}
	if _, err := os.Stat(filepath.Join(root, ".gc")); !os.IsNotExist(err) {
		t.Fatalf("rig root was modified: %v", err)
	}
}
