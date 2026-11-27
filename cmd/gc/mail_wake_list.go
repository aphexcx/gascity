package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

type mailWakeSender struct {
	agent string
	key   string
}

type mailWakeFacts struct {
	recipientID string
	human       bool
	running     bool
	named       bool
	managed     bool
	block       string
	ordinary    bool
}

type mailWakeAction int

const (
	mailWakeNone mailWakeAction = iota
	mailWakeRemind
	mailWakeStart
	mailWakeCreate
)

func decideMailWake(agent config.Agent, sender mailWakeSender, facts mailWakeFacts, incoming, outgoing []session.MailWakeEntry, now time.Time) (mailWakeAction, string) {
	if facts.human || sender.key == "" || !slices.Contains(agent.MailWakeFrom, sender.agent) {
		return mailWakeNone, ""
	}
	if facts.block != "" {
		return mailWakeNone, facts.block
	}
	action := mailWakeRemind
	if !facts.running {
		switch {
		case !agent.MailWakeStart:
			return mailWakeNone, "start-disabled"
		case !facts.named:
			return mailWakeNone, "unnamed"
		case !facts.managed:
			return mailWakeNone, "unmanaged"
		case facts.recipientID == "":
			action = mailWakeCreate
		case !facts.ordinary:
			return mailWakeNone, "sleep-state"
		default:
			action = mailWakeStart
		}
	}
	cutoff := now.Add(-session.MailWakeWindow)
	for _, entry := range outgoing {
		if entry.Sender == facts.recipientID && entry.At.After(cutoff) {
			return mailWakeNone, "return-hop"
		}
	}
	count := 0
	for _, entry := range incoming {
		if entry.Sender == sender.key && entry.At.After(cutoff) {
			count++
		}
	}
	if count >= 3 {
		return mailWakeNone, "budget"
	}
	return action, ""
}

// localMailWake runs after a successful send or reply.
type localMailWake func(mail.Message) bool

var errMailReminderWithdrawn = errors.New("mail reminder withdrawn")

func notifyStoredMail(message mail.Message, explicit nudgeFunc, list localMailWake, command string, stderr io.Writer) bool {
	if message.To == "human" {
		return false
	}
	if explicit != nil {
		if err := explicit(message.To, message.ID); err != nil {
			fmt.Fprintf(stderr, "%s: nudge failed: %v\n", command, err) //nolint:errcheck // best-effort stderr
			return false
		}
		return true
	}
	return list != nil && list(message)
}

type localMailWakePolicy struct {
	cityPath string
	cfg      *config.City
	work     beads.Store
	sessions beads.Store
	front    *session.Store
	mail     mail.Provider
}

func newLocalMailWake(cityPath string, cfg *config.City, store, sessions beads.Store, mp mail.Provider, sender, senderID, command string, stderr io.Writer, now func() time.Time) localMailWake {
	if cfg == nil || store == nil || sessions == nil || !slices.ContainsFunc(cfg.Agents, func(a config.Agent) bool { return len(a.MailWakeFrom) > 0 }) {
		return nil
	}
	p := localMailWakePolicy{cityPath, cfg, store, sessions, sessionFrontDoor(sessions), mp}
	return func(message mail.Message) bool {
		identity := p.sender(sender, senderID, message.FromSessionID)
		if identity.key == "" || message.To == "human" {
			return false
		}
		asked, reason := p.request(message, sender, identity, now().UTC())
		if reason != "" {
			fmt.Fprintf(stderr, "%s: %s: %s\n", command, message.To, reason) //nolint:errcheck // best-effort stderr
		}
		return asked
	}
}

func (p localMailWakePolicy) agent(info session.Info) (config.Agent, bool) {
	for _, name := range []string{info.AgentName, info.Template} {
		if name == "" {
			continue
		}
		a, ok := resolveAgentIdentity(p.cfg, name, "")
		if !ok {
			continue
		}
		identity := agentutil.RoutedToIdentity(&a)
		for _, configured := range p.cfg.Agents {
			if configured.QualifiedName() == identity {
				return configured, true
			}
		}
		// A generic rig template is stored once in config. Its sessions
		// carry a rig-qualified identity synthesized by the resolver.
		_, template := config.ParseQualifiedName(identity)
		if generic := config.FindAgent(p.cfg, template); generic != nil && generic.Dir == "" && generic.Scope == "rig" {
			return *generic, true
		}
	}
	return config.Agent{}, false
}

func (p localMailWakePolicy) sender(address, resolved, stamped string) mailWakeSender {
	if address == controllerMailIdentity {
		return mailWakeSender{}
	}
	if address == "human" && resolved == "" {
		return mailWakeSender{"human", "human"}
	}
	id := firstNonEmpty(resolved, stamped)
	if id == "" {
		return mailWakeSender{}
	}
	info, err := p.front.Get(id)
	if err != nil {
		return mailWakeSender{}
	}
	a, ok := p.agent(info)
	if !ok {
		return mailWakeSender{}
	}
	return mailWakeSender{a.QualifiedName(), id}
}

