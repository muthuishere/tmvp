package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/muthuishere/tmvp/internal/audit"
	"github.com/muthuishere/tmvp/internal/errs"
)

func init() {
	addCommand(func() *cobra.Command {
		var writes bool
		c := &cobra.Command{
			Use:   "logs [--since 1d] [--writes]",
			Short: "Read back the product's event log / write audit",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				since, err := parseSince(globals.Since)
				if err != nil {
					return err
				}
				recs, err := audit.Read(cfg.LogDir(), audit.ReadOptions{Since: since, WritesOnly: writes, Limit: globals.Limit})
				if err != nil {
					return errs.Wrap(errs.Runtime, err, "read logs")
				}
				for _, r := range recs {
					if globals.JSON {
						b, _ := json.Marshal(r)
						fmt.Fprintln(stdout, string(b))
						continue
					}
					a := r.Attributes
					fmt.Fprintf(stdout, "%s %-13s %v.%v %v %v outcome=%v reason=%q\n", r.Time, r.Body,
						a["env"], a["system"], a["verb"], a["args"], a["outcome"], a["reason"])
				}
				return nil
			},
		}
		c.Flags().BoolVar(&writes, "writes", false, "only write intent/outcome records")
		return c
	})
}

// parseSince accepts Go durations plus a "d" (days) suffix.
func parseSince(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		var n int
		if _, err := fmt.Sscanf(s, "%dd", &n); err == nil {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, errs.New(errs.Usage, "bad --since %q (e.g. 30m, 6h, 1d)", s)
	}
	return d, nil
}
