package main

import (
	"github.com/spf13/cobra"

	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
)

func init() {
	addCommand(func() *cobra.Command {
		var preview bool
		c := &cobra.Command{
			Use:   "write [env] <system> <verb|statement> [args] --reason \"...\" [--preview] [--force]",
			Short: "Run one mutation on one env/system (audited; --reason required; protected envs also need --force)",
			Args:  cobra.MinimumNArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				if globals.Reason == "" {
					return errs.New(errs.PolicyBlocked, "tmvp write requires --reason").WithHint(`--reason "why"`)
				}
				// `write <env> <system> …` or `write <system> …` (env from --env / TMVP_ENV / use / default_env)
				pos, given, rest := "", false, args
				if _, isEnv := cfg.Envs[args[0]]; (isEnv || cfg.IsEnvSelector(args[0])) && !cfg.HasSystem(args[0]) {
					pos, given, rest = args[0], true, args[1:]
				}
				if len(rest) < 2 {
					return errs.New(errs.Usage, "write needs a system and a verb or statement").WithHint(`tmvp write [env] <system> <verb|stmt> --reason "why"`)
				}
				sel, err := selectEnv(cfg, pos, given)
				if err != nil {
					return err
				}
				if sel.Env == "" {
					return errs.New(errs.Usage, "no env selected for the write").
						WithHint(`tmvp write <env> ` + rest[0] + ` … (or -e <env>, TMVP_ENV, tmvp use <env>, default_env)`)
				}
				req := baseRequest(protocol.OpWrite, cfg)
				req.Env, req.System, req.Verb, req.Args, req.Preview = sel.Env, rest[0], rest[1], rest[2:], preview
				req.EnvSource = sel.Source
				req.Diff = false
				printEnvHeader(cfg, req.Env, sel)
				return send(cmd.Context(), req)
			},
		}
		c.Flags().BoolVar(&preview, "preview", false, "show the blast radius without changing anything")
		return c
	})
}
