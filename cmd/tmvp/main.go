// Command tmvp: one binary, two roles — the thin client (default) and the
// local server (`tmvp server run`). See docs/DESIGN.md and CONTRACT.md.
//
// Adding a subcommand: create cmd_<name>.go with
//
//	func init() { addCommand(func() *cobra.Command { return &cobra.Command{...} }) }
//
// and use the helpers here (loadConfig, send, globals). Never edit another
// agent's cmd_*.go file.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/muthuishere/tmvp"
	"github.com/muthuishere/tmvp/internal/skill"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/muthuishere/tmvp/internal/client"
	"github.com/muthuishere/tmvp/internal/config"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
	"github.com/muthuishere/tmvp/internal/server"
)

// version is set with -ldflags "-X main.version=..."
var version = "dev"

// Globals are the persistent flags shared by every subcommand.
type Globals struct {
	Skill   string
	Config  string
	JSON    bool
	Diff    bool
	Limit   int
	Since   string
	Reason  string
	Labels  string
	Timeout string
	Force   bool     // reads: skip the cost estimate (timeout still kills); writes: allow a protected env
	Set     []string // -F key=value pass-through flags for connectors
	Env     string   // --env/-e env selector (overrides TMVP_ENV / tmvp use / default_env)
}

var globals Globals

var commands []func() *cobra.Command

// addCommand registers a subcommand constructor. Call from init().
func addCommand(f func() *cobra.Command) { commands = append(commands, f) }

// exitError carries a process exit code through cobra.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

// exitCode returns an error that makes tmvp exit with code (nil for 0).
func exitCode(code int) error {
	if code == 0 {
		return nil
	}
	return exitError{code}
}

var stdout io.Writer = os.Stdout
var stderr io.Writer = os.Stderr

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "tmvp [envs|all|selector] <system> <verb> [args]",
		Short: "Run a product skill's systems across environments (read by default)",
		Long: `tmvp reads postgres/k8s/nats/... across every env of one product.

  tmvp all db query "select 1"          # every env that has db
  tmvp prod,staging db describe users
  tmvp '*.db' health                      # selector: any env, system db
  tmvp prod -l role=primary health        # label selector
  tmvp write prod db "update ..." --reason "fix #42" [--preview]`,
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runRead,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&globals.Skill, "skill", "", "skill directory (uses <dir>/config.yaml)")
	pf.StringVar(&globals.Config, "config", "", "config file (overrides lookup order)")
	pf.BoolVar(&globals.JSON, "json", false, "JSONL output (one frame per line)")
	pf.BoolVar(&globals.Diff, "diff", false, "only show rows/lines where envs disagree")
	pf.IntVar(&globals.Limit, "limit", 0, "row/line cap per env (default 1000 rows / 500 lines)")
	pf.StringVar(&globals.Since, "since", "", "time window, e.g. 1h, 1d")
	pf.StringVar(&globals.Reason, "reason", "", "why (optional on reads, required on writes)")
	pf.StringVarP(&globals.Labels, "labels", "l", "", "label selector: k=v,k2!=v,'k in (a,b)'")
	pf.StringVar(&globals.Timeout, "timeout", "", "per-target timeout and hard kill (default: system > env > global limits.timeout > 10s); reads estimated above it are refused")
	pf.BoolVar(&globals.Force, "force", false, "reads: run even if the estimate exceeds the timeout (timeout still kills); writes: required for a protected env")
	pf.StringArrayVarP(&globals.Set, "set", "F", nil, "connector flag key=value (repeatable)")
	pf.StringVarP(&globals.Env, "env", "e", "", "env selector: dev | dev,prod | all | 'prod*' (overrides TMVP_ENV, tmvp use, default_env)")
	pf.Bool("llmhelp", false, "agent-oriented help for this command (no config or server needed)")
	root.Flags().SetInterspersed(true)
	for _, f := range commands {
		root.AddCommand(f())
	}
	return root
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	server.Version = version
	root := newRoot()
	if args, ok := stripLLMHelp(os.Args[1:]); ok {
		_ = root.ParseFlags(args) // --skill/--config for exact verb help; errors ignored
		llmHelp(root, args)
		return
	}
	if len(os.Args) < 2 || os.Args[1] != "server" {
		skill.Refresh(tmvp.Skills) // keep installed shipped skills current after an upgrade
	}
	if len(os.Args) == 2 && (os.Args[1] == "--help" || os.Args[1] == "-h") {
		_ = root.Help() // plain help needs no config
		return
	}
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		os.Args = []string{os.Args[0], "version"} // same as `tmvp version`; needs no config
	}
	root.SetArgs(passUnknownFlags(root, os.Args[1:]))
	err := root.ExecuteContext(ctx)
	if err == nil {
		return
	}
	var ee exitError
	if errors.As(err, &ee) {
		os.Exit(ee.code)
	}
	te := errs.As(err)
	if strings.Contains(err.Error(), "unknown flag") || strings.Contains(err.Error(), "unknown shorthand") {
		te = errs.New(errs.Usage, "%s", err.Error())
	}
	printError(te)
	os.Exit(errs.ExitCode(te.Code))
}

