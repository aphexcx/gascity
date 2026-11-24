package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/spf13/cobra"
)

// newExtMsgCmd groups the external-conversation binding verbs. These are
// thin projections over the extmsg binding service via the city API —
// there is deliberately no local fallback: conversation bindings are live
// controller state, and mutating the bead store behind a running
// controller's back would race its delivery pipeline.
func newExtMsgCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "extmsg",
		Short: "Manage external-conversation bindings",
		Long: `Manage bindings between external conversations (telegram, discord, ...)
and gc sessions or configured agents.

A conversation bound to an agent name survives session restarts: inbound
messages resolve a live session for the agent at delivery time, cold-waking
one when none is live. "handoff" rebinds a conversation to another agent —
the front-desk pattern: a default-routed agent inspects the conversation
and hands it to the right specialist.

These commands require the city API server; they have no local fallback.`,
	}
	cmd.AddCommand(newExtMsgBindCmd(stdout, stderr))
	cmd.AddCommand(newExtMsgHandoffCmd(stdout, stderr))
	cmd.AddCommand(newExtMsgUnbindCmd(stdout, stderr))
	cmd.AddCommand(newExtMsgBindingsCmd(stdout, stderr))
	return cmd
}

// extMsgConversationFlags collects the flags identifying one conversation.
type extMsgConversationFlags struct {
	scopeID              string
	provider             string
	accountID            string
	conversationID       string
	parentConversationID string
	kind                 string
}

func addExtMsgConversationFlags(cmd *cobra.Command, f *extMsgConversationFlags) {
	cmd.Flags().StringVar(&f.provider, "provider", "", "External messaging provider (required)")
	cmd.Flags().StringVar(&f.conversationID, "conversation-id", "", "Provider conversation ID (required)")
	cmd.Flags().StringVar(&f.accountID, "account-id", "default", "Adapter account ID")
	cmd.Flags().StringVar(&f.scopeID, "scope-id", "", "Conversation scope (default: the city name)")
	cmd.Flags().StringVar(&f.parentConversationID, "parent-conversation-id", "", "Parent conversation ID for thread conversations")
	cmd.Flags().StringVar(&f.kind, "kind", "dm", "Conversation kind: dm, room, or thread")
}

// conversationRef materializes the flags into a ConversationRef, defaulting
// the scope to the city name (the convention bridges stamp on inbound).
func (f *extMsgConversationFlags) conversationRef(cityPath string) (extmsg.ConversationRef, error) {
	if strings.TrimSpace(f.provider) == "" {
		return extmsg.ConversationRef{}, fmt.Errorf("--provider is required")
	}
	if strings.TrimSpace(f.conversationID) == "" {
		return extmsg.ConversationRef{}, fmt.Errorf("--conversation-id is required")
	}
	scope := strings.TrimSpace(f.scopeID)
	if scope == "" {
		cfg, _, err := config.LoadWithIncludesOptions(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"), skipRevisionSnapshot)
		if err != nil {
			return extmsg.ConversationRef{}, fmt.Errorf("resolving default scope from city config: %w", err)
		}
		scope = loadedCityName(cfg, cityPath)
	}
	return extmsg.ConversationRef{
		ScopeID:              scope,
		Provider:             f.provider,
		AccountID:            f.accountID,
		ConversationID:       f.conversationID,
		ParentConversationID: f.parentConversationID,
		Kind:                 extmsg.ConversationKind(f.kind),
	}, nil
}

// conversationRefIfSet returns nil when no conversation flags were given,
// letting unbind filter purely by session/agent.
func (f *extMsgConversationFlags) conversationRefIfSet(cityPath string) (*extmsg.ConversationRef, error) {
	if strings.TrimSpace(f.provider) == "" && strings.TrimSpace(f.conversationID) == "" {
		return nil, nil
	}
	ref, err := f.conversationRef(cityPath)
	if err != nil {
		return nil, err
	}
	return &ref, nil
}

// extMsgClient resolves the city and returns its API client, failing with
// a uniform message when the API is unavailable.
func extMsgClient(verb string, stderr io.Writer) (*api.Client, string, bool) {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc extmsg %s: %v\n", verb, err) //nolint:errcheck // best-effort stderr
		return nil, "", false
	}
	c := apiClient(cityPath)
	if c == nil {
		fmt.Fprintf(stderr, "gc extmsg %s: requires the city API server (no local fallback for conversation bindings)\n", verb) //nolint:errcheck // best-effort stderr
		return nil, "", false
	}
	return c, cityPath, true
}

