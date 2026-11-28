package config

import "strings"

// MailWakePeerPrefix starts a mail_wake_from entry that names a peer city by
// the id of its registered verify key. No agent name can hold a colon, so an
// entry with this prefix is never an agent.
const MailWakePeerPrefix = "city:"

// MailWakePeer returns the mail_wake_from entry naming the peer city whose
// registered verify key has the given id.
func MailWakePeer(kid string) string {
	return MailWakePeerPrefix + kid
}

// MailWakePeerKid returns the key id a mail_wake_from entry names and whether
// the entry names a peer city at all. The id is everything after the prefix,
// so the caller decides whether it is a valid one.
func MailWakePeerKid(entry string) (string, bool) {
	return strings.CutPrefix(entry, MailWakePeerPrefix)
}

// ValidVerifyKeyID reports whether kid may identify a registered verify key.
// It is the rule the trusted-keys directory applies to a key file's name, and
// the rule a peer entry of mail_wake_from must pass.
func ValidVerifyKeyID(kid string) bool {
	return kid != "" && strings.TrimSpace(kid) == kid && !strings.ContainsAny(kid, ":,\r\n\x00")
}
