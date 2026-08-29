package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/extmsg"
)

// runInline is a runBackground stand-in that runs the fan-out on its own
// goroutine with a plain background context, like the real one minus the
// server's task group.
func runInline(run func(context.Context)) { go run(context.Background()) }

func waitForReceiptState(t *testing.T, store *extmsg.InboundReceiptStore, id string, want extmsg.InboundReceiptState) extmsg.InboundReceiptStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := store.Lookup("c", id)
		if got.State == want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("receipt %s never reached state %q; last %+v", id, want, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAwaitInboundFanoutRecordsLateConclusionForPolling is the mechanism
// behind the whole bead: a fan-out that outruns the response budget still
// answers "pending" synchronously, but its eventual outcome is now RECORDED
// against the receipt id so a consumer that held on the pending receipt can
// learn later whether the send landed (pending → landed late).
func TestAwaitInboundFanoutRecordsLateConclusionForPolling(t *testing.T) {
	store := extmsg.NewInboundReceiptStore()
	release := make(chan struct{})
	fanout := func(context.Context) ([]extmsg.InboundDeliveryMember, error) {
		<-release
		return []extmsg.InboundDeliveryMember{{
			SessionID: "s1", Status: extmsg.InboundDeliveryDelivered, DeliveredBytes: 223, ExpectedBytes: 223,
		}}, nil
	}

	got := awaitInboundFanout(context.Background(), store, "c", 20*time.Millisecond, runInline, fanout, "slack/C1")
	if got.Status != extmsg.InboundDeliveryPending {
		t.Fatalf("response status = %q, want pending while the fan-out is still running", got.Status)
	}
	if got.ReceiptID == "" {
		t.Fatal("pending receipt carries no receipt id — nothing to poll on")
	}
	if st := store.Lookup("c", got.ReceiptID); st.State != extmsg.InboundReceiptPending {
		t.Fatalf("store state while fan-out runs = %q, want pending", st.State)
	}

	close(release)
	st := waitForReceiptState(t, store, got.ReceiptID, extmsg.InboundReceiptConcluded)
	if st.Delivery == nil || st.Delivery.Status != extmsg.InboundDeliveryDelivered {
		t.Fatalf("late conclusion = %+v, want delivered recorded against the receipt", st.Delivery)
	}
	if st.Delivery.ReceiptID != got.ReceiptID || st.Delivery.DeliveredBytes != 223 {
		t.Fatalf("late conclusion does not describe the same delivery: %+v", st.Delivery)
	}
}

// TestAwaitInboundFanoutRecordsLateFailureForPolling is the other half
// (pending → lost): when the background sends conclude that nothing landed,
// the consumer must be able to read that definite answer instead of
// inferring loss from silence.
func TestAwaitInboundFanoutRecordsLateFailureForPolling(t *testing.T) {
	store := extmsg.NewInboundReceiptStore()
	release := make(chan struct{})
	fanout := func(context.Context) ([]extmsg.InboundDeliveryMember, error) {
		<-release
		return nil, errors.New("membership lookup: store unavailable")
	}

	got := awaitInboundFanout(context.Background(), store, "c", 20*time.Millisecond, runInline, fanout, "slack/C1")
	if got.Status != extmsg.InboundDeliveryPending {
		t.Fatalf("response status = %q, want pending", got.Status)
	}
	close(release)
	st := waitForReceiptState(t, store, got.ReceiptID, extmsg.InboundReceiptConcluded)
	if st.Delivery == nil || st.Delivery.Status != extmsg.InboundDeliveryFailed {
		t.Fatalf("late failure = %+v, want failed recorded against the receipt", st.Delivery)
	}
}

// A fan-out that concludes INSIDE the budget is recorded too, with the very
// delivery the response carried, so a consumer polling an id it already saw
// concluded gets a consistent answer rather than "unknown".
func TestAwaitInboundFanoutRecordsInBudgetConclusion(t *testing.T) {
	store := extmsg.NewInboundReceiptStore()
	fanout := func(context.Context) ([]extmsg.InboundDeliveryMember, error) {
		return nil, nil
	}
	got := awaitInboundFanout(context.Background(), store, "c", time.Second, runInline, fanout, "slack/C1")
	if got.Status != extmsg.InboundDeliveryNoRoute {
		t.Fatalf("response status = %q, want no_route for an empty membership", got.Status)
	}
	st := store.Lookup("c", got.ReceiptID)
	if st.State != extmsg.InboundReceiptConcluded || st.Delivery == nil || st.Delivery.Status != extmsg.InboundDeliveryNoRoute {
		t.Fatalf("store = %+v, want the same no_route conclusion the response carried", st)
	}
}

// TestHumaHandleExtMsgInboundReceiptReportsEveryState pins the wire contract
// the adapter's follow-up poll keys on: one 200 per state, "unknown" included.
// Unknown is deliberately NOT a 404, because a 404 is also what a gc without
// this endpoint answers, and the adapter must fail open on THAT while treating
// a genuine unknown as "nobody is still trying" — two opposite actions that a
// shared status code could not distinguish.
func TestHumaHandleExtMsgInboundReceiptReportsEveryState(t *testing.T) {
	sm := &SupervisorMux{inboundReceipts: extmsg.NewInboundReceiptStore()}
	store := sm.inboundReceiptStore()

	out, err := sm.humaHandleExtMsgInboundReceipt(context.Background(), &ExtMsgInboundReceiptInput{CityScope: CityScope{CityName: "c"}, ReceiptID: "ir-1-404"})
	if err != nil {
		t.Fatalf("unknown receipt returned an error: %v", err)
	}
	if out.Body.State != extmsg.InboundReceiptUnknown || out.Body.ReceiptID != "ir-1-404" {
		t.Fatalf("unknown receipt body = %+v", out.Body)
	}

	store.Begin("c", "ir-1-1")
	out, err = sm.humaHandleExtMsgInboundReceipt(context.Background(), &ExtMsgInboundReceiptInput{CityScope: CityScope{CityName: "c"}, ReceiptID: " ir-1-1 "})
	if err != nil || out.Body.State != extmsg.InboundReceiptPending {
		t.Fatalf("pending receipt: err=%v body=%+v", err, out.Body)
	}

	store.Conclude("c", "ir-1-1", extmsg.FailedInboundDelivery("ir-1-1", "nudge lock timeout"))
	out, err = sm.humaHandleExtMsgInboundReceipt(context.Background(), &ExtMsgInboundReceiptInput{CityScope: CityScope{CityName: "c"}, ReceiptID: "ir-1-1"})
	if err != nil || out.Body.State != extmsg.InboundReceiptConcluded || out.Body.Delivery == nil || out.Body.Delivery.Status != extmsg.InboundDeliveryFailed {
		t.Fatalf("concluded receipt: err=%v body=%+v", err, out.Body)
	}
}

// The caller hanging up (ctx done) before the fan-out concludes is the
// other pending arm; its late conclusion must be recorded exactly like
// the timer arm's.
func TestAwaitInboundFanoutRecordsLateConclusionAfterCallerHangsUp(t *testing.T) {
	store := extmsg.NewInboundReceiptStore()
	release := make(chan struct{})
	fanout := func(context.Context) ([]extmsg.InboundDeliveryMember, error) {
		<-release
		return []extmsg.InboundDeliveryMember{{SessionID: "s1", Status: extmsg.InboundDeliveryDelivered, DeliveredBytes: 7, ExpectedBytes: 7}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := awaitInboundFanout(ctx, store, "c", time.Minute, runInline, fanout, "slack/C1")
	if got.Status != extmsg.InboundDeliveryPending {
		t.Fatalf("response status = %q, want pending when the caller is gone", got.Status)
	}
	close(release)
	st := waitForReceiptState(t, store, got.ReceiptID, extmsg.InboundReceiptConcluded)
	if st.Delivery == nil || st.Delivery.Status != extmsg.InboundDeliveryDelivered {
		t.Fatalf("late conclusion after hang-up = %+v, want delivered recorded", st.Delivery)
	}
}

// TestInboundReceiptRouteAnswersUnknownWith200 drives the registered route
// through the real supervisor mux (path param, city scope, middleware): an
// id gc does not know is a 200 with state "unknown" — the contract the
// adapter's fail-open logic depends on — including an id that decodes to
// whitespace. Only a request that names no id at all is a 404, which is the
// same answer a gc without this endpoint gives and is what fails open.
func TestInboundReceiptRouteAnswersUnknownWith200(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	for _, tc := range []struct {
		name string
		path string
		want int
	}{
		{"unknown id", "/extmsg/inbound/receipts/ir-1-404", http.StatusOK},
		{"encoded whitespace id", "/extmsg/inbound/receipts/%20", http.StatusOK},
		{"no id segment", "/extmsg/inbound/receipts/", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, cityURL(fs, tc.path), nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("GET %s: status = %d, want %d; body = %s", tc.path, rec.Code, tc.want, rec.Body.String())
			}
			if tc.want != http.StatusOK {
				return
			}
			var body extmsg.InboundReceiptStatus
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v; body = %s", err, rec.Body.String())
			}
			if body.State != extmsg.InboundReceiptUnknown {
				t.Fatalf("state = %q, want unknown; body = %s", body.State, rec.Body.String())
			}
		})
	}
}

