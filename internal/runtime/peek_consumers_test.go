package runtime

import (
	"context"
	"testing"
	"time"
)

func TestStartupDialogsAcceptAttributedPrompt(t *testing.T) {
	withZeroDialogTimings(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads := 0
	err := AcceptStartupDialogsWithTimeout(ctx, time.Hour, func(int) (string, error) {
		reads++
		if reads > 32 {
			cancel()
		}
		return "\x1b[38;5;246m❯\u00a0\x1b[39m", nil
	}, func(keys ...string) error {
		t.Errorf("unexpected keys %v", keys)
		return nil
	})
	if err != nil {
		t.Fatalf("attributed ready prompt was not recognized: %v", err)
	}
}

func TestRateLimitScanAcceptsAttributes(t *testing.T) {
	wall := "\x1b[31m■ You've hit your usage limit.\x1b[0m"
	if !ContainsProviderRateLimitScreen(wall) || !ContainsActiveProviderRateLimitScreen(wall) {
		t.Fatal("attributed usage wall was not recognized")
	}
	if ContainsActiveProviderRateLimitScreen(wall + "\n\x1b[32m• Working again\x1b[0m") {
		t.Fatal("attributed recovery was ignored")
	}
	if got := ProviderTerminalErrorReason("\x1b[31mmodel_\x1b[0mnot_found"); got != "model_not_found" {
		t.Fatalf("terminal error = %q", got)
	}
}
