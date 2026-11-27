package main

import (
	"io"
	"strings"
	"testing"
)

func TestServeRequiresHub(t *testing.T) {
	t.Setenv("GC_MAIL_RELAY_HUB", "")
	cmd := newCommand()
	cmd.SetArgs([]string{"serve"})
	cmd.SetOut(io.Discard)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "hub") {
		t.Fatalf("serve without hub: %v", err)
	}
}
