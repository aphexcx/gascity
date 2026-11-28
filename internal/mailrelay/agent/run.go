package agent

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gastownhall/gascity/internal/mailrelay/envelope"
	"github.com/gastownhall/gascity/internal/mailrelay/relay"
	"github.com/gastownhall/gascity/internal/supervisor"
)

const (
	backoffFirst = time.Second
	backoffMax   = 60 * time.Second
	// busyWait is the pause before asking again when the relay still holds
	// an earlier read for this city, as it does for a while after a
	// connection died without closing.
	busyWait = time.Second
)

// Deps are the agent's ways out of its process. Nil fields take defaults:
// the default HTTP transport, gc run as a child process, standard error,
// the system clock and a timer.
type Deps struct {
	RelayTransport http.RoundTripper
	APITransport   http.RoundTripper
	GC             Runner
	Log            io.Writer
	Now            func() time.Time
	Sleep          func(ctx context.Context, d time.Duration) error
}

func (d Deps) withDefaults(s Settings) Deps {
	if d.RelayTransport == nil {
		d.RelayTransport = http.DefaultTransport
	}
	if d.APITransport == nil {
		d.APITransport = http.DefaultTransport
	}
	if d.GC == nil {
		d.GC = execRunner{path: s.GC}
	}
	if d.Log == nil {
		d.Log = os.Stderr
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Sleep == nil {
		d.Sleep = sleepContext
	}
	return d
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// backoff waits one second after a failure, doubling to sixty.
type backoff struct{ next time.Duration }

func (b *backoff) step() time.Duration {
	if b.next == 0 {
		b.next = backoffFirst
	}
	d := b.next
	b.next = min(2*b.next, backoffMax)
	return d
}

func (b *backoff) reset() { b.next = 0 }

// eventLog writes one line per event: a UTC timestamp, then key=value pairs.
// No line carries a subject, a body, a grant or key material.
type eventLog struct {
	l   *log.Logger
	now func() time.Time
}

func (e eventLog) printf(format string, args ...any) {
	e.l.Printf("%s %s", e.now().UTC().Format(time.RFC3339Nano), fmt.Sprintf(format, args...))
}

// receiver is one run of the agent's loop.
type receiver struct {
	s      Settings
	d      Deps
	relay  *relayClient
	city   *cityMail
	peers  map[string]ed25519.PublicKey
	ring   *keyRing
	in     *inbound
	status *board
	log    eventLog
}

// Run publishes the city's newest key statement and then keeps one read
// open at the relay, filing each mail through the city's own API before it
// moves the place, and telling the recipient through gc. It returns nil
// when ctx ends.
func Run(ctx context.Context, s Settings, d Deps) (err error) {
	if err := s.checkRun(); err != nil {
		return err
	}
	signer, err := readSigningKey(s.SigningKey)
	if err != nil {
		return err
	}
	peers, err := loadPeers(s.PeersDir)
	if err != nil {
		return err
	}
	ring, err := loadKeyRing(s.EncryptionKeysDir, s.City, signer.Public().(ed25519.PublicKey))
	if err != nil {
		return err
	}
	if err := ensureStateDir(s.StateDir); err != nil {
		return err
	}
	unlock, err := lockDir(s.StateDir, false)
	if errors.Is(err, errLocked) {
		return fmt.Errorf("another agent runs with the state folder %s", s.StateDir)
	}
	if err != nil {
		return err
	}
	defer unlock()
	d = d.withDefaults(s)
	rc, err := newRelayClient(s, signer, d.RelayTransport)
	if err != nil {
		return err
	}
	in, reset, err := loadInbound(s.StateDir, placeOwner{City: s.City, Relay: rc.base.String()})
	if err != nil {
		return err
	}
	city, err := newCityMail(s, d.APITransport)
	if err != nil {
		return err
	}
	r := &receiver{
		s: s, d: d, relay: rc, city: city, peers: peers, ring: ring, in: in,
		status: newBoard(s.City, in.place), log: eventLog{l: log.New(d.Log, "", 0), now: d.Now},
	}
	if s.Socket != "" {
		stop, err := serveHealth(s.Socket, r.status)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, stop()) }()
	}
	if reset != nil {
		r.log.printf("place-reset from=%d reason=%s", reset.from, reset.reason)
	}
	r.log.printf("start city=%s place=%d keys=%d peers=%d key-id=%s", s.City, in.place, len(ring.private), len(peers), ring.newest.KeyID)
	if err := r.publish(ctx); err != nil {
		return err
	}
	r.loop(ctx)
	r.log.printf("stop place=%d", r.in.place)
	return nil
}

