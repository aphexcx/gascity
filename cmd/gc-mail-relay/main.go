// Command gc-mail-relay serves sealed mail for authenticated cities.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/mailrelay/relay"
	"github.com/spf13/cobra"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newCommand().ExecuteContext(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newCommand() *cobra.Command {
	root := &cobra.Command{Use: "gc-mail-relay", Short: "Serve sealed intercity mail", SilenceErrors: true, SilenceUsage: true}
	root.CompletionOptions.DisableDefaultCmd = true
	cfg := relay.DefaultConfig()
	var addr string
	serve := &cobra.Command{
		Use: "serve", Short: "Listen for signed relay requests", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			for _, name := range []string{"listen", "store", "keys-dir", "hub", "max-wait", "posts-per-minute", "reload-interval", "registry-url", "registry-allow-empty"} {
				env := "GC_MAIL_RELAY_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
				if value, ok := os.LookupEnv(env); ok && !cmd.Flags().Changed(name) {
					if err := cmd.Flags().Set(name, value); err != nil {
						return fmt.Errorf("invalid setting %s: %w", env, err)
					}
				}
			}
			return run(cmd, addr, cfg)
		},
	}
	flags := serve.Flags()
	flags.StringVar(&addr, "listen", "127.0.0.1:8080", "HTTP listen address")
	flags.StringVar(&cfg.StorePath, "store", "", "SQLite store file")
	flags.StringVar(&cfg.KeysDir, "keys-dir", "", "directory of trusted city signing keys")
	flags.StringVar(&cfg.Hub, "hub", "", "required hub city ID")
	flags.DurationVar(&cfg.MaxWait, "max-wait", cfg.MaxWait, "longest queue wait (at most 300s)")
	flags.IntVar(&cfg.PostsPerMinute, "posts-per-minute", cfg.PostsPerMinute, "new posts allowed per city per minute")
	flags.DurationVar(&cfg.ReloadInterval, "reload-interval", cfg.ReloadInterval, "interval for re-reading pinned keys and copying the registry")
	flags.StringVar(&cfg.RegistryURL, "registry-url", "", "https address of the key registry's list of city signing keys (empty: pinned keys only)")
	flags.BoolVar(&cfg.RegistryAllowEmpty, "registry-allow-empty", false, "let an empty registry list remove every registry key")
	root.AddCommand(serve)
	return root
}

func run(cmd *cobra.Command, addr string, cfg relay.Config) (err error) {
	s, err := relay.Open(cfg, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "relay listening on %s\n", listener.Addr()); err != nil {
		return errors.Join(err, listener.Close())
	}
	httpServer := &http.Server{
		Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: cfg.MaxWait + 30*time.Second, IdleTimeout: time.Minute,
		MaxHeaderBytes: relay.MaxHeaderBytes, BaseContext: func(net.Listener) context.Context { return ctx },
	}
	maintained := make(chan struct{})
	go func() { defer close(maintained); s.Maintain(ctx) }()
	stopped := make(chan error, 1)
	go func() {
		<-ctx.Done()
		deadline, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := httpServer.Shutdown(deadline); err != nil {
			stopped <- errors.Join(err, httpServer.Close())
			return
		}
		stopped <- nil
	}()
	err = httpServer.Serve(listener)
	cancel()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	<-maintained
	return errors.Join(err, <-stopped)
}