// TestInboundReceiptStoreSurvivesServerReplacement pins the scope of the
// store: the supervisor swaps a city's Server when its State changes, and
// a fan-out begun under the old Server must still be visible — pending,
// then concluded — through the replacement, or the replacement would
// answer "unknown" for a send that is still running (codex r2 P2 #2).
func TestInboundReceiptStoreSurvivesServerReplacement(t *testing.T) {
	old := &Server{}
	replacement := &Server{}
	id := extmsg.NextInboundReceiptID()
	old.inboundReceiptStore().Begin("c", id)

	if got := replacement.inboundReceiptStore().Lookup("c", id); got.State != extmsg.InboundReceiptPending {
		t.Fatalf("replacement server: state=%q, want pending for a fan-out begun under the old server", got.State)
	}
	old.inboundReceiptStore().Conclude("c", id, extmsg.FailedInboundDelivery(id, "nudge lock timeout"))
	if got := replacement.inboundReceiptStore().Lookup("c", id); got.State != extmsg.InboundReceiptConcluded {
		t.Fatalf("replacement server after conclusion: state=%q, want concluded", got.State)
	}
}

// notRunningCityResolver answers "not running" for every city — the
// window the supervisor passes through while a city's State is being
// replaced.
type notRunningCityResolver struct{}

