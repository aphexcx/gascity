package agent

import (
	"strings"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/mailrelay/envelope"
)

// maxSubjectBytes is the longest subject a city's store takes.
const maxSubjectBytes = 500

// contentRule is one way a mail's text is unfit to file.
type contentRule struct {
	name string // for a log line
	text string // for an error
}

// A city's store takes a subject as an argument of its own command line, so
// it cannot take one that is blank, reads as an option, or runs past its
// limit. Such a mail would fail every try and hold every mail behind it.
var (
	ruleSubjectBlank = contentRule{"subject-blank", "the subject is blank"}
	ruleSubjectDash  = contentRule{"subject-starts-with-dash", "the subject starts with a dash"}
	ruleSubjectLong  = contentRule{"subject-over-500-bytes", "the subject is over 500 bytes"}
)

// The city's API carries the mail as JSON, which would change a subject or
// body that is not UTF-8 from what the sender signed.
var (
	ruleSubjectText = contentRule{"subject-not-utf8", "the subject is not valid UTF-8"}
	ruleBodyText    = contentRule{"body-not-utf8", "the body is not valid UTF-8"}
)

// The sender's name is printed wherever the mail is listed, so it must be
// text and not a command to a terminal.
var ruleSenderControl = contentRule{"sender-control-character", "the sender name holds a control character"}

// unfitText returns the first rule a mail's text breaks. send refuses such a
// mail and the receiver passes it over unfiled.
func unfitText(m envelope.Mail) (contentRule, bool) {
	switch {
	case strings.TrimSpace(m.Subject) == "":
		return ruleSubjectBlank, true
	case strings.HasPrefix(m.Subject, "-"):
		return ruleSubjectDash, true
	case len(m.Subject) > maxSubjectBytes:
		return ruleSubjectLong, true
	case !utf8.ValidString(m.Subject):
		return ruleSubjectText, true
	case !utf8.ValidString(m.Body):
		return ruleBodyText, true
	case strings.ContainsFunc(m.From, controlCharacter):
		return ruleSenderControl, true
	}
	return contentRule{}, false
}

// controlCharacter reports a C0 or C1 control, DEL, or a line or paragraph
// separator.
func controlCharacter(r rune) bool {
	return r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == '\u2028' || r == '\u2029'
}
