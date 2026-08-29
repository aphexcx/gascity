package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// extmsgEmitEvent builds an event emitter closure for extmsg handlers.
// The payload parameter is the events.Payload sealed interface so only
// types registered in the central event-payload registry are accepted
// — ad-hoc map[string]any emissions are a compile-time error
// (Principle 7). The json.Marshal below is the internal bus
// serialization permitted by the Principle 4 edge case; the SSE
// projection decodes these bytes back into the typed Go variant via
// events.DecodePayload before emitting on the wire.
func (s *Server) extmsgEmitEvent() func(string, string, events.Payload) {
	ep := s.state.EventProvider()
	if ep == nil {
		return func(string, string, events.Payload) {}
	}
	return func(eventType, subject string, payload events.Payload) {
		b, err := json.Marshal(payload)
		if err != nil {
			fmt.Fprintf(os.Stderr, "extmsg: marshal event payload: %v\n", err)
			return
		}
		ep.Record(events.Event{
			Type:    eventType,
			Subject: subject,
			Payload: b,
		})
	}
}

// extmsgDefaultAgentForConversation builds the InboundDeps default-route
// resolver from [[extmsg.default_route]] config: it maps an unrouted
// inbound conversation to the qualified identity of the configured agent.
// A route naming an agent that does not resolve to a configured named
// session is logged and skipped — the message stays unrouted rather than
// failing the inbound on a config error.
func (s *Server) extmsgDefaultAgentForConversation() func(extmsg.ConversationRef) string {
	cfg := s.state.Config()
	if cfg == nil || len(cfg.ExtMsg.DefaultRoutes) == 0 {
		return nil
	}
	store := s.state.SessionsBeadStore().Store
	return func(ref extmsg.ConversationRef) string {
		agent := cfg.ExtMsgDefaultRouteAgent(ref.Provider, ref.AccountID)
		if agent == "" {
			return ""
		}
		spec, ok, err := s.findNamedSessionSpecForTarget(store, agent)
		if err != nil || !ok {
			log.Printf("extmsg: default-route agent %q for %s/%s does not resolve to a configured named session (err=%v)", agent, ref.Provider, ref.AccountID, err)
			return ""
		}
		return spec.Identity
	}
}

// extmsgResolveSessionSelector builds the OutboundDeps session-selector
// resolver: it maps a selector — a configured agent identity, session name,
// alias, or concrete session bead ID — to the concrete ID of a live session,
// without materializing one. HandleOutbound uses it to authorize publishes on
// agent-bound conversations.
func (s *Server) extmsgResolveSessionSelector() func(ctx context.Context, selector string) (string, error) {
	store := s.state.SessionsBeadStore().Store
	if store == nil {
		return nil
	}
	return func(ctx context.Context, selector string) (string, error) {
		return s.resolveSessionTargetIDWithContext(ctx, store, selector, apiSessionResolveOptions{})
	}
}

func extmsgHandleLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return value
	}
	if idx := strings.LastIndex(value, "/"); idx >= 0 && idx+1 < len(value) {
		return value[idx+1:]
	}
	return value
}

func (s *Server) extmsgSessionHandleForSelector(selector string) string {
	store := s.state.SessionsBeadStore().Store
	if store == nil {
		return extmsgHandleLabel(selector)
	}
	resolvedID, err := session.ResolveSessionIDAllowClosed(store, selector)
	if err != nil {
		return extmsgHandleLabel(selector)
	}
	return s.extmsgSessionHandleForResolvedID(resolvedID, selector)
}

func (s *Server) extmsgSessionHandleForResolvedID(resolvedID, fallback string) string {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return extmsgHandleLabel(fallback)
	}
	source, ok := session.NewStore(store).ExtmsgHandleSource(resolvedID)
	if !ok || source == "" {
		return extmsgHandleLabel(fallback)
	}
	return extmsgHandleLabel(source)
}

