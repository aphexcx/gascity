package agent

import (
	"strings"

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
	}
	return contentRule{}, false
}
