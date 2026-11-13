package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/muthuishere/tmvp/internal/skill"
)

func init() {
	addCommand(func() *cobra.Command {
		c := &cobra.Command{Use: "new", Short: "Scaffold things (new skill <product>)"}
		var path, envs, systems, from string
		var noLink bool
		sk := &cobra.Command{
			Use:   "skill <product>",
			Short: "Scaffold <path>/<product>-tmvp: SKILL.md, config.yaml with $(…) secret slots, assets/<system>/",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				specs, err := skill.ParseSystems(systems)
				if err != nil {
					return err
				}
				dir, err := skill.New(skill.Options{Product: args[0], Path: path, Envs: skill.SplitList(envs), Systems: specs, From: from})
				if err != nil {
					return err
				}
				fmt.Fprintln(stdout, dir)
				if !noLink {
					link, err := skill.LinkClaude(dir)
					switch {
					case err != nil:
						fmt.Fprintf(stderr, "warning: not linked for Claude Code: %v\n", err)
					case link != "":
						fmt.Fprintf(stdout, "linked %s -> %s (Claude Code discovery)\n", link, dir)
					}
				}
				return nil
			},
		}
		sk.Flags().StringVar(&path, "path", ".agents/skills", "parent directory of the skill")
		sk.Flags().StringVar(&envs, "envs", "dev,staging,prod", "environments, comma-separated")
		sk.Flags().StringVar(&systems, "systems", "", "systems as name:type, e.g. db:postgres,cluster:k8s,admin:web")
		sk.Flags().StringVar(&from, "from", "", "adopt an existing config.yaml (e.g. ~/.tmvp/<product>/config.yaml) instead of generating one; --envs/--systems are ignored")
		sk.Flags().BoolVar(&noLink, "no-link", false, "do not add the .claude/skills/<product>-tmvp symlink")
		c.AddCommand(sk)
		return c
	})
}
