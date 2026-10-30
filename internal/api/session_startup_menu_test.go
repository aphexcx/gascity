package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSessionResponseShowsStartupMenu(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "worker")
	fs.sp.SetPeekOutput(info.SessionName, "Hooks need review\n› 1. Review hooks\n  2. Trust all and continue\n  3. Continue without trusting\nPress enter to confirm"+strings.Repeat("\n", 30))
	h := newTestCityHandler(t, fs)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", cityURL(fs, "/sessions?peek=true"), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var response struct {
		Items []sessionResponse `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 1 || response.Items[0].State != "parked" || response.Items[0].Reason != "Hooks need review" {
		t.Fatalf("response=%+v, want parked with menu heading", response)
	}
}

func TestSessionPreviewTailPreservesRequestedWindow(t *testing.T) {
	for _, text := range []string{"one\ntwo\nthree", "one\ntwo\nthree\n"} {
		if got := sessionPreviewTail(text, 2); got != strings.TrimPrefix(text, "one\n") {
			t.Fatalf("tail=%q", got)
		}
		if got := sessionPreviewTail(text, 4); got != text {
			t.Fatalf("short preview=%q", got)
		}
	}
}