func extMsgReportBindError(verb string, c *api.Client, err error, stderr io.Writer) int {
	if api.ShouldFallback(c, err) {
		fmt.Fprintf(stderr, "gc extmsg %s: city API unreachable (no local fallback for conversation bindings): %v\n", verb, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stderr, "gc extmsg %s: %v\n", verb, err) //nolint:errcheck // best-effort stderr
	return 1
}

func printExtMsgBinding(stdout io.Writer, jsonOutput bool, record extmsg.SessionBindingRecord, action string) int {
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		if err := enc.Encode(record); err != nil {
			return 1
		}
		return 0
	}
	target := record.SessionID
	if record.AgentName != "" {
		target = "agent " + record.AgentName
	}
	fmt.Fprintf(stdout, "%s %s/%s -> %s (binding %s, generation %d)\n", //nolint:errcheck // best-effort stdout
		action, record.Conversation.Provider, record.Conversation.ConversationID, target, record.ID, record.BindingGeneration)
	return 0
}

func newExtMsgBindCmd(stdout, stderr io.Writer) *cobra.Command {
	var conv extMsgConversationFlags
	var agentName, sessionID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "bind",
		Short: "Bind a conversation to a session or configured agent",
		Long: `Bind an external conversation to a concrete session (--session) or to a
configured agent (--agent). Agent bindings survive session restarts:
delivery resolves a live session for the agent each time, cold-waking one
when none is live. Binding an actively-bound conversation conflicts; use
"gc extmsg handoff" to rebind.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdExtMsgBind(conv, agentName, sessionID, false, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	addExtMsgConversationFlags(cmd, &conv)
	cmd.Flags().StringVar(&agentName, "agent", "", "Configured agent identity to bind (mutually exclusive with --session)")
	cmd.Flags().StringVar(&sessionID, "session", "", "Session ID to bind (mutually exclusive with --agent)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output the binding record as JSON")
	return cmd
}

func newExtMsgHandoffCmd(stdout, stderr io.Writer) *cobra.Command {
	var conv extMsgConversationFlags
	var to string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "handoff",
		Short: "Rebind a conversation to another configured agent",
		Long: `Rebind an external conversation to another configured agent, replacing
the active binding. Run from inside an agent session to hand a
conversation to the right specialist — the routing judgment lives in the
agent's prompt, this verb is pure transport.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if strings.TrimSpace(to) == "" {
				fmt.Fprintln(stderr, "gc extmsg handoff: --to is required") //nolint:errcheck // best-effort stderr
				return errExit
			}
			if cmdExtMsgBind(conv, to, "", true, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	addExtMsgConversationFlags(cmd, &conv)
	cmd.Flags().StringVar(&to, "to", "", "Configured agent identity to hand the conversation to (required)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output the binding record as JSON")
	return cmd
}

// cmdExtMsgBind backs both bind (replace=false) and handoff (replace=true).
func cmdExtMsgBind(conv extMsgConversationFlags, agentName, sessionID string, replace, jsonOutput bool, stdout, stderr io.Writer) int {
	verb := "bind"
	action := "bound"
	if replace {
		verb = "handoff"
		action = "handed off"
	}
	agentName = strings.TrimSpace(agentName)
	sessionID = strings.TrimSpace(sessionID)
	switch {
	case agentName == "" && sessionID == "":
		fmt.Fprintf(stderr, "gc extmsg %s: --agent or --session is required\n", verb) //nolint:errcheck // best-effort stderr
		return 1
	case agentName != "" && sessionID != "":
		fmt.Fprintf(stderr, "gc extmsg %s: --agent and --session are mutually exclusive\n", verb) //nolint:errcheck // best-effort stderr
		return 1
	}
	c, cityPath, ok := extMsgClient(verb, stderr)
	if !ok {
		return 1
	}
	ref, err := conv.conversationRef(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc extmsg %s: %v\n", verb, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	record, err := c.BindExtMsgConversation(api.ExtMsgBindSpec{
		Conversation: ref,
		SessionID:    sessionID,
		AgentName:    agentName,
		Replace:      replace,
	})
	if err != nil {
		return extMsgReportBindError(verb, c, err, stderr)
	}
	return printExtMsgBinding(stdout, jsonOutput, record, action)
}

