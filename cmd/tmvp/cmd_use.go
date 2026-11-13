package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/muthuishere/tmvp/internal/envsel"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
)

// `tmvp use [env] [--clear]`: the active env for this product (per config
// path), persisted 0600 under TMVP_HOME/state. Any env may be selected;
// writes to a protected env still need --force (see cmd_write.go).
func init() {
	addCommand(func() *cobra.Command {
		var clear bool
		c := &cobra.Command{
			Use:   "use [env | default] [--clear]",
			Short: "Show or set the active env for this product (TMVP_ENV and -e override it; `default` or --clear resets to default_env)",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				if len(args) == 1 && args[0] == "default" && cfg.Envs["default"] == nil {
					clear, args = true, nil // `tmvp use default` = --clear (unless an env is really named default)
				}
				switch {
				case clear && len(args) > 0:
					return errs.New(errs.Usage, "use either an env or --clear")
				case clear:
					if err := envsel.Clear(cfg); err != nil {
						return err
					}
				case len(args) == 1:
					if _, err := envsel.Use(cfg, args[0]); err != nil {
						return err
					}
				}
				sel := envsel.Resolve(cfg, globals.Env)
				row := map[string]any{"product": cfg.Product, "env": sel.Env, "source": sel.Source, "where": sel.Where,
					"default_env": cfg.DefaultEnv, "config": cfg.Path}
				if sel.Env != "" {
					row["protected"] = cfg.IsProtected(sel.Env)
				}
				if globals.JSON {
					printFrame(protocol.Frame{Type: protocol.FrameRow, Row: row})
					return nil
				}
				if sel.Env == "" {
					fmt.Fprintf(stdout, "no envs in %s config\n", cfg.Product)
					return nil
				}
				prot := ""
				if cfg.IsProtected(sel.Env) {
					prot = " [protected: writes need --force]"
				}
				fmt.Fprintf(stdout, "%s: env %s%s (source: %s — %s)\n", cfg.Product, sel.Env, prot, sel.Source, sel.Where)
				return nil
			},
		}
		c.Flags().BoolVar(&clear, "clear", false, "forget the `tmvp use` choice (back to default_env)")
		return c
	})
}
