package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/muthuishere/tmvp/internal/client"
	"github.com/muthuishere/tmvp/internal/protocol"
	"github.com/muthuishere/tmvp/internal/server"
)

func init() {
	addCommand(func() *cobra.Command {
		c := &cobra.Command{Use: "server", Short: "The local tmvp server (auto-started by every call)"}
		run := &cobra.Command{
			Use: "run", Short: "Run the server in the foreground (what auto-start launches)", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				err := server.Run(cmd.Context(), server.NewEngine(), server.Options{})
				if errors.Is(err, server.ErrAlreadyRunning) {
					fmt.Fprintln(stderr, err)
					return nil
				}
				return err
			},
		}
		start := &cobra.Command{
			Use: "start", Short: "Start the server detached (normally automatic)", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				conn, err := client.Dial(cmd.Context(), true)
				if err != nil {
					return err
				}
				conn.Close()
				fmt.Fprintln(stdout, "running: "+protocol.SocketPath())
				return nil
			},
		}
		status := &cobra.Command{
			Use: "status", Short: "Loaded products, connections, tunnels, secret slots (names only)", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				if !client.InProcess() {
					if c, err := client.Dial(cmd.Context(), false); err != nil {
						fmt.Fprintln(stdout, "not running")
						return nil
					} else {
						c.Close()
					}
				}
				return send(cmd.Context(), protocol.Request{Op: protocol.OpStatus})
			},
		}
		stop := &cobra.Command{
			Use: "stop", Short: "Stop the server (drops all secrets and connections)", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				if !client.InProcess() {
					if c, err := client.Dial(cmd.Context(), false); err != nil {
						fmt.Fprintln(stdout, "not running")
						return nil
					} else {
						c.Close()
					}
				}
				return send(cmd.Context(), protocol.Request{Op: protocol.OpStop})
			},
		}
		c.AddCommand(run, start, status, stop)
		return c
	})
}
