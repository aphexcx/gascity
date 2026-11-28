package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// gcTimeout bounds each gc command the agent runs.
const gcTimeout = 45 * time.Second

// maxTellText bounds the sender and subject quoted to a recipient.
const maxTellText = 200

// humanRecipient is gc's own mailbox for the person at the city; it has no
// session to tell.
const humanRecipient = "human"

// Runner runs the gc program with arguments; a non-nil error is a failure.
type Runner interface {
	Run(ctx context.Context, args ...string) error
}

// execRunner runs gc as a bounded child process, in the agent's working
// folder and environment, so gc finds the city as it does for any command
// run there. gc's output is not kept: it can quote the mail's subject.
type execRunner struct{ path string }

func (r execRunner) Run(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, gcTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.path, args...)
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
}

// notice is what the recipient is told about one filed mail.
type notice struct {
	recipient, sender, subject, mailID string
}

// tell asks gc to nudge the recipient's session. gc answers whether it
// reached the session or queued its wake; when it could not, the recipient
// gets a one-line local notice mailed with --notify, which wakes a stopped
// session. It returns how the recipient was told.
func tell(ctx context.Context, gc Runner, n notice) (string, error) {
	if n.recipient == humanRecipient {
		return "none", nil
	}
	sender, subject := plainText(n.sender), plainText(n.subject)
	nudge := fmt.Sprintf("New relay mail from %s: %s", sender, subject)
	line := fmt.Sprintf("Relay mail from %s: %s", sender, subject)
	if n.mailID != "" {
		nudge += " (gc mail read " + n.mailID + ")"
		line = fmt.Sprintf("Relay mail from %s filed as %s: %s", sender, n.mailID, subject)
	}
	nudgeErr := gc.Run(ctx, "session", "nudge", "--", n.recipient, nudge)
	if nudgeErr == nil {
		return "nudge", nil
	}
	noticeErr := gc.Run(ctx, "mail", "send", "--notify", "--from="+humanRecipient, "--to="+n.recipient, "--subject="+line)
	if noticeErr == nil {
		return "notice", nil
	}
	redact := func(err error) string {
		s := err.Error()
		for _, secret := range []string{nudge, line, n.subject, subject} {
			if secret != "" {
				s = strings.ReplaceAll(s, secret, "[text]")
			}
		}
		return truncate(s, maxTellText)
	}
	return "", fmt.Errorf("nudge=%q notice=%q", redact(nudgeErr), redact(noticeErr))
}

// plainText makes text safe to type into a session: control characters
// become spaces and the length is bounded.
func plainText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	return truncate(strings.TrimSpace(s), maxTellText)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
