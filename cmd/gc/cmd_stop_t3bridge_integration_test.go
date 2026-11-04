//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/t3bridge"
	"github.com/gorilla/websocket"
)

func TestCmdStopConfiguredT3PreservedHistory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		force bool
		live  bool
	}{
		{name: "graceful"},
		{name: "forced", force: true},
		{name: "live survivor", force: true, live: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.live {
				oldBound := stopPerTargetTimeoutDefault
				stopPerTargetTimeoutDefault = 100 * time.Millisecond
				t.Cleanup(func() { stopPerTargetTimeoutDefault = oldBound })
			}
			cityDir := t.TempDir()
			one := 1
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test"}, Beads: config.BeadsConfig{Provider: "file"},
				Session: config.SessionConfig{Provider: "t3bridge"}, Daemon: config.DaemonConfig{ShutdownTimeout: "1ns"},
				Agents: []config.Agent{{Name: "worker", MaxActiveSessions: &one}},
			}
			writeStopLifecycleCityConfig(t, cityDir, cfg)
			// A configured pool runtime may outlive its session bead. Its thread
			// remains owned after shutdown, with only the session status changed.
			env, err := json.Marshal(map[string]string{"GC_CITY_PATH": cityDir, "GC_SESSION_ID": "previous-session"})
			if err != nil {
				t.Fatal(err)
			}
			thread := map[string]interface{}{
				"id": "thread-worker", "projectId": "project-1",
				"customMetadata": map[string]interface{}{"gc.sessionName": "worker", "gc.sessionEnv": string(env)},
				"session":        map[string]interface{}{"status": "ready"},
			}
			snapshot := map[string]interface{}{"threads": []interface{}{thread}}
			var mu sync.Mutex
			var operations []string
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/api/auth/") {
					w.Header().Set("Content-Type", "application/json")
					if err := json.NewEncoder(w).Encode(map[string]string{"token": "test-token", "ticket": "test-ticket", "expiresAt": "2099-01-01T00:00:00Z"}); err != nil {
						t.Errorf("auth response: %v", err)
					}
					return
				}
				if r.URL.Path == "/api/orchestration/snapshot" {
					mu.Lock()
					defer mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					if err := json.NewEncoder(w).Encode(snapshot); err != nil {
						t.Errorf("snapshot response: %v", err)
					}
					return
				}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade: %v", err)
					return
				}
				defer func() { _ = conn.Close() }()
				var req struct {
					ID      string                 `json:"id"`
					Tag     string                 `json:"tag"`
					Payload map[string]interface{} `json:"payload"`
				}
				if err := conn.ReadJSON(&req); err != nil {
					t.Errorf("request: %v", err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				value := map[string]interface{}{}
				switch req.Tag {
				case "orchestration.getSnapshot":
					value = snapshot
				case "orchestration.dispatchCommand":
					command := req.Payload
					if nested, ok := command["command"].(map[string]interface{}); ok {
						command = nested
					}
					typ, _ := command["type"].(string)
					operations = append(operations, typ)
					if typ == "thread.session.stop" {
						if command["threadId"] != "thread-worker" {
							t.Errorf("stop targeted %v", command["threadId"])
						}
						if !tc.live {
							thread["session"] = map[string]interface{}{"status": "stopped"}
						}
					}
				default:
					t.Errorf("unexpected RPC %q", req.Tag)
				}
				if err := conn.WriteJSON(map[string]interface{}{
					"_tag": "Exit", "requestId": req.ID,
					"exit": map[string]interface{}{"_tag": "Success", "value": value},
				}); err != nil {
					t.Errorf("RPC response: %v", err)
				}
			}))
			defer server.Close()
			t.Setenv("GC_EXEC_STATE_DIR", t.TempDir())
			t.Setenv("T3_BEARER_TOKEN", "test-bearer")
			t.Setenv("T3_HOME", t.TempDir())
			t.Setenv("T3_WS_URL", "ws"+strings.TrimPrefix(server.URL, "http")+"/ws")
			t.Setenv("GC_T3BRIDGE_STATE_DIR", t.TempDir())
			sp := t3bridge.NewSeamBacked()
			oldFactory := sessionProviderForStopCity
			t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
			sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return sp, nil }
			overrideShutdownBeadsProviderForStop(t, func(string) error { return nil })
			var stdout, stderr bytes.Buffer
			started := time.Now()
			code := cmdStopBody(cityDir, cfg, tc.force, &stdout, &stderr)
			elapsed := time.Since(started)
			if tc.live {
				if code != 1 || !strings.Contains(stderr.String(), `city-owned session "worker" remains after shutdown`) || elapsed < stopPerTargetTimeoutDefault {
					t.Fatalf("survivor: code=%d elapsed=%s stdout=%q stderr=%q", code, elapsed, stdout.String(), stderr.String())
				}
			} else if code != 0 || elapsed >= stopPerTargetTimeoutDefault/2 || !strings.Contains(stdout.String(), "City stopped.") {
				t.Fatalf("preserved history: code=%d elapsed=%s bound=%s stdout=%q stderr=%q", code, elapsed, stopPerTargetTimeoutDefault, stdout.String(), stderr.String())
			}
			owner, id, err := sp.(interface {
				SessionOwnership(string) (string, string, error)
			}).SessionOwnership("worker")
			if err != nil || owner != cityDir || id != "previous-session" {
				t.Fatalf("history ownership changed: city=%q id=%q err=%v", owner, id, err)
			}
			mu.Lock()
			defer mu.Unlock()
			want := "thread.session.stop"
			if !tc.force {
				want = "thread.turn.interrupt," + want
			}
			var shutdowns []string
			for _, op := range operations {
				if op == "thread.session.stop" || op == "thread.turn.interrupt" {
					shutdowns = append(shutdowns, op)
				}
				if strings.Contains(op, "delete") || strings.Contains(op, "archive") {
					t.Errorf("history mutation: %s", op)
				}
			}
			if got := strings.Join(shutdowns, ","); got != want {
				t.Errorf("shutdowns=%q want=%q (all=%s)", got, want, fmt.Sprint(operations))
			}
		})
	}
}
