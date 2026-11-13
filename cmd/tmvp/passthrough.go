package main

import (
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// passUnknownFlags rewrites argv for reads (the root command:
// `tmvp prod box docker logs api --tail 50`) and `tmvp write` so flags tmvp does not know
// reach the verb as args instead of failing as "unknown flag". tmvp's own
// flags (with their values) stay where cobra parses them; every other token
// keeps its order after a "--". An explicit "--" from the user is left alone.
func passUnknownFlags(root *cobra.Command, args []string) []string {
	cmd, _, err := root.Find(args)
	if err != nil || (cmd != root && cmd.Name() != "write") {
		return args
	}
	for _, a := range args {
		if a == "--" {
			return args
		}
	}
	fs := cmd.Flags()
	fs.AddFlagSet(cmd.InheritedFlags())
	known := func(tok string) (f *pflag.Flag, inline bool) {
		if strings.HasPrefix(tok, "--") {
			name, _, eq := strings.Cut(tok[2:], "=")
			return fs.Lookup(name), eq
		}
		if len(tok) == 2 {
			return fs.ShorthandLookup(tok[1:]), false
		}
		// -Fk=v / -lapp=x: known shorthand with its value attached
		if f := fs.ShorthandLookup(tok[1:2]); f != nil && f.Value.Type() != "bool" {
			return f, true
		}
		return nil, false
	}
	var own, rest []string
	sawCmd, moved := cmd == root, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !sawCmd && a == cmd.Name() {
			sawCmd = true
			own = append(own, a)
			continue
		}
		if len(a) < 2 || a[0] != '-' || !sawCmd {
			if sawCmd {
				rest = append(rest, a)
			} else {
				own = append(own, a)
			}
			continue
		}
		f, inline := known(a)
		if f == nil {
			rest = append(rest, a)
			moved = true
			continue
		}
		own = append(own, a)
		if !inline && f.Value.Type() != "bool" && i+1 < len(args) {
			i++
			own = append(own, args[i])
		}
	}
	if !moved {
		return args
	}
	return append(append(own, "--"), rest...)
}
