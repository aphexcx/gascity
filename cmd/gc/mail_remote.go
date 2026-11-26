package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clientcontext"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// Remote mail uses a city-scoped client. The server resolves the recipient
// and stores the sender address asserted by the client. A city-write grant
// authorizes the request to its destination. The default return address
// is "<local city>/<identity>", and --from overrides it verbatim.

// remoteMailIdentity returns the identity part of the default remote sender:
// the first non-empty of GC_ALIAS, GC_AGENT, GC_SESSION_ID, else "human". The
// alias/agent names lead because they are meaningful to a peer city; a bare
// session id (the local default, see defaultMailIdentityCandidates) is opaque
// outside the city that minted it.
func remoteMailIdentity() string {
	for _, k := range []string{"GC_ALIAS", "GC_AGENT", "GC_SESSION_ID"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return "human"
}

// localCityNameForRemoteMail returns the effective name of the LOCAL city the
// caller is operating from (explicit city env, then cwd discovery), or "" when
// no local city is discoverable. It never consults the remote target.
func localCityNameForRemoteMail() string {
	var cityPath string
	if ctx, handled, err := resolveContextFromCityEnv(contextResolutionMode{}); handled && err == nil {
		cityPath = ctx.CityPath
	} else if ctx, err := resolveContextFromDir(contextResolutionMode{}); err == nil {
		cityPath = ctx.CityPath
	}
	if cityPath == "" {
		return ""
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		cfg = nil
	}
	return strings.TrimSpace(loadedCityName(cfg, cityPath))
}

// remoteMailSender resolves the sender for a remote mail mutation: an explicit
// --from (whitespace-trimmed, otherwise as given), else
// "<local city>/<identity>" (identity alone when no local city is
// discoverable). The peer stores an unknown sender literally; if the peer
// happens to have a session whose alias equals this string (a rig named after
// the sending city), its beadmail binds the message to that session — so a
// peer must not name a rig after a city that mails it (see the runbook).
func remoteMailSender(from string) string {
	if from = strings.TrimSpace(from); from != "" {
		return from
	}
	id := remoteMailIdentity()
	if city := localCityNameForRemoteMail(); city != "" {
		return city + "/" + id
	}
	return id
}

// remoteMailSubject mirrors the beadmail provider's title derivation for a
// send without -s: the API requires a non-empty subject (minLength 1), so
// derive it from the first non-blank body line (truncated). It returns "" only
// when neither the subject nor the body carries any non-blank text — the
// caller refuses that locally instead of round-tripping to a 422.
func remoteMailSubject(subject, body string) string {
	if subject = strings.TrimSpace(subject); subject != "" {
		return subject
	}
	title := strings.TrimSpace(strings.SplitN(strings.TrimLeft(body, " \t\r\n"), "\n", 2)[0])
	if len(title) > 80 {
		title = title[:77] + "..."
	}
	return title
}

// cmdMailSendRemote is the remote arm of `gc mail send`. args are the
// positional [to, body...] (after --to has been folded in by the caller).
func cmdMailSendRemote(c *api.Client, target *remoteTarget, args []string, notify, all bool, from, to, subject, message string, jsonOut bool, stdout, stderr io.Writer) int {
	fail := func(code, msg string) int {
		if jsonOut {
			return writeJSONError(stdout, stderr, code, msg, 1)
		}
		fmt.Fprintln(stderr, msg) //nolint:errcheck // best-effort stderr
		return 1
	}
	if all {
		return fail("unsupported_remote", "gc mail send: --all is not supported for a remote city (live-session enumeration is local); address one recipient")
	}
	if to != "" {
		args = append([]string{to}, args...)
	}
	if len(args) < 1 {
		return fail("invalid_arguments", "gc mail send: missing recipient")
	}
	recipient := args[0]
	body := message
	if body == "" && len(args) > 1 {
		body = strings.Join(args[1:], " ")
	}
	derivedSubject := remoteMailSubject(subject, body)
	if derivedSubject == "" {
		return fail("invalid_arguments", "gc mail send: usage: gc mail send <to> <body>  OR  gc mail send <to> -s <subject> [-m <body>] (a non-blank subject or body is required)")
	}
	sender := remoteMailSender(from)
	if !jsonOut {
		fmt.Fprintln(stderr, formatRemoteTarget(target)) //nolint:errcheck // best-effort stderr
	}
	req := api.MailSendRequest{
		From:    sender,
		To:      recipient,
		Subject: derivedSubject,
		Body:    body,
		Notify:  notify,
	}
	result, err := c.SendMail(req)
	if errors.Is(err, api.ErrMailNotifyUnsupported) {
		fmt.Fprintln(stderr, "gc mail send: peer is too old to request a turn; retrying without --notify") //nolint:errcheck // best-effort stderr
		req.Notify = false
		result, err = c.SendMail(req)
	}
	if err != nil {
		if notify && api.IsConnError(err) {
			return fail("mail_send_failed", "gc mail send: "+err.Error()+". The peer may already have stored the mail.")
		}
		return fail("mail_send_failed", "gc mail send: "+err.Error())
	}
	m := result.Message
	accepted := result.Notified != nil && *result.Notified
	if jsonOut {
		summary := summarizeMailMessage(m)
		action := mailActionResult{SchemaVersion: "1", OK: true, Command: "mail.send", Action: "send", ID: m.ID, Message: &summary, Messages: []mailMessageSummary{summary}, Count: intRef(1)}
		if notify {
			return writeCLIJSONLineOrExit(stdout, stderr, "gc mail send", struct {
				mailActionResult
				Notified bool `json:"notified"`
			}{action, accepted})
		}
		return writeCLIJSONLineOrExit(stdout, stderr, "gc mail send", action)
	}
	status := ""
	if notify {
		status = "; peer did not accept the turn request"
		if accepted {
			status = "; peer accepted the turn request"
		}
	}
	fmt.Fprintf(stdout, "Sent message %s to %s (as %s)%s\n", m.ID, m.To, m.From, status) //nolint:errcheck // best-effort stdout
	return 0
}

type mailReplyResolver struct {
	cfg       *config.City
	configErr error
	sessions  beads.Store
}

// target resolves a city-qualified return address against the local
// registry. A context may have an alias distinct from its destination city.
func (r mailReplyResolver) target(original mail.Message, providerLocal bool, stderr io.Writer) (*remoteTarget, string, error) {
	city, recipient, qualified := strings.Cut(original.From, "/")
	if strings.TrimSpace(original.From) == "" || strings.IndexFunc(original.From, unicode.IsControl) >= 0 ||
		qualified && (city == "" || recipient == "" || strings.Contains(original.From, "//") || strings.HasSuffix(recipient, "/") || strings.HasPrefix(recipient, "-")) {
		return nil, "", fmt.Errorf("cannot resolve sender %q; use gc --context CITY mail send AGENT", original.From)
	}
	if !qualified {
		// Exec providers own their local recipient namespace. Only the provider
		// can resolve or refuse an unqualified return address.
		if providerLocal && r.configErr == nil {
			return nil, "", nil
		}
		recipient = original.From
	}
	hint := func(name string) string {
		return shellquote.Join([]string{"gc", "--context", name, "mail", "send", recipient})
	}
	if original.FromSessionID != "" || (r.configErr == nil && knownLocalMailSender(original, r.cfg)) {
		return nil, "", nil
	}
	var localSender bool
	if r.sessions != nil {
		var err error
		localSender, err = sessionOwnsMailAddress(r.sessions, original.From)
		if err != nil {
			if !providerLocal {
				return nil, "", fmt.Errorf("resolving sender %q: %w; use %s", original.From, err, hint("CITY"))
			}
			// An exec provider owns bare recipients even if the city's
			// optional session lookup cannot confirm the address.
			r.configErr = fmt.Errorf("%w; resolving sender: %w", r.configErr, err)
		}
	}
	if !qualified {
		if localSender {
			return nil, "", nil
		}
		if r.configErr != nil {
			fmt.Fprintf(stderr, "gc mail reply: warning: %v; recipient %q not checked\n", r.configErr, original.From) //nolint:errcheck // best-effort stderr
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("no local recipient %q; to answer another city use %s", original.From, hint("CITY"))
	}
	if r.configErr != nil && !localSender {
		return nil, "", fmt.Errorf("%w; cannot tell whether %q is local; use %s", r.configErr, original.From, hint("CITY"))
	}
	if r.cfg == nil && r.configErr == nil {
		return nil, "", fmt.Errorf("no local city to resolve sender %q; use %s", original.From, hint("CITY"))
	}
	contexts, err := clientcontext.Load(DefaultPath())
	if err != nil {
		if localSender {
			fmt.Fprintf(stderr, "gc mail reply: warning: %v; replied locally without checking contexts\n", err) //nolint:errcheck // best-effort stderr
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("%w; use %s after configuring the origin", err, hint("CITY"))
	}
	var name string
	for _, candidate := range contexts.Contexts {
		if candidate.EffectiveCity() == city {
			if name != "" {
				return nil, "", fmt.Errorf("multiple contexts for city %q; select one with %s", city, hint("CITY"))
			}
			name = candidate.Name
		}
	}
	if name != "" && (localSender || rigConfigHasRigNamed(r.cfg, city)) {
		return nil, "", fmt.Errorf("sender %q matches both a local address namespace and city %q; to reply there use %s", original.From, city, hint(name))
	}
	if localSender {
		return nil, "", nil
	}
	if name == "" {
		contextName := city
		if _, taken := contexts.Lookup(city); taken {
			contextName = "CITY" // The same-named alias targets a different city.
		}
		return nil, "", fmt.Errorf("no context for city %q; configure one and use %s", city, hint(contextName))
	}
	if err := guardNoAPI(readRemoteSelection()); err != nil {
		return nil, "", err
	}
	target, err := targetFromContext(contexts, name, "mail sender")
	return target, recipient, err
}

func knownLocalMailSender(original mail.Message, cfg *config.City) bool {
	return original.From == "human" || original.FromSessionID != "" ||
		config.FindAgent(cfg, original.From) != nil || config.FindNamedSession(cfg, original.From) != nil
}

func sessionOwnsMailAddress(sessions beads.Store, address string) (bool, error) {
	directory := session.NewStore(beads.SessionStore{Store: sessions})
	_, err := directory.ResolveAddress(address, true)
	if !errors.Is(err, session.ErrSessionNotFound) {
		return err == nil, err
	}
	// Older and exec-provider messages may predate a rename and carry only
	// the old display address. Beadmail includes alias history in its inboxes.
	all, err := directory.ListAddresses(true)
	if err != nil {
		return false, err
	}
	for _, info := range all {
		if slices.Contains(info.AliasHistory, address) {
			return true, nil
		}
	}
	return false, nil
}

func mailReplyError(code string, err error, jsonOut bool, stdout, stderr io.Writer) int {
	msg := "gc mail reply: " + err.Error()
	if jsonOut {
		return writeJSONError(stdout, stderr, code, msg, 1)
	}
	fmt.Fprintln(stderr, msg) //nolint:errcheck // best-effort stderr
	return 1
}

// cmdMailReplyRemote replies to an original stored in the explicitly selected city.
func cmdMailReplyRemote(c *api.Client, target *remoteTarget, args []string, subject, message string, notify, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		return mailReplyError("invalid_arguments", fmt.Errorf("missing message ID"), jsonOut, stdout, stderr)
	}
	body := message
	if body == "" && len(args) > 1 {
		body = strings.Join(args[1:], " ")
	}
	return sendRemoteReply(c, target, events.Discard, args[0], "", remoteMailSender(""), subject, body, notify, jsonOut, stdout, stderr)
}

func sendRemoteReply(c *api.Client, target *remoteTarget, rec events.Recorder, id, recipient, sender, subject, body string, notify, jsonOut bool, stdout, stderr io.Writer) int {
	if notify {
		return mailReplyError("unsupported_remote", fmt.Errorf("--notify delivery for a remote city lands separately; reply without --notify"), jsonOut, stdout, stderr)
	}
	if !jsonOut {
		fmt.Fprintln(stderr, formatRemoteTarget(target)) //nolint:errcheck // best-effort stderr
	}
	var reply mail.Message
	var err error
	var destination string
	if recipient == "" {
		reply, err = c.ReplyMail(id, api.MailReplyRequest{From: sender, Subject: subject, Body: body})
	} else {
		// The original ID belongs to the local store, so it cannot identify
		// a parent message in the other city.
		destination = target.CityName
		var sent api.MailSendResult
		sent, err = c.SendMail(api.MailSendRequest{From: sender, To: recipient, Subject: subject, Body: body})
		reply = sent.Message
	}
	if err != nil {
		return mailReplyError("mail_reply_failed", err, jsonOut, stdout, stderr)
	}
	to := reply.To
	if destination != "" {
		to = destination + "/" + to
		rec.Record(events.Event{
			Type: events.MailReplied, Actor: sender, Subject: reply.ID,
			Message: to, Payload: mailEventPayload(&reply),
		})
	}
	if jsonOut {
		summary := summarizeMailMessage(reply)
		return writeCLIJSONLineOrExit(stdout, stderr, "gc mail reply", mailActionResult{SchemaVersion: "1", OK: true, Command: "mail.reply", Action: "reply", ID: reply.ID, City: destination, Message: &summary, Messages: []mailMessageSummary{summary}, Count: intRef(1)})
	}
	fmt.Fprintf(stdout, "Replied to %s — sent message %s to %s (as %s)\n", id, reply.ID, to, reply.From) //nolint:errcheck // best-effort stdout
	return 0
}

// remoteInboxReader adapts the remote client's inbox list to the
// mailInboxReader seam so the remote inbox renders exactly like a local one:
// it pages through the server's keyset pagination until the mailbox is
// exhausted (a local inbox lists every unread message, so a single capped page
// would silently truncate), and a partial aggregate read — one rig's provider
// failed server-side — is an error, not an authoritative short list (remote
// reads are non-fallbackable, gate G1).
type remoteInboxReader struct{ c *api.Client }

// remoteInboxMaxPages bounds the paging loop so a server that keeps returning
// a cursor cannot spin the client forever; at the server's default page size
// this is far beyond any real unread mailbox.
const remoteInboxMaxPages = 200

func (r remoteInboxReader) Inbox(recipient string) ([]mail.Message, error) {
	var out []mail.Message
	cursor := ""
	for page := 0; page < remoteInboxMaxPages; page++ {
		cr, err := r.c.ListMailInboxPage(recipient, "", cursor, 0)
		if err != nil {
			return nil, err
		}
		if cr.Body.Partial {
			return nil, fmt.Errorf("remote inbox read is partial (%s); not rendering an incomplete mailbox", strings.Join(cr.Body.PartialErrors, "; "))
		}
		out = append(out, cr.Body.Items...)
		if cr.Body.NextCursor == "" || len(cr.Body.Items) == 0 {
			return out, nil
		}
		cursor = cr.Body.NextCursor
	}
	return nil, fmt.Errorf("remote inbox read exceeded %d pages; refusing to render a possibly incomplete mailbox", remoteInboxMaxPages)
}

// cmdMailInboxRemote is the remote arm of `gc mail inbox [recipient]`. With no
// recipient it reads the mailbox of this client's own remote sender identity
// ("<local city>/<identity>") stored in the selected city.
func cmdMailInboxRemote(c *api.Client, target *remoteTarget, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	recipient := ""
	if len(args) > 0 {
		recipient = strings.TrimSpace(args[0])
	}
	if recipient == "" {
		recipient = remoteMailSender("")
	}
	if !jsonOut {
		fmt.Fprintln(stderr, formatRemoteTarget(target)) //nolint:errcheck // best-effort stderr
	}
	return doMailInboxTargetWithJSON(remoteInboxReader{c}, resolvedMailTarget{display: recipient, recipients: []string{recipient}}, jsonOut, stdout, stderr)
}
