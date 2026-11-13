package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/muthuishere/tmvp/internal/protocol"
	"github.com/muthuishere/tmvp/internal/scan"
)

// `tmvp env-keys [dir]`: key NAMES from every .env* / *.env file under dir,
// for discovery. Values never leave the process; only "set" or "empty".
func init() {
	addCommand(func() *cobra.Command {
		return &cobra.Command{
			Use:   "env-keys [dir]",
			Short: "List variable NAMES in every .env / .env.<env> / <env>.env under dir (default .), grouped by env; values are never printed",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				dir := "."
				if len(args) == 1 {
					dir = args[0]
				}
				ks, err := scan.EnvKeys(dir)
				if err != nil {
					return err
				}
				if globals.JSON {
					for _, k := range ks {
						printFrame(protocol.Frame{Type: protocol.FrameRow, Row: map[string]any{"file": k.File, "env": k.Env, "key": k.Name, "set": k.Set}})
					}
					return nil
				}
				cur, head := "", ""
				var line []string
				flush := func() {
					if cur != "" {
						fmt.Fprintf(stdout, "%s\n  %s\n", head, strings.Join(line, " "))
					}
				}
				for _, k := range ks {
					if k.File != cur {
						flush()
						cur, head, line = k.File, k.File, nil
						if k.Env != "" {
							head += "  (env: " + k.Env + ")"
						}
					}
					n := k.Name
					if !k.Set {
						n += "(empty)"
					}
					line = append(line, n)
				}
				flush()
				if len(ks) == 0 {
					fmt.Fprintln(stdout, "no env files found")
				}
				return nil
			},
		}
	})
}
