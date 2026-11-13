package main

import (
	"github.com/spf13/cobra"

	"github.com/muthuishere/tmvp/internal/config"
	"github.com/muthuishere/tmvp/internal/protocol"
)

func init() {
	addCommand(func() *cobra.Command {
		return &cobra.Command{
			Use:   "check [envsel] [system]",
			Short: "Validate config, resolve secrets (names only) and connect to every selected system",
			Args:  cobra.MaximumNArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				req := baseRequest(protocol.OpCheck, cfg)
				if len(args) > 0 {
					s := config.ParseSelector(args[0])
					req.Env, req.System = s.Env, s.System
					if len(args) > 1 {
						req.System = args[1]
					}
					if req.System == "" && cfg.HasSystem(args[0]) && !cfg.IsEnvSelector(args[0]) {
						req.Env, req.System = "", args[0]
					}
				}
				return send(cmd.Context(), req)
			},
		}
	})
}
