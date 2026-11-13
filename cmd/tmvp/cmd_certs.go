package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/muthuishere/tmvp/internal/protocol"
	"github.com/muthuishere/tmvp/internal/server"
)

// `tmvp certs`: TLS certificate expiry of every system with a TLS endpoint
// (handshake only, via-aware, short timeouts) plus certs held in secrets.
// Runs in-process like doctor; never prints key material.
func init() {
	addCommand(func() *cobra.Command {
		var days int
		c := &cobra.Command{
			Use:   "certs [--env E] [--days 30]",
			Short: "TLS cert expiry of every system's endpoint + certs held in secrets (warn < --days, fail if expired)",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				timeout := 5 * time.Second
				if d, err := time.ParseDuration(globals.Timeout); err == nil && d > 0 {
					timeout = d
				}
				reps, err := server.NewEngine().Certs(cmd.Context(), cfg.Path, server.CertsOptions{Env: globals.Env, WarnDays: days, Timeout: timeout, Secrets: true})
				if err != nil {
					return err
				}
				return exitCode(printCerts(reps, days))
			},
		}
		c.Flags().IntVar(&days, "days", 30, "warn when a certificate expires within this many days")
		return c
	})
}

func printCerts(reps []server.CertReport, days int) int {
	server.SortCerts(reps)
	exit := 0
	if !globals.JSON {
		fmt.Fprintf(stdout, "%-9s %-26s %-28s %-9s %-6s %-21s %s\n", "STATUS", "WHERE", "ENDPOINT/SECRET", "SOURCE", "DAYS", "NOT AFTER", "ISSUER / TRUST")
	}
	for _, r := range reps {
		if r.Status == "expired" || r.Status == "error" {
			exit = 1
		}
		if globals.JSON {
			printFrame(protocol.Frame{Type: protocol.FrameRow, Env: r.Env, System: r.System, Row: r.Row()})
			continue
		}
		where := r.Env + "." + r.System
		what := r.Endpoint
		if r.Source == "secret" {
			where, what = r.Env, r.Secret
			if where == "" {
				where = "(top-level)"
			}
		}
		dl, na, iss := "-", "-", ""
		if r.Cert != nil {
			dl, na, iss = fmt.Sprint(r.Cert.DaysLeft), r.Cert.NotAfter.Format("2006-01-02T15:04Z"), r.Cert.Issuer
		}
		if r.Trusted != "" {
			iss += " / trusted: " + trunc(r.Trusted, 60)
		}
		if r.Error != nil {
			iss = string(r.Error.Code) + ": " + r.Error.Message
		}
		fmt.Fprintf(stdout, "%-9s %-26s %-28s %-9s %-6s %-21s %s\n", r.Status, where, trunc(what, 28), r.Source, dl, na, iss)
	}
	if !globals.JSON {
		if len(reps) == 0 {
			fmt.Fprintln(stdout, "no TLS endpoints or certificate secrets found")
		}
		fmt.Fprintf(stdout, "warn < %d days; expired or unreachable → exit 1\n", days)
	}
	return exit
}

// certsSummary runs certs for doctor (bounded) — used by cmd_doctor.go.
func certsSummary(ctx context.Context, path, env string, timeout time.Duration) ([]server.CertReport, error) {
	return server.NewEngine().Certs(ctx, path, server.CertsOptions{Env: env, WarnDays: 30, Timeout: timeout, Secrets: false})
}