// loadPeers reads the pinned signing keys of the cities this city may mail.
func loadPeers(dir string) (map[string]ed25519.PublicKey, error) {
	if err := checkPins(dir); err != nil {
		return nil, err
	}
	keys, refused, err := supervisor.LoadTrustedKeys(dir)
	if err != nil {
		return nil, fmt.Errorf("peers folder: %w", err)
	}
	if len(refused) > 0 {
		return nil, fmt.Errorf("peers folder %s holds %d unusable key files; the first: %w", dir, len(refused), refused[0])
	}
	for id := range keys {
		if !envelope.ValidCityID(id) {
			return nil, fmt.Errorf("peer key %s.pub in %s is not named for a relay city id", id, dir)
		}
	}
	return keys, nil
}

// checkPins refuses a peers folder or pin file that the group or others can
// change: whoever can change a pin can forge that peer's mail and read mail
// sent to it.
func checkPins(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("peers folder: %w", err)
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("peers folder %s can be changed by group or others (mode %#o)", dir, perm)
	}
	pins, err := filepath.Glob(filepath.Join(dir, "*.pub"))
	if err != nil {
		return fmt.Errorf("peers folder: %w", err)
	}
	for _, pin := range pins {
		info, err := os.Lstat(pin)
		if err != nil {
			return fmt.Errorf("peer key: %w", err)
		}
		if perm := info.Mode().Perm(); perm&0o022 != 0 {
			return fmt.Errorf("peer key %s can be changed by group or others (mode %#o)", pin, perm)
		}
	}
	return nil
}

// publish puts the newest key statement on the relay, which accepts the same
// statement again, so it is put at every start. A relay that refuses it
// stops the run: peers would seal to a key this city cannot open.
func (r *receiver) publish(ctx context.Context) error {
	var b backoff
	for {
		code, err := r.relay.publish(ctx, r.ring.newestWire)
		switch {
		case ctx.Err() != nil:
			return nil
		case err == nil && code == http.StatusNoContent:
			r.log.printf("published key-id=%s issued=%d", r.ring.newest.KeyID, r.ring.newest.Issued)
			r.status.degrade("")
			return nil
		case err == nil && (code == http.StatusBadRequest || code == http.StatusForbidden || code == http.StatusConflict):
			return fmt.Errorf("relay refused this city's key statement %s with HTTP %d", r.ring.newest.KeyID, code)
		case err != nil:
			r.log.printf("publish-failed reason=%s", transportReason(ctx, err))
		default:
			r.log.printf("publish-failed status=%d", code)
		}
		r.status.degrade("relay: key statement not published")
		if r.d.Sleep(ctx, b.step()) != nil {
			return nil
		}
	}
}

