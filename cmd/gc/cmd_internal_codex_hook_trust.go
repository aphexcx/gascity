package main

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/spf13/cobra"
)

func newInternalCodexHookTrustCmd(stdout, stderr io.Writer) *cobra.Command {
	var launcher, cityDir, workDir string
	cmd := &cobra.Command{
		Use: "trust-codex-hooks", Hidden: true,
		Short: "Record trust for bundled Codex hooks before startup",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) (result error) {
			defer func() {
				if result != nil {
					fmt.Fprintf(stderr, "gc internal trust-codex-hooks: %v\n", result) //nolint:errcheck
					result = errExit
				}
			}()
			if launcher == "" || !filepath.IsAbs(cityDir) || !filepath.IsAbs(workDir) {
				return fmt.Errorf("codex hook trust requires --launcher and absolute --city-dir/--workdir")
			}
			count, err := hooks.TrustCodexManagedHooks(cmd.Context(), launcher, cityDir, workDir)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "codex-hook-trust: appended %d managed hook trust record(s) for %s\n", count, workDir)
			return err
		},
	}
	cmd.Flags().StringVar(&launcher, "launcher", "", "configured Codex launcher, including profile arguments")
	cmd.Flags().StringVar(&cityDir, "city-dir", "", "city owning the generated hooks")
	cmd.Flags().StringVar(&workDir, "workdir", "", "session working directory")
	return cmd
}

func appendCodexHookTrustPreStart(commands []string, launcher, cityDir, workDir string) []string {
	return append(commands, `"${GC_BIN:-gc}" internal trust-codex-hooks `+
		shellquote.Join([]string{"--launcher", launcher, "--city-dir", cityDir, "--workdir", workDir}))
}
