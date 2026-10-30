package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

const startupHooksMenuFixture = "Hooks need review: 4 hooks are new or changed\n1. Review hooks\n2. Trust all and continue\n3. Continue without trusting (hooks won't run)"

func TestExecutionBackstopNamesStartupMenu(t *testing.T) {
	for _, delivered := range []bool{true, false} {
		t.Run(map[bool]string{true: "delivered", false: "undeliverable"}[delivered], func(t *testing.T) {
			f := newExecutionBackstopFixture(t)
			if !delivered {
				f.provider = f.sp
			}
			f.sp.SetPeekOutput(f.sessName, startupHooksMenuFixture)
			for i := 0; i <= idleClaimNudgeMaxAttempts+2; i++ {
				f.idleFor(t, 10*time.Minute)
				f.tick(t)
				f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
			}
			if !strings.Contains(f.stdout.String(), "Hooks need review: 4 hooks are new or changed") || strings.Contains(f.stdout.String(), "execution-stalled") {
				t.Fatalf("menu must be named in escalation: %s", f.stdout.String())
			}
			want := 0
			if delivered {
				want = 1
			}
			if len(f.drained) != want {
				t.Fatalf("drain policy changed: %v", f.drained)
			}
		})
	}
}

func TestStartupMenuDrainKeepsExecutionDrainSemantics(t *testing.T) {
	sp := runtime.NewFake()
	sp.SetPeekOutput("worker", startupHooksMenuFixture)
	reason, err := executionDrainReason(sp, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, "Hooks need review") {
		t.Fatalf("reason=%q", reason)
	}
	if drainReasonCancelable(reason) || pendingDrainReasonCancelable(reason) || liveClaimDrainReasonCancelable(reason) {
		t.Fatal("menu drain became cancelable")
	}
}