// loop asks, handles each delivery in order, and asks again. The place moves
// only past a delivery that is filed, passed or dropped.
func (r *receiver) loop(ctx context.Context) {
	var relayWait, apiWait, pinsWait backoff
	for ctx.Err() == nil {
		batch, out := r.ask(ctx)
		switch out.kind {
		case outcomeStopped:
			return
		case outcomeBusy:
			if r.d.Sleep(ctx, busyWait) != nil {
				return
			}
			continue
		case outcomeError:
			if out.status == http.StatusBadRequest && r.in.place > 0 {
				// The relay refuses a place above the highest number it
				// handed out, as after it lost its store. Waiting would let
				// its new counter pass the place and delete mail unread, so
				// start over: what is filed again is passed as seen or
				// filed under the same key.
				from := r.in.place
				if err := r.in.reset(); err != nil {
					r.log.printf("state-failed error=%q", err.Error())
				} else {
					r.log.printf("place-reset from=%d status=%d", from, out.status)
					r.status.update(func(s *Status) { s.Place = 0 })
					continue
				}
			}
			fallthrough
		case outcomeCut:
			r.status.degrade("relay: last read " + out.kind)
			if r.d.Sleep(ctx, relayWait.step()) != nil {
				return
			}
			continue
		}
		relayWait.reset()
		if !r.readPins() {
			if r.d.Sleep(ctx, pinsWait.step()) != nil {
				return
			}
			continue
		}
		pinsWait.reset()
		handled := true
		for _, dl := range batch {
			done, err := r.handle(ctx, dl)
			if err != nil {
				r.log.printf("state-failed number=%d error=%q", dl.Number, err.Error())
				r.status.degrade("state: " + err.Error())
				if r.d.Sleep(ctx, apiWait.step()) != nil {
					return
				}
				handled = false
				break
			}
			if !done {
				if ctx.Err() == nil && r.d.Sleep(ctx, apiWait.step()) != nil {
					return
				}
				handled = false
				break
			}
			apiWait.reset()
		}
		if handled {
			r.status.degrade("")
		}
	}
}

// ask makes one held read from the place and logs one line for it.
func (r *receiver) ask(ctx context.Context) ([]relay.Delivery, askOutcome) {
	began := r.d.Now()
	batch, out := r.relay.read(ctx, r.in.place, r.s.Wait)
	ended := r.d.Now()
	line := fmt.Sprintf("ask began=%s ended=%s ms=%d after=%d outcome=%s",
		began.UTC().Format(time.RFC3339Nano), ended.UTC().Format(time.RFC3339Nano), ended.Sub(began).Milliseconds(), r.in.place, out.kind)
	if out.count > 0 {
		line += " count=" + strconv.Itoa(out.count)
	}
	if out.status != 0 && out.kind != outcomeMail && out.kind != outcomeEmpty {
		line += " status=" + strconv.Itoa(out.status)
	}
	if out.reason != "" {
		line += " reason=" + out.reason
	}
	r.log.printf("%s", line)
	r.status.update(func(s *Status) { s.LastAsk = &AskRecord{Began: began, Ended: ended, Outcome: out.kind} })
	return batch, out
}

// handle takes one delivery to its end. It reports done when the place may
// move past it; an error means the agent's own state could not be saved.
func (r *receiver) handle(ctx context.Context, dl relay.Delivery) (bool, error) {
	if dl.Number <= r.in.place {
		return true, nil
	}
	now := r.d.Now()
	env, err := envelope.ParseEnvelope(dl.Envelope)
	if err != nil {
		return r.drop(dl, envelope.Label{}, "malformed", "")
	}
	label := env.Label
	m, reason := r.open(env, now)
	if reason != "" {
		return r.drop(dl, label, reason, "")
	}
	if rule, bad := unfitText(m); bad {
		return r.drop(dl, label, "malformed", "rule="+rule.name)
	}
	key := filingKey(label.From, label.ID)
	if r.in.hasSeen(key, now) {
		r.log.printf("skip number=%d id=%q from=%s reason=seen", dl.Number, label.ID, label.From)
		return r.advance(dl.Number)
	}
	res := r.city.file(ctx, key, label.From, m)
	switch res.kind {
	case filingFiled:
		if err := r.in.remember(key, now); err != nil {
			return false, err
		}
		r.log.printf("filed number=%d id=%q from=%s mail=%s", dl.Number, label.ID, label.From, res.id)
		r.tell(ctx, dl.Number, label, m, res)
		if ctx.Err() != nil {
			// Stopped while telling: the place stays, and the copy the relay
			// hands over next time is passed as seen.
			return false, nil
		}
		return r.advance(dl.Number)
	case filingMismatch:
		r.log.printf("fault kind=mismatch number=%d id=%q from=%s %s", dl.Number, label.ID, label.From, res.detail)
		r.status.fault(Fault{At: now, Kind: "mismatch", Number: dl.Number, From: label.From, ID: label.ID, Detail: res.detail})
		if err := r.in.remember(key, now); err != nil {
			return false, err
		}
		return r.advance(dl.Number)
	case filingRefused:
		return r.drop(dl, label, "refused", res.detail)
	}
	r.log.printf("file-failed number=%d id=%q from=%s %s", dl.Number, label.ID, label.From, res.detail)
	r.status.degrade("city API: mail not filed (" + res.detail + ")")
	return false, nil
}

