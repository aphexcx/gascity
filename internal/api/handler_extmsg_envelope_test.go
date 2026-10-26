package api

import (
	"testing"
)

func extmsgEnvelopeFixtures() []struct {
	name     string
	reminder extmsgNotifyReminder
	long     string
} {
	const instructions = "To reply in Slack, run:\n" +
		"  gc slack-mini post-message --channel {conversation_id}[ --thread-ts {thread_ts}] --text '<your reply>'\n" +
		"Prefix your reply with **{handle}:**."
	return []struct {
		name     string
		reminder extmsgNotifyReminder
		long     string
	}{
		{
			name: "human",
			reminder: extmsgNotifyReminder{
				Provider: "slack", ConversationID: "C123", ActorDisplay: "Afik Cohen",
				ActorKind: "human", Text: "status?", Handle: "worker",
				ProviderMessageID: "1700000002.0002", ReplyInstructions: instructions,
			},
			long: `<system-reminder>
New message in shared conversation slack/C123:

- Afik Cohen (human): status?

Message id: 1700000002.0002

To reply in Slack, run:
  gc slack-mini post-message --channel C123 --thread-ts 1700000002.0002 --text '<your reply>'
Prefix your reply with **worker:**.
</system-reminder>`,
		},
		{
			name: "thread",
			reminder: extmsgNotifyReminder{
				Provider: "slack", ConversationID: "C123", ActorDisplay: "Afik Cohen",
				ActorKind: "human", Text: "status?", Handle: "worker",
				ProviderMessageID: "1700000002.0002", ReplyToMessageID: "1700000001.0001",
				ReplyInstructions: instructions,
			},
			long: `<system-reminder>
New message in shared conversation slack/C123:

- Afik Cohen (human): status?

Message id: 1700000002.0002 (in thread 1700000001.0001)

To reply in Slack, run:
  gc slack-mini post-message --channel C123 --thread-ts 1700000001.0001 --text '<your reply>'
Prefix your reply with **worker:**.
</system-reminder>`,
		},
		{
			name: "peer",
			reminder: extmsgNotifyReminder{
				Provider: "slack", ConversationID: "C123", ActorDisplay: "peer",
				ActorKind: "agent", Text: "posted an update", Handle: "worker",
				ReplyInstructions: instructions,
			},
			long: `<system-reminder>
New message in shared conversation slack/C123:

- peer (agent): posted an update

To reply in Slack, run:
  gc slack-mini post-message --channel C123 --text '<your reply>'
Prefix your reply with **worker:**.
</system-reminder>`,
		},
		{
			name: "batch",
			reminder: extmsgNotifyReminder{
				Provider: "slack", ConversationID: "C123", ActorDisplay: "alice",
				ActorKind: "human", Text: "status?\nship it", Handle: "worker",
				ExplicitTarget: "reviewer", ProviderMessageID: "1700000002.0002",
				ReplyToMessageID: "1700000001.0001",
			},
			long: `<system-reminder>
New message in shared conversation slack/C123:

- alice (human): status?
ship it

Message id: 1700000002.0002 (in thread 1700000001.0001)

Addressed to: @reviewer — if that is not you, do not reply.

To reply in Slack, write your response to a file and run:
  gc slack reply-current --conversation-id C123 --body-file <path>
</system-reminder>`,
		},
	}
}

func TestFormatExtmsgNotifyReminderLongGolden(t *testing.T) {
	for _, fixture := range extmsgEnvelopeFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			got := formatExtmsgNotifyReminder(fixture.reminder)
			if got != fixture.long {
				t.Fatalf("reminder differs from golden:\ngot: %q\nwant: %q", got, fixture.long)
			}
			t.Logf("bytes=%d text_bytes=%d envelope_bytes=%d", len(got), len(fixture.reminder.Text), len(got)-len(fixture.reminder.Text))
		})
	}
}
