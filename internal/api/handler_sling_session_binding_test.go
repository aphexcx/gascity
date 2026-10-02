package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

func TestSlingDurablePoolOwnerInSessionBinding(t *testing.T) {
	for _, tc := range []struct {
		name     string
		template string
		missing  bool
		readErr  error
		wantCode int
	}{
		{name: "own session", template: "myrig/worker", wantCode: http.StatusOK},
		{name: "foreign template", template: "myrig/other", wantCode: http.StatusBadRequest},
		{name: "missing session", missing: true, wantCode: http.StatusBadRequest},
		{name: "session read failure", template: "myrig/worker", readErr: errors.New("session binding unavailable"), wantCode: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, state := newSlingTestServer(t)
			state.cfg.Agents[0].MaxActiveSessions = intPtr(2)
			state.cfg.NamedSessions = nil
			sessions := beads.NewMemStore()
			sessions.HonorExplicitIDs = true
			state.sessionsBeadStore = sessions
			const ownerID = "gc-session-owner"
			work := state.stores["myrig"]
			before, err := work.Create(beads.Bead{
				Title:    "already claimed task",
				Type:     "task",
				Assignee: ownerID,
				Metadata: map[string]string{
					beadmeta.RoutedToMetadataKey:  "myrig/worker",
					beadmeta.SessionIDMetadataKey: ownerID,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			status := "in_progress"
			if err := work.Update(before.ID, beads.UpdateOpts{Status: &status}); err != nil {
				t.Fatal(err)
			}
			before, err = work.Get(before.ID)
			if err != nil {
				t.Fatal(err)
			}
			var owner beads.Bead
			if !tc.missing {
				owner, err = sessions.Create(beads.Bead{
					ID:     ownerID,
					Type:   session.BeadType,
					Labels: []string{session.LabelSession},
					Metadata: map[string]string{
						"template":                             tc.template,
						"state":                                "active",
						"session_name":                         "worker-alpha",
						beadmeta.CurrentClaimBeadIDMetadataKey: before.ID,
					},
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if tc.readErr != nil {
				state.sessionsBeadStore = &getErrStore{Store: sessions, err: tc.readErr}
			}
			var body bytes.Buffer
			if err := json.NewEncoder(&body).Encode(slingBody{Target: "myrig/worker", Bead: before.ID, NoConvoy: true}); err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), &body))
			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			after, err := work.Get(before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Errorf("claimed work changed: before=%+v after=%+v", before, after)
			}
			if !tc.missing {
				afterOwner, err := sessions.Get(owner.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(afterOwner, owner) {
					t.Errorf("session claim changed: before=%+v after=%+v", owner, afterOwner)
				}
			}
		})
	}
}
