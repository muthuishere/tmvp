package main

import (
	"encoding/json"
	"fmt"
	"github.com/muthuishere/tmvp"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/muthuishere/tmvp/internal/config"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/server"
	"github.com/muthuishere/tmvp/internal/skill"
)

func init() {
	addCommand(func() *cobra.Command {
		c := &cobra.Command{Use: "skill", Short: "Product skill: add <name> <type>, sync (SKILL.md from config), lint; install (shipped skills like tmvp-discoverer)"}
		var envs string
		add := &cobra.Command{
			Use:   "add <name> <type>",
			Short: "Add a system to config.yaml (comments and order kept) and create assets/<name>/",
			Args:  cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				p, err := config.Find(config.FindOptions{ConfigPath: globals.Config, SkillPath: globals.Skill})
				if err != nil {
					return err
				}
				touched, err := skill.AddSystem(p, args[0], args[1], skill.SplitList(envs))
				if err != nil {
					return err
				}
				fmt.Fprintf(stdout, "added %s (%s) to %s in %s\n", args[0], args[1], strings.Join(touched, ", "), p)
				return nil
			},
		}
		add.Flags().StringVar(&envs, "env", "", "only these envs (comma-separated; default all)")
		c.AddCommand(add)
		var discover bool
		install := &cobra.Command{
			Use:   "install --discover",
			Short: "Install the tmvp-discoverer skill (opt-in) into ~/.claude/skills, ~/.agents/skills and $CLAUDE_CONFIG_DIR/skills; then ask your agent: /tmvp-discoverer. Later tmvp runs keep installed copies current",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				if !discover {
					return errs.New(errs.Usage, "nothing selected: `tmvp skill install --discover` installs the tmvp-discoverer skill (run it with /tmvp-discoverer in your agent)")
				}
				names := []string{"tmvp-discoverer"}
				for _, root := range skill.InstallRoots() {
					for _, n := range names {
						changed, err := skill.Install(tmvp.Skills, n, root)
						if err != nil {
							return err
						}
						state := "up to date"
						if changed {
							state = "installed"
						}
						fmt.Fprintf(stdout, "%s  %s\n", filepath.Join(root, n), state)
					}
				}
				fmt.Fprintln(stdout, "next: open your agent in the product repo and run /tmvp-discoverer")
				return nil
			},
		}
		install.Flags().BoolVar(&discover, "discover", false, "install tmvp-discoverer (scans a repo, asks you, writes <project>-tmvp skills)")
		c.AddCommand(install)
		c.AddCommand(&cobra.Command{
			Use:   "sync",
			Short: "Regenerate SKILL.md from config.yaml (deterministic; hand-written SKILL.md is left alone)",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				p, err := config.Find(config.FindOptions{ConfigPath: globals.Config, SkillPath: globals.Skill})
				if err != nil {
					return err
				}
				if err := skill.Sync(p); err != nil {
					return err
				}
				fmt.Fprintln(stdout, filepath.Join(filepath.Dir(p), "SKILL.md"))
				return nil
			},
		})
		c.AddCommand(&cobra.Command{
			Use:   "lint [dir]",
			Short: "Check skill structure: SKILL.md front-matter, config validates, no secret values, saved items have front-matter, asset paths exist",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				dir := globals.Skill
				if len(args) == 1 {
					dir = args[0]
				}
				if dir == "" {
					p, err := config.Find(config.FindOptions{ConfigPath: globals.Config})
					if err != nil {
						return err
					}
					dir = filepath.Dir(p)
				}
				issues := skill.Lint(dir, skill.LintOptions{Validate: server.ValidateOptions()})
				bad := 0
				for _, is := range issues {
					if is.Level == "error" {
						bad++
					}
					if globals.JSON {
						b, _ := json.Marshal(is)
						fmt.Fprintln(stdout, string(b))
					} else {
						fmt.Fprintf(stdout, "%s: %s: %s\n", is.Level, is.Path, is.Msg)
					}
				}
				if bad > 0 {
					return errs.New(errs.Config, "skill lint: %d error(s) in %s", bad, dir)
				}
				if !globals.JSON {
					fmt.Fprintf(stdout, "ok: %s\n", dir)
				}
				return nil
			},
		})
		return c
	})
}
