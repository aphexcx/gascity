package herdr

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSessionOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, city, id              string
		live, probeError, readError bool
	}{
		{name: "owned", city: "/city", id: "gc-1"},
		{name: "missing city", id: "gc-1", live: true},
		{name: "missing id", city: "/city", live: true},
		{name: "absent"},
		{name: "probe failure", probeError: true},
		{name: "sidecar failure", readError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, state := newFakeHerdrProvider(t)
			owner, ok := any(p).(interface {
				SessionOwnership(string) (string, string, error)
			})
			if !ok {
				t.Fatal("registered herdr provider does not expose ownership")
			}
			for key, value := range map[string]string{"GC_CITY_PATH": tc.city, "GC_SESSION_ID": tc.id} {
				if value != "" {
					if err := p.SetMeta("session", key, value); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.live {
				if err := os.WriteFile(filepath.Join(state, "registered"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.probeError {
				p.c.bin = filepath.Join(state, "missing-herdr")
			}
			if tc.readError {
				if err := os.MkdirAll(filepath.Join(p.metaDir, "session", "GC_CITY_PATH"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			city, id, err := owner.SessionOwnership("session")
			switch {
			case tc.probeError || tc.readError:
				if err == nil || runtime.IsSessionNotFound(err) {
					t.Fatalf("uncertain ownership = %q, %q, %v", city, id, err)
				}
			case tc.city != "" && tc.id != "":
				if city != tc.city || id != tc.id || err != nil {
					t.Fatalf("owned = %q, %q, %v", city, id, err)
				}
			case tc.live:
				if city != "" || id != "" || err != nil {
					t.Fatalf("unknown = %q, %q, %v", city, id, err)
				}
			default:
				if !runtime.IsSessionNotFound(err) {
					t.Fatalf("absent = %q, %q, %v", city, id, err)
				}
			}
		})
	}
}

// TestStampIdentityMeta pins the start-time identity stamp: the reconciler
// binds a live runtime to its session bead by probing GC_SESSION_ID /
// GC_INSTANCE_TOKEN / GC_RUNTIME_EPOCH via GetMeta, so Start must make them
// readable the moment the agent exists — not after the (up to a minute)
// startup delivery completes. An empty or missing env key stays unstamped.
func TestStampIdentityMeta(t *testing.T) {
	p := New("gctest-identity-meta", t.TempDir(), t.TempDir(), 0, 0)
	p.stampIdentityMeta("canary", map[string]string{
		"GC_SESSION_ID":     "gm-abc123",
		"GC_INSTANCE_TOKEN": "tok-1",
		"GC_RUNTIME_EPOCH":  "3",
		"GC_CITY":           "not-an-identity-key",
	})
	for key, want := range map[string]string{
		"GC_SESSION_ID":     "gm-abc123",
		"GC_INSTANCE_TOKEN": "tok-1",
		"GC_RUNTIME_EPOCH":  "3",
		"GC_CITY":           "",
	} {
		got, err := p.GetMeta("canary", key)
		if err != nil {
			t.Fatalf("GetMeta(%s): %v", key, err)
		}
		if got != want {
			t.Errorf("GetMeta(%s) = %q, want %q", key, got, want)
		}
	}

	p.stampIdentityMeta("empty", map[string]string{"GC_SESSION_ID": ""})
	if got, _ := p.GetMeta("empty", "GC_SESSION_ID"); got != "" {
		t.Errorf("empty env value stamped as %q, want unstamped", got)
	}
}
