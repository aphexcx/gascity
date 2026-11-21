package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

func TestManagedReservedSessionSuspend(t *testing.T) {
	for _, surface := range []string{"huma", "legacy"} {
		t.Run(surface, func(t *testing.T) {
			fs := newSessionFakeState(t)
			fs.cfg.Agents[0].Dir = ""
			fs.cfg.NamedSessions[0].Dir = ""
			srv := New(&restartControllerState{fs})
			h := newTestCityHandlerWith(t, fs, srv)
			rec := httptest.NewRecorder()
			req := newPostRequest(cityURL(fs, "/session/worker/suspend"), nil)
			if surface == "legacy" {
				req.SetPathValue("id", "worker")
				srv.handleSessionSuspend(rec, req)
			} else {
				h.ServeHTTP(rec, req)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("suspend status=%d, want 200; body: %s", rec.Code, rec.Body.String())
			}
			all, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(all) != 1 {
				t.Fatalf("session beads=%d, want one canonical bead", len(all))
			}
			if got := all[0].Metadata["state"]; got != "suspended" {
				t.Fatalf("state=%q, want suspended", got)
			}
			if fs.sp.IsRunning(all[0].Metadata["session_name"]) {
				t.Fatal("suspended runtime is still running")
			}
		})
	}
}