// extmsgNotifyBroadcast carries one conversation event through the member
// fan-out in extmsgNotifyMembers.
//
// ExplicitTarget, when non-empty, carries the address-by-handle target so
// peer members can self-silence on off-target messages (see #2484). Outbound
// reply broadcasts and self-update notifications pass "" because they are
// not addressed to a specific agent.
//
// ProviderMessageID and ReplyToMessageID surface the provider-side message
// id and its thread id so the injected reminder can tell agents where a
// threaded reply must go. On the inbound path they come from the inbound
// message; on the outbound broadcast path from the publish receipt and the
// publish request's reply target.
type extmsgNotifyBroadcast struct {
	Conversation      extmsg.ConversationRef
	ActorDisplay      string
	ActorKind         string
	Text              string
	ExcludeSelector   string
	ExplicitTarget    string
	ProviderMessageID string
	ReplyToMessageID  string
}

// extmsgNotifyMembers sends a peer-publication reminder to transcript members
// via the session message API. This treats membership as the routing truth and
// lets session resolution materialize or wake named sessions on first receive.
//
// It returns one [extmsg.InboundDeliveryMember] per member it attempted, which
// the inbound path folds into the caller-visible delivery receipt. Callers
// that only fan out and do not report (the outbound broadcast) discard it.
//
// A non-nil error means the fan-out could not be DETERMINED — the membership
// lookup failed, or the services backing it are unavailable — which is not the
// same as a conversation with no members. Both produce zero outcomes, but zero
// outcomes summarize as no_route, and no_route tells a consumer to commit its
// dedup claim because a retry could not reach anyone either. Collapsing a
// transient store fault into that verdict would silently discard the message.
// Callers must report an error as a delivery failure, never as no_route.
//
// A member that is skipped as the excluded sender contributes no entry: it was
// never a delivery target, so counting it would understate the success rate
// for no reason. A member that could not even be resolved DOES contribute a
// failed entry — from the sender's point of view that is an undelivered
// recipient, and silently dropping it is how a fan-out reports success while
// reaching nobody.
//
// The fan-out is synchronous: it returns only once every member goroutine has
// finished or ctx is done. Bounding the wall-clock cost is the caller's job
// via ctx, because only the caller knows its own response budget.
func (s *Server) extmsgNotifyMembers(ctx context.Context, b extmsgNotifyBroadcast) ([]extmsg.InboundDeliveryMember, error) {
	svc := s.state.ExtMsgServices()
	// Sessions class, and a WRITE path: the member fan-out below materializes a
	// configured named session that has no live bead yet, so through the work
	// store on a relocated city every cold-wake mints a stranded session bead.
	store := s.state.SessionsBeadStore().Store
	if svc == nil || store == nil {
		// Cannot determine membership, so cannot claim there was nobody.
		return nil, fmt.Errorf("extmsg services unavailable")
	}
	conv := b.Conversation
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "extmsg-notify"}
	explicitTargetSessionID := extmsgNotifyExplicitTargetSessionID(ctx, svc, conv, b.ExplicitTarget)
	replyInstructions := s.extmsgReplyInstructionsForConversation(conv)
	members, err := svc.Transcript.ListMemberships(ctx, caller, conv)
	if err != nil {
		log.Printf("extmsg: ListMemberships failed for %s/%s: %v", conv.Provider, conv.ConversationID, err)
		return nil, fmt.Errorf("list memberships for %s/%s: %w", conv.Provider, conv.ConversationID, err)
	}
	if len(members) == 0 {
		// Membership is the routing truth for this fan-out: zero members means
		// the message notifies nobody. That is legitimate for a conversation
		// with no bound/participating sessions, but when it happens on a bound
		// conversation it is the hq-ar4 black hole — make it observable either
		// way instead of returning in silence.
		log.Printf("extmsg: no transcript members for %s/%s — nobody notified (actor=%q)", conv.Provider, conv.ConversationID, b.ActorDisplay)
		// A SUCCESSFUL lookup that found nobody. This is the only path allowed
		// to produce no_route.
		return nil, nil
	}

	excludedResolvedID := ""
	excludedSelector := apiNormalizeSessionTarget(b.ExcludeSelector)
	if selector := strings.TrimSpace(b.ExcludeSelector); selector != "" {
		resolvedID, err := s.resolveSessionTargetIDWithContext(ctx, store, selector, apiSessionResolveOptions{})
		if err != nil {
			log.Printf("extmsg: resolve sender %s failed: %v", selector, err)
		} else {
			excludedResolvedID = resolvedID
		}
	}

	// outcomes is appended to from every member goroutine, so it is mutex-held
	// rather than pre-sized by index: members can drop out (excluded sender)
	// without leaving a zero-valued hole that would read as a failed delivery.
	var outcomesMu sync.Mutex
	outcomes := make([]extmsg.InboundDeliveryMember, 0, len(members))
	record := func(m extmsg.InboundDeliveryMember) {
		outcomesMu.Lock()
		defer outcomesMu.Unlock()
		outcomes = append(outcomes, m)
	}

	notifyResolved := func(sessionSelector, resolvedID string) {
		handle := s.extmsgSessionHandleForResolvedID(resolvedID, sessionSelector)
		nudge := formatExtmsgNotifyReminder(extmsgNotifyReminder{
			Provider:                conv.Provider,
			ConversationID:          conv.ConversationID,
			ActorDisplay:            b.ActorDisplay,
			ActorKind:               b.ActorKind,
			Text:                    b.Text,
			RecipientSelector:       sessionSelector,
			RecipientSessionID:      resolvedID,
			Handle:                  handle,
			ExplicitTarget:          b.ExplicitTarget,
			ExplicitTargetSessionID: explicitTargetSessionID,
			ProviderMessageID:       b.ProviderMessageID,
			ReplyToMessageID:        b.ReplyToMessageID,
			ReplyInstructions:       replyInstructions,
		})
		// Expected size and digest are properties of the string gc built, so
		// they are known here regardless of how the send goes — which is what
		// lets a failed member still report what it was supposed to receive.
		outcome := extmsg.InboundDeliveryMember{
			SessionID:     resolvedID,
			Selector:      sessionSelector,
			ExpectedBytes: len(nudge),
			Digest:        runtime.NudgePayloadDigest(nudge),
		}
		result, err := s.sendBackgroundMessageToSession(ctx, store, resolvedID, nudge)
		// One decision point. Every fact the send path produced goes in —
		// the error, the runtime's delivered flag, its byte count, its submit
		// verdict, its downgrade reason — and the status comes out of
		// extmsg.ClassifyInboundMember, whose table test is the spec. Nothing
		// here re-reads the error or the flag on its own: on 2026-08-28 the
		// runtime's "submit not confirmed" error was classified as failed with
		// 0 bytes for a payload that had landed whole, and the consumer's
		// clean re-post turned one founder message into six (gp-2io).
		verdict := extmsg.ClassifyInboundMember(extmsg.InboundMemberEvidence{
			NudgeDelivery: runtime.NudgeDelivery{Delivered: result.Delivered, Bytes: result.Bytes, Submit: result.Submit},
			ExpectedBytes: len(nudge),
			Err:           err,
			Undelivered:   string(result.Undelivered),
		})
		outcome.Status = verdict.Status
		outcome.DeliveredBytes = verdict.DeliveredBytes
		outcome.Error = verdict.Reason
		if verdict.Status != extmsg.InboundDeliveryDelivered {
			log.Printf("extmsg: notify %s %s (%d/%d bytes): %s", sessionSelector, verdict.Status, verdict.DeliveredBytes, len(nudge), verdict.Reason)
		}
		record(outcome)
	}

	var wg sync.WaitGroup
	for _, m := range members {
		wg.Add(1)
		go func(sessionSelector string) {
			defer wg.Done()
			if excludedSelector != "" && apiNormalizeSessionTarget(sessionSelector) == excludedSelector {
				return
			}
			preexistingID, preErr := s.resolveSessionTargetIDWithContext(ctx, store, sessionSelector, apiSessionResolveOptions{})
			if preErr == nil && preexistingID != "" {
				if excludedResolvedID != "" && preexistingID == excludedResolvedID {
					return
				}
				notifyResolved(sessionSelector, preexistingID)
				return
			}
			resolvedID, err := s.resolveSessionIDMaterializingNamedWithContext(ctx, store, sessionSelector)
			if err != nil {
				log.Printf("extmsg: resolve session %s failed: %v", sessionSelector, err)
				// An unresolvable member is an undelivered recipient, not a
				// non-member: report it so the aggregate cannot read
				// "delivered" while a transcript member got nothing.
				record(extmsg.InboundDeliveryMember{
					SessionID: sessionSelector,
					Selector:  sessionSelector,
					Status:    extmsg.InboundDeliveryFailed,
					Error:     "resolve session: " + err.Error(),
				})
				return
			}
			if preErr != nil {
				log.Printf("extmsg: materialized session %s as %s for conversation %s/%s", sessionSelector, resolvedID, conv.Provider, conv.ConversationID)
			}
			if excludedResolvedID != "" && resolvedID == excludedResolvedID {
				return
			}
			notifyResolved(sessionSelector, resolvedID)
		}(m.SessionID)
	}
	wg.Wait()

	// Stable order so a response body and a log line are diffable across
	// retries; goroutine completion order is not.
	sort.Slice(outcomes, func(i, j int) bool {
		if outcomes[i].Selector != outcomes[j].Selector {
			return outcomes[i].Selector < outcomes[j].Selector
		}
		return outcomes[i].SessionID < outcomes[j].SessionID
	})
	return outcomes, nil
}