func (r *receiver) advance(number int64) (bool, error) {
	if err := r.in.advance(number); err != nil {
		return false, err
	}
	r.status.update(func(s *Status) { s.Place = number })
	return true, nil
}

// drop logs a mail by its label and reason, never its contents, and passes it.
func (r *receiver) drop(dl relay.Delivery, label envelope.Label, reason, detail string) (bool, error) {
	line := fmt.Sprintf("drop number=%d id=%q from=%s to=%s sent=%d reason=%s", dl.Number, label.ID, label.From, label.To, label.Sent, reason)
	if detail != "" {
		line += " " + detail
	}
	r.log.printf("%s", line)
	r.status.fault(Fault{At: r.d.Now(), Kind: "dropped", Number: dl.Number, From: label.From, ID: label.ID, Detail: reason})
	return r.advance(dl.Number)
}

func (r *receiver) tell(ctx context.Context, number int64, label envelope.Label, m envelope.Mail, res filingResult) {
	recipient := res.to
	if recipient == "" {
		recipient = m.To
	}
	via, err := tell(ctx, r.d.GC, notice{recipient: recipient, sender: label.From + "/" + m.From, subject: m.Subject, mailID: res.id})
	if err != nil {
		r.log.printf("tell-failed number=%d mail=%s %s", number, res.id, err.Error())
		r.status.fault(Fault{At: r.d.Now(), Kind: "tell", Number: number, From: label.From, ID: label.ID, Detail: "gc could not tell the recipient"})
		return
	}
	r.log.printf("told number=%d mail=%s via=%s", number, res.id, via)
}

// readPins reads the peers folder before a batch, so a pin added, changed
// or removed while the agent runs counts from the next batch on. When the
// folder cannot be read the batch is left at the relay, to be asked again:
// the agent can no longer vouch for the pins it holds, and passing the mail
// over would lose it for a folder that may read again in a moment.
func (r *receiver) readPins() bool {
	peers, err := loadPeers(r.s.PeersDir)
	if err != nil {
		r.log.printf("peers-failed error=%q", err.Error())
		r.status.degrade("peers: folder could not be read")
		return false
	}
	if !maps.EqualFunc(peers, r.peers, func(a, b ed25519.PublicKey) bool { return a.Equal(b) }) {
		r.peers = peers
		r.log.printf("peers-changed peers=%d", len(peers))
	}
	return true
}

// open opens and checks a mail from a pinned peer and returns why it did not
// open. The pins are the ones read before this batch.
func (r *receiver) open(env envelope.Envelope, now time.Time) (envelope.Mail, string) {
	from := env.Label.From
	signer, ok := r.peers[from]
	if !ok {
		return envelope.Mail{}, "not-a-peer"
	}
	m, err := envelope.Open(env, r.s.City, r.ring.private, from, signer, now)
	if err != nil {
		return envelope.Mail{}, openReason(err)
	}
	return m, ""
}

// openReason names why a mail did not open.
func openReason(err error) string {
	switch {
	case errors.Is(err, envelope.ErrSignature):
		return "bad-signature"
	case errors.Is(err, envelope.ErrTime):
		return "outside-window"
	case errors.Is(err, envelope.ErrAddress):
		return "address-mismatch"
	case errors.Is(err, envelope.ErrFormat), errors.Is(err, envelope.ErrSize):
		return "malformed"
	}
	return "not-opened"
}
