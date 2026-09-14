//go:build integration

package tmux

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/test/tmuxtest"
)

// The first new-session client must not stamp its caller's session identity
// onto the shared server, where an orphan scan could mistake it for an agent.
func TestNewSessionKeepsIdentityOutOfServer(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("process environment inspection requires Darwin or Linux")
	}
	guard := tmuxtest.NewGuard(t)
	cfg := DefaultConfig()
	cfg.SocketName = guard.SocketName()
	tm := NewTmuxWithConfig(cfg)
	identity := map[string]string{
		"GC_SESSION_ID":         "ci-test-session",
		"GC_SESSION_NAME":       "test-session",
		"GC_INSTANCE_TOKEN":     "test-instance",
		"GC_AGENT":              "test-agent",
		"GC_ALIAS":              "test-alias",
		"GC_TEMPLATE":           "test-template",
		"GC_SESSION_ORIGIN":     "manual",
		"GC_RUNTIME_EPOCH":      "3",
		"GC_CONTINUATION_EPOCH": "2",
	}
	for key := range identity {
		t.Setenv(key, "caller-identity")
	}
	const name = "first-session"
	if err := tm.NewSessionWithCommandAndEnv(name, t.TempDir(), "exec sleep 300", identity); err != nil {
		t.Fatal(err)
	}
	serverPID, err := tm.run("display-message", "-p", "-t", name, "#{pid}")
	if err != nil {
		t.Fatal(err)
	}
	serverEnv := processEnvForIdentityTest(t, serverPID)
	for key, value := range identity {
		if _, ok := serverEnv[key]; ok {
			t.Errorf("tmux server inherited %s", key)
		}
		got, err := tm.run("show-environment", "-t", name, key)
		if err != nil || got != key+"="+value {
			t.Errorf("session env %s missing explicit value: %v", key, err)
		}
	}
}

func processEnvForIdentityTest(t *testing.T, pid string) map[string]string {
	t.Helper()
	var fields []string
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile("/proc/" + pid + "/environ")
		if err != nil {
			t.Fatal(err)
		}
		fields = strings.Split(string(data), "\x00")
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), testutil.ExecRaceTimeout)
		defer cancel()
		// ps eww includes argv before the environment. The server keeps its
		// original new-session argv, including -e KEY=value flags; those are
		// session values, not the server's process environment.
		var outputs []string
		for _, flags := range []string{"ww", "eww"} {
			data, err := exec.CommandContext(ctx, "ps", flags, "-o", "command=", "-p", pid).Output()
			if err != nil {
				t.Fatal(err)
			}
			outputs = append(outputs, strings.TrimSpace(string(data)))
		}
		if outputs[0] == "" {
			t.Fatal("process command disappeared during environment inspection")
		}
		environment, ok := strings.CutPrefix(outputs[1], outputs[0])
		if !ok {
			t.Fatal("process command changed during environment inspection")
		}
		fields = strings.Fields(environment)
	}
	env := make(map[string]string)
	for _, field := range fields {
		if key, value, ok := strings.Cut(field, "="); ok {
			env[key] = value
		}
	}
	return env
}
