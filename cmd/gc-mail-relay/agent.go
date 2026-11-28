package main

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/mailrelay/agent"
	"github.com/spf13/cobra"
)

// newAgentCommands returns a city's verbs: keygen, send and run. Settings
// come from a settings file, the environment read through getenv, and flags.
func newAgentCommands(getenv func(string) string) []*cobra.Command {
	return []*cobra.Command{newKeygenCommand(getenv), newSendCommand(getenv), newRunCommand(getenv)}
}

func addSettingFlags(cmd *cobra.Command) {
	for _, s := range agent.SettingUsage {
		cmd.Flags().String(s.Name, "", fmt.Sprintf("%s (env %s)", s.Usage, agent.EnvName(s.Name)))
	}
}

func resolveSettings(cmd *cobra.Command, getenv func(string) string) (agent.Settings, error) {
	given := make(map[string]string)
	for _, s := range agent.SettingUsage {
		if f := cmd.Flags().Lookup(s.Name); f != nil && f.Changed {
			given[s.Name] = f.Value.String()
		}
	}
	return agent.ResolveSettings(getenv, given)
}

func newKeygenCommand(getenv func(string) string) *cobra.Command {
	var rotate bool
	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Make this city's encryption key and its signed key statement",
		Long: `Make this city's encryption key in the encryption key folder, with its key
statement signed by the city's signing key, and print the public key.
keygen never overwrites a key: with a key present it refuses unless --rotate
is given, which adds a new key with a new key id and keeps the old one, so
mail sealed to it still opens. keygen contacts nobody.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := resolveSettings(cmd, getenv)
			if err != nil {
				return err
			}
			_, err = agent.Keygen(s, rotate, cmd.OutOrStdout())
			return err
		},
	}
	addSettingFlags(cmd)
	cmd.Flags().BoolVar(&rotate, "rotate", false, "add a new key beside the existing ones")
	return cmd
}

func newSendCommand(getenv func(string) string) *cobra.Command {
	var from, subject, message string
	cmd := &cobra.Command{
		Use:   "send <city>/<address>",
		Short: "Seal a mail to a peer city and post it to the relay",
		Long: `Seal a mail to a peer city's newest key and post it to the relay. The peer's
key statement must verify under the peer's pinned signing key and must not be
older than the newest one seen. Prints the mail's id.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := resolveSettings(cmd, getenv)
			if err != nil {
				return err
			}
			if from == "" {
				from = defaultSender(getenv)
			}
			id, err := agent.Send(cmd.Context(), s, agent.Deps{}, agent.Outgoing{To: args[0], From: from, Subject: subject, Body: message})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), id)
			return err
		},
	}
	addSettingFlags(cmd)
	cmd.Flags().StringVar(&from, "from", "", `sender address in this city (default: $GC_ALIAS, $GC_AGENT, $GC_SESSION_ID, or "human")`)
	cmd.Flags().StringVarP(&subject, "subject", "s", "", "subject line (required)")
	cmd.Flags().StringVarP(&message, "message", "m", "", "body text")
	return cmd
}

// defaultSender is the sender gc itself names for a mail to another city.
func defaultSender(getenv func(string) string) string {
	for _, name := range []string{"GC_ALIAS", "GC_AGENT", "GC_SESSION_ID"} {
		if v := getenv(name); v != "" {
			return v
		}
	}
	return "human"
}

func newRunCommand(getenv func(string) string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Receive this city's mail from the relay until stopped",
		Long: `Publish this city's newest key statement, then keep one signed read open at
the relay. Each mail is opened and checked, filed in the city's inbox through
the city's own API before the agent moves its place, and the recipient is
told through gc. With a socket path the agent answers gc's health probe on it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := resolveSettings(cmd, getenv)
			if err != nil {
				return err
			}
			return agent.Run(cmd.Context(), s, agent.Deps{Log: cmd.ErrOrStderr()})
		},
	}
	addSettingFlags(cmd)
	return cmd
}