// extmsgReplyInstructionsForConversation returns the reply-instruction
// template the conversation's adapter registered, or "" when the registry
// or adapter is missing or the adapter does not provide one — the reminder
// then falls back to the generic reply-current text.
func (s *Server) extmsgReplyInstructionsForConversation(conv extmsg.ConversationRef) string {
	reg := s.state.AdapterRegistry()
	if reg == nil {
		return ""
	}
	adapter := reg.LookupByConversation(conv)
	if adapter == nil {
		return ""
	}
	provider, ok := adapter.(extmsg.ReplyInstructionsProvider)
	if !ok {
		return ""
	}
	return provider.ReplyInstructions()
}

func extmsgNotifyExplicitTargetSessionID(ctx context.Context, svc *extmsg.Services, conv extmsg.ConversationRef, explicitTarget string) string {
	if strings.TrimSpace(explicitTarget) == "" || svc == nil || svc.Groups == nil {
		return ""
	}
	route, err := svc.Groups.ResolveInbound(ctx, extmsg.ExternalInboundMessage{
		Conversation:   conv,
		ExplicitTarget: explicitTarget,
	})
	if err != nil {
		log.Printf("extmsg: resolve explicit target %q for %s/%s failed: %v", explicitTarget, conv.Provider, conv.ConversationID, err)
		return ""
	}
	if route == nil || route.Match != extmsg.GroupRouteExplicitTarget {
		return ""
	}
	return strings.TrimSpace(route.TargetSessionID)
}

