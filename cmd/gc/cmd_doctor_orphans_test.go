package main

import (
	"io"
	"strings"
	"testing"
)

func TestDoctorFixOrphansRequiresFix(t *testing.T) {
	cmd := newDoctorCmd(io.Discard, io.Discard)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--fix-orphans"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--fix-orphans requires --fix") {
		t.Fatalf("Execute() = %v, want missing --fix error before city discovery", err)
	}
}