func (notRunningCityResolver) ListCities() []CityInfo { return nil }
func (notRunningCityResolver) CityState(string) State { return nil }

// TestInboundReceiptRouteAnswersWhileCityNotRunning pins the gap codex r3
// named: the store is process-wide, but a route bound through the city
// resolver 404s while the city is between States — and a 404 is exactly
// what the adapter must fail OPEN on (a gc without the endpoint), so a
// fan-out still running under the old Server would go unverified. The
// receipt route therefore answers from the process-wide store without
// requiring a running city Server.
func TestInboundReceiptRouteAnswersWhileCityNotRunning(t *testing.T) {
	sm := NewSupervisorMux(notRunningCityResolver{}, nil, false, "test", "", time.Now())
	h := wrapTestSupervisorMiddleware(sm)
	id := extmsg.NextInboundReceiptID()
	extmsg.DefaultInboundReceipts().Begin("ghost", id)

	// Proof the city really is "not running" to the resolver.
	req := httptest.NewRequest(http.MethodGet, "/v0/city/ghost/extmsg/bindings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("sibling city route while not running: status = %d, want 404", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v0/city/ghost/extmsg/inbound/receipts/"+id, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("receipt route while the city is not running: status = %d, want 200 (the fan-out is still running in this process); body = %s", rec.Code, rec.Body.String())
	}
	var body extmsg.InboundReceiptStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.State != extmsg.InboundReceiptPending {
		t.Fatalf("state = %q, want pending", body.State)
	}

	// The same id through ANOTHER city's path is unknown: the store is
	// process-wide, the answer is not (codex r4 P2).
	req = httptest.NewRequest(http.MethodGet, "/v0/city/other/extmsg/inbound/receipts/"+id, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("other city's path: status = %d, want 200", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.State != extmsg.InboundReceiptUnknown || body.Delivery != nil {
		t.Fatalf("other city's path: %+v, want unknown with no delivery", body)
	}
}