// extmsgInboundReceiptBudget bounds how long the inbound response waits for
// the terminal fan-out before answering "pending".
//
// It is set under the 20s HTTP client deadline the Slack adapter uses
// (gcForwardClient), so a slow session degrades to an honest pending receipt
// that the adapter can act on, rather than to a client-side timeout that tells
// it nothing. It is NOT a cancellation: the fan-out keeps running past this
// point (see extmsgNotifyInboundWithReceipt).
const extmsgInboundReceiptBudget = 15 * time.Second

// extmsgNotifyInboundWithReceipt runs the inbound terminal fan-out and returns
// a delivery receipt describing what actually reached agent terminals.
//
// The fan-out runs on the background task group — as it always has, so server
// shutdown still waits for it — but the response now WAITS for it, up to
// extmsgInboundReceiptBudget. That wait is the entire point: the previous
// fire-and-forget shape meant the 200 was written before a single keystroke
// reached a terminal, so it could not report delivery even in principle, and a
// caller gating on it was gating on nothing (pc_2e2378b9918e).
//
// On budget expiry the fan-out is deliberately NOT cancelled and the receipt
// says "pending". A paste already handed to a terminal cannot be un-sent, so
// cancelling would not undo a partial delivery — it would only destroy the
// remaining ones. The honest report is that gc does not yet know, which leaves
// the retry decision with the caller that owns the dedup claim.
//
// Note that the fan-out's own lifetime is bounded only in principle: the
// background context caps it at extmsgNotifyTimeout, but the runtime nudge
// below the session manager does not take a context and will not observe that
// cancellation. A wedged provider therefore holds its fan-out goroutine (and
// the shutdown WaitGroup) until it returns on its own. That predates this
// receipt and is not made worse by it — the wait here is bounded regardless —
// but do not read the background context as a hard kill.
func (s *Server) extmsgNotifyInboundWithReceipt(ctx context.Context, msg extmsg.ExternalInboundMessage) extmsg.InboundDelivery {
	conversation := msg.Conversation.Provider + "/" + msg.Conversation.ConversationID
	return awaitInboundFanout(ctx, s.inboundReceiptStore(), s.state.CityName(), extmsgInboundReceiptBudget, s.runBackground,
		func(bgCtx context.Context) ([]extmsg.InboundDeliveryMember, error) {
			return s.extmsgNotifyInboundMembers(bgCtx, msg)
		}, conversation)
}

