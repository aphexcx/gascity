package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestConfigLoadHealthDeduplicatesUntilRecovery(t *testing.T) {
	health := &configLoadHealth{}
	start := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	first := errors.New("import first: dirty; run gc import install")
	second := errors.New("import second: dirty; run gc import install")
	if !health.record(first, start) || health.record(first, start.Add(time.Minute)) {
		t.Fatal("repeated failure should only be logged once")
	}
	if got := health.snapshot(); got.Error != first.Error() || !got.Since.Equal(start) {
		t.Fatalf("failure = %+v", got)
	}
	if !health.record(second, start.Add(2*time.Minute)) || health.record(first, start.Add(3*time.Minute)) {
		t.Fatal("distinct failures should only be logged once per incident")
	}
	if got := health.snapshot(); !got.Since.Equal(start) {
		t.Fatalf("repeated failure lost first-seen time: %+v", got)
	}
	health.clear()
	if health.snapshot() != nil || !health.record(first, start.Add(4*time.Minute)) {
		t.Fatal("recovery should clear health and allow a new incident")
	}
}

func TestControllerConfigLoadStatusWithoutReconcile(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	health := &configLoadHealth{}
	health.record(errors.New("import second: dirty"), time.Unix(123, 0).UTC())
	server, client := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleControllerConn(server, "unused", controllerHostingSupervisor, func() {}, nil, nil, nil, nil, nil, nil, health)
	}()
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte(configLoadStatusCommand + "\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(client).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var reply configLoadStatusReply
	if err := json.Unmarshal(line, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Failure == nil || reply.Failure.Error != "import second: dirty" || reply.Failure.Since.Unix() != 123 {
		t.Fatalf("reply = %+v", reply)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("controller did not finish")
	}
}

func TestCityStatusConfigLoadFailureIncludesSince(t *testing.T) {
	cityPath := t.TempDir()
	writeCityToml(t, cityPath, "[workspace\n")
	old := readConfigLoadFailureHook
	t.Cleanup(func() { readConfigLoadFailureHook = old })
	readConfigLoadFailureHook = func(string) (*configLoadFailure, error) {
		return &configLoadFailure{Error: "city import second: dirty; run gc import install", Since: time.Unix(123, 0).UTC()}, nil
	}
	for _, jsonOutput := range []bool{false, true} {
		var stdout, stderr bytes.Buffer
		code := cmdCityStatus([]string{cityPath}, jsonOutput, &stdout, &stderr)
		output := stdout.String() + stderr.String()
		if code == 0 || !strings.Contains(output, "1970-01-01T00:02:03Z") || !strings.Contains(output, "city import second") || !strings.Contains(output, "keeping last good config") {
			t.Fatalf("json=%v code=%d output=%s", jsonOutput, code, output)
		}
	}
}
