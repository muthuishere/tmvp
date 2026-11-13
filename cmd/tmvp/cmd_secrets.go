package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/muthuishere/tmvp/internal/audit"
	"github.com/muthuishere/tmvp/internal/client"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
)

func init() {
	addCommand(func() *cobra.Command {
		c := &cobra.Command{Use: "secrets", Short: "Manage and check the server's in-memory secrets for this product"}
		mk := func(use, short, op string) *cobra.Command {
			return &cobra.Command{Use: use, Short: short, Args: cobra.NoArgs,
				RunE: func(cmd *cobra.Command, _ []string) error {
					cfg, err := loadConfig()
					if err != nil {
						return err
					}
					return send(cmd.Context(), baseRequest(op, cfg))
				}}
		}
		var days int
		status := &cobra.Command{
			Use:   "status [--env E] [--days 30]",
			Short: "Per secret: ok|missing|invalid|login_required, source tool, last fetch, value changed, expiry, cert days left (never values)",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				req := baseRequest(protocol.OpSecretsStatus, cfg)
				req.Env = globals.Env
				if req.Flags == nil {
					req.Flags = map[string]string{}
				}
				req.Flags["days"] = strconv.Itoa(days)
				return sendSecretsStatus(cmd.Context(), req)
			},
		}
		status.Flags().IntVar(&days, "days", 30, "warn when a held certificate expires within this many days")
		auditCmd := &cobra.Command{
			Use:   "audit [--since 1d]",
			Short: "Secret fetches/refreshes/invalidations/failures from the audit log: name, time, triggering command (no values)",
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
				recs, err := audit.Read(cfg.LogDir(), audit.ReadOptions{Since: since})
				if err != nil {
					return errs.Wrap(errs.Runtime, err, "read logs")
				}
				var out []audit.Record
				for _, r := range recs {
					if r.Body == "secret" {
						out = append(out, r)
					}
				}
				if globals.Limit > 0 && len(out) > globals.Limit {
					out = out[len(out)-globals.Limit:]
				}
				for _, r := range out {
					a := r.Attributes
					if globals.JSON {
						row := map[string]any{"time": r.Time}
						for _, k := range []string{"secret", "event", "trigger", "updated", "outcome", "code", "error", "expires_at"} {
							if v, ok := a[k]; ok && v != "" {
								row[k] = v
							}
						}
						b, _ := json.Marshal(row)
						fmt.Fprintln(stdout, string(b))
						continue
					}
					extra := ""
					if a["outcome"] == "failed" {
						extra = fmt.Sprintf("  %v: %v", a["code"], a["error"])
					}
					fmt.Fprintf(stdout, "%s  %-10v %-28v updated=%-7v by %q%s\n", r.Time, a["event"], a["secret"], a["updated"], a["trigger"], extra)
				}
				if len(out) == 0 && !globals.JSON {
					fmt.Fprintln(stderr, "no secret events in the audit log"+map[bool]string{true: " for --since " + globals.Since, false: ""}[globals.Since != ""])
				}
				return nil
			},
		}
		c.AddCommand(
			mk("reload", "Re-run every secret command (after rotation); prints names + ok/failed", protocol.OpSecretsReload),
			mk("clear", "Drop this product's secrets and connections from server memory", protocol.OpSecretsClear),
			status, auditCmd,
		)
		return c
	})
}

// sendSecretsStatus prints the status rows as a table (text) or JSONL.
func sendSecretsStatus(ctx context.Context, req protocol.Request) error {
	if globals.JSON {
		return send(ctx, req)
	}
	header := false
	code, err := client.Do(ctx, req, func(f protocol.Frame) {
		if f.Type != protocol.FrameRow {
			printFrame(f)
			return
		}
		if !header {
			header = true
			fmt.Fprintf(stdout, "%-30s %-15s %-10s %-20s %-8s %-20s %s\n", "SECRET", "STATE", "SOURCE", "FETCHED", "UPDATED", "EXPIRES", "CERT / KEY")
		}
		r := f.Row
		s := func(k string) string {
			if v, ok := r[k]; ok && v != nil {
				return fmt.Sprint(v)
			}
			return "-"
		}
		cert := "-"
		if _, ok := r["cert_not_after"]; ok {
			cert = fmt.Sprintf("%s: %s days left (until %s, %s certs)", s("cert_status"), s("cert_days_left"), s("cert_not_after"), s("cert_count"))
		}
		if pk, ok := r["private_key"].(bool); ok {
			cert += fmt.Sprintf(", private key: %v", map[bool]string{true: "present", false: "none"}[pk])
		}
		state := s("state")
		if _, ok := r["code"]; ok {
			state += " (" + s("code") + ")"
		}
		fmt.Fprintf(stdout, "%-30s %-15s %-10s %-20s %-8s %-20s %s\n", s("secret"), state, s("source"), s("fetched_at"), s("updated"), s("expires_at"), cert)
		if _, ok := r["reason"]; ok {
			fmt.Fprintf(stdout, "  └ %s\n", s("reason"))
		}
	})
	if err != nil {
		printError(errs.As(err))
		if code == 0 {
			code = 1
		}
	}
	return exitCode(code)
}