// awaitInboundFanout runs one inbound fan-out under budget and returns the
// receipt to answer with. It is the mechanism behind gp-3yg: whether or not
// the response waited long enough to see it, the fan-out's conclusion is
// RECORDED in store against the receipt id, so a caller that was answered
// "pending" can poll GET /extmsg/inbound/receipts/{id} afterwards and learn
// definitively whether the send landed. Before this the late result was
// published into a channel nobody read any more, and a message lost after a
// pending receipt left no trace beyond the caller's own "held" log line.
//
// Split from the Server method so the budget race is testable with a fan-out
// the test controls; city scopes the record (a lookup through another
// city's path answers unknown); conversation is log context only.
func awaitInboundFanout(
	ctx context.Context,
	store *extmsg.InboundReceiptStore,
	city string,
	budget time.Duration,
	runBackground func(func(context.Context)),
	fanout func(context.Context) ([]extmsg.InboundDeliveryMember, error),
	conversation string,
) extmsg.InboundDelivery {
	receiptID := extmsg.NextInboundReceiptID()
	store.Begin(city, receiptID)
	// Buffered so the fan-out goroutine never blocks publishing its result
	// after the budget has expired and nobody is receiving any more.
	done := make(chan extmsg.InboundDelivery, 1)
	runBackground(func(bgCtx context.Context) {
		members, err := fanout(bgCtx)
		var delivery extmsg.InboundDelivery
		if err != nil {
			// The fan-out could not be determined. Reporting this as no_route
			// (which zero members would summarize to) would tell the caller to
			// commit its dedup claim and drop the message for good.
			log.Printf("extmsg: inbound fan-out failed (receipt=%s conversation=%s): %v", receiptID, conversation, err)
			delivery = extmsg.FailedInboundDelivery(receiptID, err.Error())
		} else {
			delivery = extmsg.SummarizeInboundDelivery(receiptID, members)
			if delivery.Status != extmsg.InboundDeliveryDelivered {
				// The case gp-32q exists for. Logged at the seam so it is
				// visible in gc's own logs even when the caller drops the
				// receipt — and logged HERE, on the fan-out side, so a late
				// conclusion the response never saw is still on record.
				log.Printf("extmsg: %s conversation=%s", delivery, conversation)
			}
		}
		// Record before publishing: a caller that reads the channel and
		// immediately polls must not see "pending" for a fan-out that has
		// concluded.
		store.Conclude(city, receiptID, delivery)
		done <- delivery
	})

	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case delivery := <-done:
		return delivery
	case <-ctx.Done():
		// The caller hung up or the server is shutting down. Waiting out the
		// rest of the budget would pin a handler goroutine per inbound request
		// for a response nobody will read. The sends continue regardless.
		log.Printf("extmsg: inbound request ended before the fan-out concluded, reporting pending (receipt=%s conversation=%s): %v",
			receiptID, conversation, ctx.Err())
		return extmsg.PendingInboundDelivery(receiptID)
	case <-timer.C:
		log.Printf("extmsg: inbound fan-out exceeded %s receipt budget, reporting pending (receipt=%s conversation=%s) — sends continue in background; poll GET /extmsg/inbound/receipts/%s for the outcome",
			budget, receiptID, conversation, receiptID)
		return extmsg.PendingInboundDelivery(receiptID)
	}
}

