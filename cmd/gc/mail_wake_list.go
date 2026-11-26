package main

import (
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail"
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

func decideMailWake(config.Agent, mailWakeSender, mailWakeFacts, []session.MailWakeEntry, []session.MailWakeEntry, time.Time) (mailWakeAction, string) {
	return mailWakeNone, ""
}

// localMailWake runs after a successful send or reply.
type localMailWake func(mail.Message) bool

func notifyStoredMail(mail.Message, nudgeFunc, localMailWake, string, io.Writer) bool {
	return false
}

func newLocalMailWake(string, *config.City, beads.Store, beads.Store, string, string, string, io.Writer, func() time.Time) localMailWake {
	return nil
}
