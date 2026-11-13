package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/muthuishere/tmvp/internal/errs"
)

func init() {
	addCommand(func() *cobra.Command {
		c := &cobra.Command{Use: "config", Short: "Inspect the config"}
		var resolved bool
		show := &cobra.Command{
			Use:   "show [--resolved]",
			Short: "Print the config (vars/env expanded, every secret masked)",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				out, err := cfg.ShowResolved()
				if err != nil {
					return errs.Wrap(errs.Config, err, "render")
				}
				fmt.Fprintf(stdout, "# %s\n%s", cfg.Path, out)
				return nil
			},
		}
		show.Flags().BoolVar(&resolved, "resolved", true, "expand vars and env (secrets always masked)")
		path := &cobra.Command{
			Use: "path", Short: "Print which config file the lookup order picks", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				fmt.Fprintln(stdout, cfg.Path)
				return nil
			},
		}
		c.AddCommand(show, path)
		return c
	})
}
