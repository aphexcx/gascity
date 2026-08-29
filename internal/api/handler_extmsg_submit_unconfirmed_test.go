package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
)

// submitUnconfirmedFakeProvider is a runtime whose send path frames and pastes
// the complete payload, then cannot observe the agent go busy — exactly what
// the tmux provider does on ErrNudgeSubmitUnconfirmed: the paste succeeded,
// delivered=true was recorded on the nudge receipt, and the error names only
// the unconfirmed submit. It returns (false, err) because that is the tuple
// [tmux.Provider.nudgeNowDelivered] returns for this case.
type submitUnconfirmedFakeProvider struct {
	*runtime.Fake
}

func (p *submitUnconfirmedFakeProvider) NudgeDelivered(name string, content []runtime.ContentBlock) (bool, error) {
	if err := p.Fake.Nudge(name, content); err != nil {
		return false, err
	}
	return false, fmt.Errorf("%w: session %q", tmux.ErrNudgeSubmitUnconfirmed, name)
}

// TestExtmsgNotifyMembersSubmitUnconfirmedIsDeliveredWithContext pins the
// receipt's answer to the 2026-08-28 08:24Z incident (gp-3yg): gc pasted a
// 1215-byte reminder whole, the session answered it two seconds later, and
// the receipt still said failed 0/1215 because the busy probe never caught the
// (fast) turn. The Slack adapter read that as "a retry is clean", re-posted
// the message six times, and then dead-lettered a message the agent had
// already acted on.
//
// A complete bracketed paste whose submit merely went unobserved is a
// delivery with a caveat, not a failure: status delivered, the full byte
// count, and the submit caveat carried in the member error — the gp-2rq
// contract the adapter already implements for transports without a busy
// probe ("read for the log line only, never for the vouch").
func TestExtmsgNotifyMembersSubmitUnconfirmedIsDeliveredWithContext(t *testing.T) {
	fs, srv, ref := notifyDeliveryFixture(t)
	fs.sessionProvider = &submitUnconfirmedFakeProvider{Fake: fs.sp}

	outcomes, notifyErr := srv.extmsgNotifyMembers(context.Background(), extmsgNotifyBroadcast{
		Conversation: ref,
		ActorDisplay: "Taylor",
		ActorKind:    "human",
		Text:         "LightIC / NDAA — can you take a look?",
	})
	if notifyErr != nil {
		t.Fatalf("extmsgNotifyMembers: %v", notifyErr)
	}
	if len(outcomes) != 2 {
		t.Fatalf("got %d outcomes, want 2: %+v", len(outcomes), outcomes)
	}
	for _, m := range outcomes {
		if m.Status != extmsg.InboundDeliveryDelivered {
			t.Fatalf("member %s status = %q, want delivered: a complete paste with an unobserved submit "+
				"is not a clean-retry failure — reporting it as one is what produced six duplicate posts "+
				"and a false dead letter on 2026-08-28 (%+v)", m.SessionID, m.Status, m)
		}
		if m.DeliveredBytes != m.ExpectedBytes || m.DeliveredBytes == 0 {
			t.Fatalf("member %s delivered_bytes=%d expected_bytes=%d, want equal and non-zero: the whole payload "+
				"was pasted", m.SessionID, m.DeliveredBytes, m.ExpectedBytes)
		}
		if !strings.Contains(m.Error, "not confirmed") {
			t.Fatalf("member %s error = %q, want the submit caveat preserved as context", m.SessionID, m.Error)
		}
	}
	if got := extmsg.SummarizeInboundDelivery("ir-test", outcomes); got.Status != extmsg.InboundDeliveryDelivered {
		t.Fatalf("aggregate status = %q, want delivered", got.Status)
	}
}

// TestTmuxSubmitUnconfirmedIsTheRuntimeSentinel guards the identity the
// classification above depends on: the api layer matches the transport-neutral
// [runtime.ErrNudgeSubmitUnconfirmed], so the tmux error must BE that sentinel
// (through however many %w wraps the send path adds), or the false-negative
// receipt silently comes back.
func TestTmuxSubmitUnconfirmedIsTheRuntimeSentinel(t *testing.T) {
	wrapped := fmt.Errorf("sending message to session: %w", fmt.Errorf("%w: session %q", tmux.ErrNudgeSubmitUnconfirmed, "s"))
	if !errors.Is(wrapped, runtime.ErrNudgeSubmitUnconfirmed) {
		t.Fatalf("tmux.ErrNudgeSubmitUnconfirmed does not match runtime.ErrNudgeSubmitUnconfirmed: %v", wrapped)
	}
	// Other failures must keep reading as failures — the sentinel is narrow.
	if errors.Is(fmt.Errorf("nudge lock timeout for session %q", "s"), runtime.ErrNudgeSubmitUnconfirmed) {
		t.Fatal("an unrelated nudge error matched the submit-unconfirmed sentinel")
	}
}