func (p localMailWakePolicy) request(message mail.Message, address string, sender mailWakeSender, now time.Time) (bool, string) {
	city := loadedCityName(p.cfg, p.cityPath)
	id := message.ToSessionID
	var err error
	if id == "" {
		id, err = resolveSessionIDAllowClosedWithConfig(p.cityPath, p.cfg, p.sessions, message.To)
	}
	var info session.Info
	var a config.Agent
	var facts mailWakeFacts
	if err == nil {
		info, err = p.front.Get(id)
		if err != nil {
			return false, ""
		}
		var ok bool
		a, ok = p.agent(info)
		if !ok {
			return false, ""
		}
		facts.recipientID = id
		facts.named = configuredNamedSessionBeadHasSpecInfo(info, p.cfg, city)
	} else {
		if !errors.Is(err, session.ErrSessionNotFound) {
			return false, ""
		}
		spec, ok, lookupErr := findNamedSessionSpecForTarget(p.cfg, city, message.To)
		if lookupErr != nil || !ok || spec.Agent == nil {
			return false, ""
		}
		a = *spec.Agent
		if !slices.Contains(a.MailWakeFrom, sender.agent) {
			return false, ""
		}
		closed, lookupErr := session.HasClosedOrArchivedNamedSession(p.sessions, spec.Identity)
		if lookupErr != nil {
			return false, "lookup"
		}
		if closed {
			return false, "closed"
		}
		facts.named = true
	}
	if !slices.Contains(a.MailWakeFrom, sender.agent) {
		return false, ""
	}
	// Generic rig templates share one configured sender-list identity. Bind
	// the recipient copy to its concrete rig for the controller's predicate.
	if a.Scope == "rig" && a.Dir == "" {
		for _, identity := range []string{info.Template, info.ConfiguredNamedIdentity, info.AgentName} {
			if dir, _ := config.ParseQualifiedName(identity); dir != "" {
				a.Dir = dir
				break
			}
		}
	}
	if isAgentEffectivelySuspendedWith(p.cfg, p.cityPath, &a, loadSuspensionStateBestEffort(p.cityPath)) {
		return false, "suspended"
	}
	var sp runtime.Provider
	var target nudgeTarget
	if facts.recipientID != "" {
		wait, waitErr := p.front.HasOpenMailWakeWait(id)
		if waitErr != nil {
			return false, "wait-lookup"
		}
		if block := session.MailWakeBlock(info, wait, false); block != "" {
			return false, block
		}
		target = resolveNudgeTargetFromSessionInfo(p.cityPath, p.cfg, info)
		ctx := sessionProviderContextForCity(p.cfg, p.cityPath, os.Getenv("GC_SESSION"))
		sp, err = newSessionProviderFromContext(ctx, newSessionBeadSnapshotFromInfos([]session.Info{info}))
		if err != nil {
			return false, "provider"
		}
		obs, observeErr := workerObserveNudgeTarget(target, p.sessions, sp)
		if observeErr != nil {
			return false, "observe"
		}
		facts.running = obs.Running
		facts.ordinary = session.MailWakeBlock(info, wait, true) == ""
	}
	if !facts.running && a.MailWakeStart && facts.named {
		facts.managed = p.cityPath != "" && nudgeCityUsesManagedReconciler(p.cityPath)
	}
	action, reason := decideMailWake(a, sender, facts, nil, nil, now)
	if action == mailWakeNone {
		return false, reason
	}
	var incoming, outgoing []session.MailWakeEntry
	if id != "" {
		incoming, err = p.front.MailWakeHistory(id)
		if err != nil {
			return false, "history"
		}
	}
	if sender.key != "human" {
		outgoing, err = p.front.MailWakeHistory(sender.key)
		if err != nil {
			return false, "history"
		}
	}
	action, reason = decideMailWake(a, sender, facts, incoming, outgoing, now)
	if action == mailWakeNone {
		return false, reason
	}
	created := action == mailWakeCreate
	if created {
		id, err = resolveSessionIDMaterializingNamed(p.cityPath, p.cfg, p.sessions, message.To)
		if err != nil {
			return false, "create"
		}
		info, err = p.front.Get(id)
		if err != nil {
			return true, "lookup"
		}
		wait, err := p.front.HasOpenMailWakeWait(id)
		if err != nil {
			return true, "wait-lookup"
		}
		if block := session.MailWakeBlock(info, wait, false); block != "" {
			return true, block
		}
		target = resolveNudgeTargetFromSessionInfo(p.cityPath, p.cfg, info)
	}
	if err := p.front.RecordMailWake(id, incoming, session.MailWakeEntry{Sender: sender.key, At: now}); err != nil {
		return created, "history-write"
	}
	item := mailReminderItem(target, address, message.ID, now)
	if action == mailWakeRemind {
		var landed bool
		ctx := runtime.WithNudgeDeliveryCheck(context.Background(), func() error {
			_, blocked, err := blockedQueuedMailNudgeReason(p.mail, item)
			if err != nil {
				return err
			}
			if blocked {
				return errMailReminderWithdrawn
			}
			return nil
		})
		item, landed, err = deliverLiveMailReminder(ctx, target, p.sessions, sp, item)
		if errors.Is(err, errMailReminderWithdrawn) {
			return false, ""
		}
		if err != nil {
			return false, "delivery"
		}
		if landed {
			return true, ""
		}
	}
	nudges := cliNudgesStore(p.work, p.cfg, p.cityPath)
	if err := enqueueQueuedNudgeWithStore(p.cityPath, nudges, item); err != nil {
		if action == mailWakeRemind {
			_ = releaseLiveNudgeReservation(target, sp, item.ID)
		}
		return created, "queue"
	}
	switch action {
	case mailWakeStart:
		asked, err := p.front.RequestMailWake(id, now)
		if err != nil || !asked {
			if err := rollbackQueuedNudge(p.cityPath, nudgeFrontDoor(nudges), item, "mail wake refused"); err != nil {
				return false, "rollback"
			}
			return false, "wake-refused"
		}
		if err := nudgePokeController(p.cityPath); err != nil {
			return true, "poke"
		}
	case mailWakeRemind:
		maybeStartNudgePoller(target)
	}
	return true, ""
}
