package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/muthuishere/tmvp/internal/config"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/login"
	"github.com/muthuishere/tmvp/internal/protocol"
)

// `tmvp envs` and `tmvp systems`: what is available, from config alone (no
// server, no secret command). Never prints a field value that could be a secret.

func init() {
	addCommand(func() *cobra.Command {
		return &cobra.Command{
			Use:   "envs",
			Short: "List envs: destructive flag and system count (config only; no secrets, no server)",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				for _, en := range cfg.EnvOrder {
					e := cfg.Envs[en]
					row := map[string]any{"env": en, "type": e.Type, "destructive": e.Destructive, "systems": len(e.Systems),
						"names": strings.Join(e.SystemOrder, ",")}
					if globals.JSON {
						printFrame(protocol.Frame{Type: protocol.FrameRow, Row: row})
						continue
					}
					d := ""
					if e.Destructive {
						d = "destructive"
					}
					fmt.Fprintf(stdout, "%-12s %-8s %-12s %2d systems  %s\n", en, e.Type, d, len(e.Systems), strings.Join(e.SystemOrder, ", "))
				}
				return nil
			},
		}
	})
	addCommand(func() *cobra.Command {
		var check bool
		c := &cobra.Command{
			Use:   "systems [env]",
			Short: "List systems per env: name, type, labels, via, login (config only; --check runs the login checks)",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				envs := cfg.EnvOrder
				if len(args) == 1 {
					if cfg.Envs[args[0]] == nil {
						return errs.New(errs.NotFound, "no env %q", args[0]).WithHint("tmvp envs")
					}
					envs = []string{args[0]}
				}
				lc := login.New()
				for _, en := range envs {
					e := cfg.Envs[en]
					for _, sn := range e.SystemOrder {
						s := e.Systems[sn]
						row := map[string]any{"env": en, "system": sn, "type": s.Type, "labels": labels(s.Labels),
							"via": strings.Join(s.Via, ">"), "login": loginState(cmd.Context(), lc, cfg, s, check),
							"destructive": cfg.IsDestructive(en, sn)}
						if globals.JSON {
							printFrame(protocol.Frame{Type: protocol.FrameRow, Row: row})
							continue
						}
						fmt.Fprintf(stdout, "%-10s %-14s %-12s %-24s %-14s %s\n", en, sn, s.Type, row["labels"], viaText(s.Via), row["login"])
					}
				}
				return nil
			},
		}
		c.Flags().BoolVar(&check, "check", false, "run each system's login.check now (ok | required)")
		return c
	})
}

func labels(m map[string]string) string {
	ks := make([]string, 0, len(m))
	for k, v := range m {
		ks = append(ks, k+"="+v)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

func viaText(v []string) string {
	if len(v) == 0 {
		return "-"
	}
	return "via " + strings.Join(v, ">")
}

// loginState: "none", "manual: <cmd>", "browser" — or with check, "ok" /
// "required: <cmd>". The login command is config text the USER runs, not a secret.
func loginState(ctx context.Context, lc *login.Checker, cfg *config.Config, s *config.System, check bool) string {
	if s.Login == nil {
		return "none"
	}
	st := "manual: " + s.Login.Manual
	if s.Login.Manual == "browser" {
		st = "browser (tmvp login " + s.Env + " " + s.Name + ")"
	}
	if !check || s.Login.Check == "" {
		return st
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err := lc.Ensure(cctx, cfg.Product+"/"+s.Env+"/"+s.Name, &login.Spec{Manual: s.Login.Manual, Check: s.Login.Check}, cfg.Dir, s.Env, s.Name)
	if err != nil {
		return "required: " + s.Login.Manual
	}
	return "ok"
}