func newExtMsgUnbindCmd(stdout, stderr io.Writer) *cobra.Command {
	var conv extMsgConversationFlags
	var input extmsg.UnbindInput
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "unbind",
		Short: "Remove active conversation bindings",
		Long: `Remove active external-conversation bindings. Filter by conversation
(--provider/--conversation-id), by --agent, by --session, or a
combination. At least one filter is required.

Use --binding-id alone to remove exactly the binding you inspected (404 if
it is no longer active). Or combine conversation flags with --expect-agent
and --expect-caller to remove only a matching active binding (409 if changed).
For a default route, the expected caller is extmsg-default-route.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			for _, flag := range []string{"binding-id", "expect-agent", "expect-caller"} {
				value, err := cmd.Flags().GetString(flag)
				if err != nil {
					return err
				}
				if cmd.Flags().Changed(flag) && strings.TrimSpace(value) == "" {
					return fmt.Errorf("--%s must not be empty", flag)
				}
			}
			if cmdExtMsgUnbind(conv, input, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	addExtMsgConversationFlags(cmd, &conv)
	cmd.Flags().StringVar(&input.AgentName, "agent", "", "Unbind conversations bound to this configured agent")
	cmd.Flags().StringVar(&input.SessionID, "session", "", "Unbind conversations bound to this session ID")
	cmd.Flags().StringVar(&input.BindingID, "binding-id", "", "Remove exactly this active binding (exclusive with other filters)")
	cmd.Flags().StringVar(&input.ExpectedAgentName, "expect-agent", "", "Require this agent on the conversation's active binding")
	cmd.Flags().StringVar(&input.ExpectedCallerID, "expect-caller", "", "Require this caller ID on the conversation's active binding")
	cmd.MarkFlagsRequiredTogether("expect-agent", "expect-caller")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output the removed binding records as JSON")
	return cmd
}

func cmdExtMsgUnbind(conv extMsgConversationFlags, input extmsg.UnbindInput, jsonOutput bool, stdout, stderr io.Writer) int {
	input.AgentName = strings.TrimSpace(input.AgentName)
	input.SessionID = strings.TrimSpace(input.SessionID)
	c, cityPath, ok := extMsgClient("unbind", stderr)
	if !ok {
		return 1
	}
	ref, err := conv.conversationRefIfSet(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc extmsg unbind: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if ref == nil && input.AgentName == "" && input.SessionID == "" && input.BindingID == "" && input.ExpectedAgentName == "" && input.ExpectedCallerID == "" {
		fmt.Fprintln(stderr, "gc extmsg unbind: a conversation (--provider/--conversation-id), --agent, or --session is required") //nolint:errcheck // best-effort stderr
		return 1
	}
	input.Conversation = ref
	unbound, err := c.UnbindExtMsgBindings(input)
	if err != nil {
		return extMsgReportBindError("unbind", c, err, stderr)
	}
	if jsonOutput {
		enc := json.NewEncoder(stdout)
		if err := enc.Encode(unbound); err != nil {
			return 1
		}
		return 0
	}
	if len(unbound) == 0 {
		fmt.Fprintln(stdout, "No active bindings matched.") //nolint:errcheck // best-effort stdout
		return 0
	}
	for _, record := range unbound {
		target := record.SessionID
		if record.AgentName != "" {
			target = "agent " + record.AgentName
		}
		fmt.Fprintf(stdout, "unbound %s/%s from %s (binding %s)\n", //nolint:errcheck // best-effort stdout
			record.Conversation.Provider, record.Conversation.ConversationID, target, record.ID)
	}
	return 0
}

func newExtMsgBindingsCmd(stdout, stderr io.Writer) *cobra.Command {
	var conv extMsgConversationFlags
	var sessionID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use: "bindings", Short: "List active external-conversation bindings and their caller",
		Long: `List active bindings by conversation (--provider/--conversation-id) or
--session. Conversation results include agent-name bindings. JSON records
include caller.kind, caller.id, and created_at for conditional unbinds.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			c, cityPath, ok := extMsgClient("bindings", stderr)
			if !ok {
				return errExit
			}
			ref, err := conv.conversationRefIfSet(cityPath)
			if err == nil {
				var records []extmsg.SessionBindingRecord
				records, err = c.ListExtMsgBindings(ref, sessionID)
				if err == nil {
					if jsonOutput {
						return json.NewEncoder(stdout).Encode(records)
					}
					for _, record := range records {
						if printExtMsgBinding(stdout, false, record, "bound") != 0 {
							return errExit
						}
						fmt.Fprintf(stdout, "  caller %s/%s, created %s\n", record.Caller.Kind, record.Caller.ID, record.CreatedAt.Format("2006-01-02T15:04:05Z07:00")) //nolint:errcheck // best-effort stdout
					}
					return nil
				}
			}
			fmt.Fprintf(stderr, "gc extmsg bindings: %v\n", err) //nolint:errcheck // best-effort stderr
			return errExit
		},
	}
	addExtMsgConversationFlags(cmd, &conv)
	cmd.Flags().StringVar(&sessionID, "session", "", "List bindings for this session ID instead of a conversation")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output binding records including caller provenance as JSON")
	return cmd
}