// loadConfig finds and loads the product config by lookup order.
func loadConfig() (*config.Config, error) {
	p, err := config.Find(config.FindOptions{ConfigPath: globals.Config, SkillPath: globals.Skill, Product: os.Getenv("TMVP_PRODUCT")})
	if err != nil {
		return nil, err
	}
	return config.Load(p)
}

// baseRequest fills the global fields of a request from flags + config.
func baseRequest(op string, cfg *config.Config) protocol.Request {
	cwd, _ := os.Getwd()
	r := protocol.Request{Op: op, Labels: globals.Labels, Reason: globals.Reason, Diff: globals.Diff,
		Limit: globals.Limit, Since: globals.Since, Agent: agentName(), Cwd: cwd, Timeout: globals.Timeout, Force: globals.Force}
	if cfg != nil {
		r.ConfigPath, r.Product = cfg.Path, cfg.Product
	}
	if len(globals.Set) > 0 {
		r.Flags = map[string]string{}
		for _, kv := range globals.Set {
			k, v, _ := strings.Cut(kv, "=")
			r.Flags[k] = v
		}
	}
	return r
}

func agentName() string {
	if a := os.Getenv("TMVP_AGENT"); a != "" {
		return a
	}
	switch {
	case os.Getenv("CLAUDECODE") != "":
		return "claude-code"
	case os.Getenv("CODEX_SANDBOX") != "" || os.Getenv("CODEX_HOME") != "":
		return "codex"
	}
	return ""
}

// send runs a request through the client and prints frames. Returns an
// exitError for non-zero exits.
func send(ctx context.Context, req protocol.Request) error {
	code, err := client.Do(ctx, req, printFrame)
	if err != nil {
		printError(errs.As(err))
		if code == 0 {
			code = 1
		}
	}
	return exitCode(code)
}

func printFrame(f protocol.Frame) {
	if globals.JSON {
		b, _ := json.Marshal(f)
		fmt.Fprintln(stdout, string(b))
		return
	}
	where := ""
	if f.Env != "" || f.System != "" {
		where = f.Env + "." + f.System
	}
	switch f.Type {
	case protocol.FrameRow:
		fmt.Fprintln(stdout, prefix(where)+formatRow(f.Row))
	case protocol.FrameText:
		if where != "" {
			fmt.Fprintf(stdout, "%s | %s\n", where, f.Text)
		} else {
			fmt.Fprintln(stdout, f.Text)
		}
	case protocol.FrameError:
		printError(f.Error)
	case protocol.FrameStatus:
		switch f.Status {
		case "skipped", "truncated":
			fmt.Fprintf(stderr, "%s: %s (%s)\n", where, f.Status, f.Text)
		case "ok":
			if f.Text != "" {
				fmt.Fprintf(stderr, "%s: %s\n", where, f.Text)
			}
		}
	case protocol.FrameInfo:
		b, _ := json.MarshalIndent(f.Info, "", "  ")
		fmt.Fprintln(stdout, string(b))
	}
}

func prefix(where string) string {
	if where == "" {
		return ""
	}
	return where + "  "
}

func formatRow(r map[string]any) string {
	keys := make([]string, 0, len(r))
	for k := range r {
		if k != "env" && k != "system" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		var v string
		switch x := r[k].(type) {
		case string:
			v = x
			if strings.ContainsAny(v, " \t\n\"") {
				b, _ := json.Marshal(v)
				v = string(b)
			}
		case nil:
			v = "null"
		default:
			b, _ := json.Marshal(x)
			v = string(b)
		}
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, "  ")
}

func printError(e *errs.Error) {
	if e == nil {
		return
	}
	if globals.JSON {
		b, _ := json.Marshal(protocol.Frame{Type: protocol.FrameError, Error: e})
		fmt.Fprintln(stdout, string(b))
		return
	}
	fmt.Fprintln(stderr, "error: "+e.Error())
}

// stripLLMHelp removes --llmhelp from args and reports whether it was there.
func stripLLMHelp(args []string) ([]string, bool) {
	out := make([]string, 0, len(args))
	found := false
	for _, a := range args {
		if a == "--llmhelp" || a == "--llmhelp=true" {
			found = true
			continue
		}
		out = append(out, a)
	}
	return out, found
}
