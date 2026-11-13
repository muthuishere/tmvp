package main

import (
	"github.com/spf13/cobra"

	"github.com/muthuishere/tmvp/internal/protocol"
)

func init() {
	addCommand(func() *cobra.Command {
		var user string
		c := &cobra.Command{
			Use:   "login <env> <system> [--user name]",
			Short: "Show login status; for web systems with manual: browser, open the headed login",
			Args:  cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				req := baseRequest(protocol.OpLogin, cfg)
				req.Env, req.System = args[0], args[1]
				if user != "" {
					if req.Flags == nil {
						req.Flags = map[string]string{}
					}
					req.Flags["user"] = user
				}
				return send(cmd.Context(), req)
			},
		}
		c.Flags().StringVar(&user, "user", "", "named user from the system's users:")
		return c
	})
}
