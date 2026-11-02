//go:build integration

package tmux

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/test/tmuxtest"
)

// A missing session must never resolve to another session with the same prefix.
func TestExactSessionTargetsRealTmux(t *testing.T) {
	for _, operation := range []string{"kill missing", "stop missing", "read environment", "write environment", "send keys", "stop present"} {
		t.Run(operation, func(t *testing.T) {
			guard := tmuxtest.NewGuard(t)
			cfg := DefaultConfig()
			cfg.SocketName = guard.SocketName()
			p := NewProviderWithConfig(cfg)
			tm := p.Tmux()
			t.Logf("private socket: %s", cfg.SocketName)
			if err := tm.NewSessionWithCommandAndEnv("abc-foreign", t.TempDir(), "exec cat", map[string]string{"GC_EXACT_TARGET": "foreign"}); err != nil {
				t.Fatal(err)
			}
			if present, err := tm.HasSession("abc"); err != nil || present {
				t.Fatalf("fixture abc present = %v, error = %v", present, err)
			}
			switch operation {
			case "kill missing":
				if err := tm.KillSession("abc"); !errors.Is(err, ErrSessionNotFound) {
					t.Errorf("KillSession(absent) = %v, want ErrSessionNotFound", err)
				}
			case "stop missing":
				// Provider.Stop retains its idempotent missing-session contract.
				if err := p.Stop("abc"); err != nil {
					t.Errorf("Stop(absent) = %v, want nil", err)
				}
			case "read environment":
				if env, err := p.GetAllEnvironment("abc"); !errors.Is(err, ErrSessionNotFound) || len(env) != 0 {
					t.Errorf("GetAllEnvironment(absent) returned %d variables, error = %v; want ErrSessionNotFound", len(env), err)
				}
			case "write environment":
				if err := tm.SetEnvironment("abc", "GC_EXACT_TARGET", "changed"); !errors.Is(err, ErrSessionNotFound) {
					t.Errorf("SetEnvironment(absent) = %v, want ErrSessionNotFound", err)
				}
				if got, err := tm.GetEnvironment("abc-foreign", "GC_EXACT_TARGET"); err != nil || got != "foreign" {
					t.Errorf("foreign environment = %q, %v; want foreign", got, err)
				}
			case "send keys":
				if err := tm.SendKeysRaw("abc", "C-c"); !errors.Is(err, ErrSessionNotFound) {
					t.Errorf("SendKeysRaw(absent) = %v, want ErrSessionNotFound", err)
				}
				if err := p.SendKeys("abc", "C-c"); err != nil {
					t.Errorf("SendKeys(absent) = %v, want best-effort nil", err)
				}
			case "stop present":
				if err := tm.NewSessionWithCommand("abc", t.TempDir(), "exec cat"); err != nil {
					t.Fatal(err)
				}
				if err := p.Stop("abc"); err != nil {
					t.Errorf("Stop(present) = %v", err)
				}
				if present, err := tm.HasSession("abc"); err != nil || present {
					t.Errorf("stopped abc present = %v, error = %v", present, err)
				}
			}
			if present, err := tm.HasSession("abc-foreign"); err != nil || !present {
				t.Errorf("foreign session survived = %v, error = %v", present, err)
			}
			if dead, err := tm.IsPaneDead("abc-foreign"); err != nil || dead {
				t.Errorf("foreign process dead = %v, error = %v", dead, err)
			}
		})
	}
}
