package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/skill"
)

func init() {
	addCommand(func() *cobra.Command {
		return &cobra.Command{
			Use:   "export pg-service|kubeconfig [env]",
			Short: "Print pg_service.conf / kubeconfig snippets for plain psql/kubectl (never secret values)",
			Args:  cobra.RangeArgs(1, 2),
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				env := ""
				if len(args) == 2 {
					env = args[1]
				}
				var out string
				switch args[0] {
				case "pg-service":
					out, err = skill.PgService(cfg, env)
				case "kubeconfig":
					out, err = skill.Kubeconfig(cfg, env)
				default:
					return errs.New(errs.Usage, "export: want pg-service or kubeconfig, got %q", args[0])
				}
				if err != nil {
					return err
				}
				fmt.Fprint(stdout, out)
				return nil
			},
		}
	})
}