func (s *Server) extmsgNotifyInboundMembers(ctx context.Context, msg extmsg.ExternalInboundMessage) ([]extmsg.InboundDeliveryMember, error) {
	actorKind := "agent"
	if !msg.Actor.IsBot {
		actorKind = "human"
	}
	return s.extmsgNotifyMembers(ctx, extmsgNotifyBroadcast{
		Conversation:      msg.Conversation,
		ActorDisplay:      msg.Actor.DisplayName,
		ActorKind:         actorKind,
		Text:              msg.Text,
		ExplicitTarget:    msg.ExplicitTarget,
		ProviderMessageID: msg.ProviderMessageID,
		ReplyToMessageID:  msg.ReplyToMessageID,
	})
}

// titleCaseProvider uppercases the first ASCII byte of a provider name.
// Used to avoid a golang.org/x/text/cases dependency just for one
// capitalization in the inbound nudge — provider names are always
// short lowercase ASCII identifiers (slack, discord, ...).
func titleCaseProvider(name string) string {
	if name == "" {
		return ""
	}
	first := name[0]
	if first >= 'a' && first <= 'z' {
		return string(first-'a'+'A') + name[1:]
	}
	return name
}

// extmsgNotifyReminder collects the inputs the inbound-message
// <system-reminder> block is constructed from. Externally-supplied fields
// (ActorDisplay, Text, ExplicitTarget) are sanitized via
// extmsg.SanitizeForSystemReminder inside formatExtmsgNotifyReminder before
// interpolation; callers should not pre-sanitize.
//
// ExplicitTarget carries the provider-resolved address-by-handle target (set
// when an inbound was addressed to a specific agent via @handle: prefix or a
// subteam mention). When non-empty and not routed to the receiving session,
// formatExtmsgNotifyReminder emits a "do not reply" discriminator line so
// peer sessions can self-silence on off-target messages. See
// gastownhall/gascity#2484.
type extmsgNotifyReminder struct {
	Provider                string
	ConversationID          string
	ActorDisplay            string
	ActorKind               string
	Text                    string
	RecipientSelector       string
	RecipientSessionID      string
	Handle                  string
	ExplicitTarget          string
	ExplicitTargetSessionID string
	// ProviderMessageID/ReplyToMessageID carry the provider-side message
	// id and thread id (empty when the notifying path has none, e.g. an
	// outbound broadcast with no receipt). They surface threading context
	// in the reminder and feed the {message_ts}/{thread_ts} placeholders.
	ProviderMessageID string
	ReplyToMessageID  string
	// ReplyInstructions is the adapter-registered reply-instruction
	// template (see extmsg.ReplyInstructionsProvider); empty selects the
	// generic reply-current fallback text.
	ReplyInstructions string
}

