package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/server"
)

func init() {
	addCommand(func() *cobra.Command {
		return &cobra.Command{
			Use:   "validate",
			Short: "Validate the config: schema, refs, name collisions, via cycles, literal secrets (runs no secret command)",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				issues := cfg.Validate(server.ValidateOptions())
				bad := 0
				for _, is := range issues {
					if is.Level == "error" {
						bad++
					}
					if globals.JSON {
						b, _ := json.Marshal(is)
						fmt.Fprintln(stdout, string(b))
					} else {
						fmt.Fprintln(stdout, is.String())
					}
				}
				if bad > 0 {
					return errs.New(errs.Config, "%d error(s) in %s", bad, cfg.Path)
				}
				if !globals.JSON {
					fmt.Fprintf(stdout, "ok: %s (product %s, %d envs)\n", cfg.Path, cfg.Product, len(cfg.Envs))
				}
				return nil
			},
		}
	})
}
