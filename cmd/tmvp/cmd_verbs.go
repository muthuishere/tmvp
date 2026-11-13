package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
)

func init() {
	addCommand(func() *cobra.Command {
		return &cobra.Command{
			Use:   "verbs [type...]",
			Short: "List the verbs of each connector type (mode: read | write | destructive | read-write)",
			Long: `List connector verbs so an agent can discover them at runtime.

  read        plain tmvp <env> <system> <verb>
  write       only tmvp write <env> <system> <verb> --reason "..."
  destructive write that also needs destructive: true
  read-write  runs both ways; mutates only through tmvp write
  default     the write verb used when tmvp write gets a non-verb (e.g. raw SQL)
  @item       the verb a bare "tmvp <env> <system> @name" runs`,
			RunE: func(cmd *cobra.Command, args []string) error {
				types := args
				if len(types) == 0 {
					types = connector.Types()
				}
				for _, typ := range types {
					if _, ok := connector.Lookup(typ); !ok {
						return errs.New(errs.NotFound, "no connector type %q", typ).WithHint("types: " + strings.Join(connector.Types(), ", "))
					}
				}
				for _, typ := range types {
					for _, v := range connector.VerbsOf(typ) {
						if globals.JSON {
							printFrame(protocol.Frame{Type: protocol.FrameRow, Row: map[string]any{"type": typ, "verb": v.Name,
								"mode": v.Mode.String(), "default": v.Default, "saved": v.Saved, "help": v.Help}})
							continue
						}
						var tags []string
						if v.Default {
							tags = append(tags, "default")
						}
						if v.Saved {
							tags = append(tags, "@item")
						}
						mode := v.Mode.String()
						if len(tags) > 0 {
							mode += " [" + strings.Join(tags, ",") + "]"
						}
						fmt.Fprintf(stdout, "%-12s %-14s %-22s %s\n", typ, v.Name, mode, v.Help)
					}
				}
				return nil
			},
		}
	})
}