// formatExtmsgNotifyReminder builds the inbound-message reminder body.
// Attacker-controllable fields (ActorDisplay, Text, ExplicitTarget) are
// stripped of literal <system-reminder> open/close sequences before being
// interpolated into the reminder block. Without this guard, an external
// sender can inject the sequence and break out of the legitimate reminder,
// injecting attacker-controlled instructions into the receiving agent's
// prompt. See gastownhall/gascity#2195.
//
// When ExplicitTarget is non-empty and does not target the receiving session,
// a discriminator line is appended so peer sessions can self-silence on
// messages addressed to a different agent. See gastownhall/gascity#2484.
func formatExtmsgNotifyReminder(r extmsgNotifyReminder) string {
	providerCLI := strings.ToLower(r.Provider)
	providerDisplay := titleCaseProvider(providerCLI)
	safeActor := extmsg.SanitizeForSystemReminder(r.ActorDisplay)
	safeText := extmsg.SanitizeForSystemReminder(r.Text)

	var b strings.Builder
	fmt.Fprintf(&b,
		"<system-reminder>\nNew message in shared conversation %s/%s:\n\n"+
			"- %s (%s): %s\n\n",
		r.Provider, r.ConversationID,
		safeActor, r.ActorKind, safeText,
	)
	if messageID := strings.TrimSpace(r.ProviderMessageID); messageID != "" {
		fmt.Fprintf(&b, "Message id: %s", extmsg.SanitizeForSystemReminder(messageID))
		if threadID := strings.TrimSpace(r.ReplyToMessageID); threadID != "" {
			fmt.Fprintf(&b, " (in thread %s)", extmsg.SanitizeForSystemReminder(threadID))
		}
		b.WriteString("\n\n")
	}
	if target := strings.TrimSpace(r.ExplicitTarget); target != "" && !extmsgNotifyReminderTargetsRecipient(r, target) {
		safeTarget := extmsg.SanitizeForSystemReminder(target)
		fmt.Fprintf(&b,
			"Addressed to: @%s — if that is not you, do not reply.\n\n",
			safeTarget,
		)
	}
	if instructions := renderExtmsgReplyInstructions(r); instructions != "" {
		b.WriteString(instructions)
		if !strings.HasSuffix(instructions, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("</system-reminder>")
		return b.String()
	}
	fmt.Fprintf(&b,
		"To reply in %s, write your response to a file and run:\n"+
			"  gc %s reply-current --conversation-id %s --body-file <path>\n"+
			"</system-reminder>",
		providerDisplay,
		providerCLI, r.ConversationID,
	)
	return b.String()
}

var (
	// extmsgOptionalSegmentRE matches [bracketed segments] in an
	// adapter-registered reply-instruction template; a segment is dropped
	// when any placeholder inside it resolves empty.
	extmsgOptionalSegmentRE = regexp.MustCompile(`\[[^\[\]]*\]`)
	// extmsgPlaceholderRE matches {placeholder} tokens.
	extmsgPlaceholderRE = regexp.MustCompile(`\{[a-z_]+\}`)
)

// renderExtmsgReplyInstructions renders the adapter-registered
// reply-instruction template with the reminder's values. The effective
// {thread_ts} is the inbound message's thread id, falling back to the
// message's own id — replying to a top-level message starts its thread.
// Substituted values are attacker-influenced (provider-supplied ids), so
// they are sanitized like every other interpolated reminder field; the
// template itself comes from the local adapter registration.
func renderExtmsgReplyInstructions(r extmsgNotifyReminder) string {
	template := strings.TrimSpace(r.ReplyInstructions)
	if template == "" {
		return ""
	}
	threadTS := strings.TrimSpace(r.ReplyToMessageID)
	if threadTS == "" {
		threadTS = strings.TrimSpace(r.ProviderMessageID)
	}
	values := map[string]string{
		"conversation_id": strings.TrimSpace(r.ConversationID),
		"message_ts":      strings.TrimSpace(r.ProviderMessageID),
		"thread_ts":       threadTS,
		"handle":          strings.TrimSpace(r.Handle),
	}
	rendered := extmsgOptionalSegmentRE.ReplaceAllStringFunc(template, func(segment string) string {
		expanded, allSet := substituteExtmsgPlaceholders(segment[1:len(segment)-1], values)
		if !allSet {
			return ""
		}
		return expanded
	})
	rendered, _ = substituteExtmsgPlaceholders(rendered, values)
	return rendered
}

// substituteExtmsgPlaceholders replaces known {placeholder} tokens with
// their sanitized values, leaving unknown tokens literal. The bool reports
// whether every known placeholder in s resolved non-empty.
func substituteExtmsgPlaceholders(s string, values map[string]string) (string, bool) {
	allSet := true
	out := extmsgPlaceholderRE.ReplaceAllStringFunc(s, func(token string) string {
		value, known := values[token[1:len(token)-1]]
		if !known {
			return token
		}
		if value == "" {
			allSet = false
			return ""
		}
		return extmsg.SanitizeForSystemReminder(value)
	})
	return out, allSet
}

func extmsgNotifyReminderTargetsRecipient(r extmsgNotifyReminder, target string) bool {
	if targetSessionID := strings.TrimSpace(r.ExplicitTargetSessionID); targetSessionID != "" {
		return strings.TrimSpace(r.RecipientSessionID) == targetSessionID ||
			apiNormalizeSessionTarget(r.RecipientSelector) == apiNormalizeSessionTarget(targetSessionID)
	}
	return strings.EqualFold(target, strings.TrimSpace(r.Handle))
}
