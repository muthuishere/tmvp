package main

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime/debug"
	"time"

	"github.com/muthuishere/clicodemode/codemode"
	"github.com/muthuishere/clicodemode/js"
	"github.com/spf13/cobra"

	"github.com/muthuishere/tmvp/internal/client"
	"github.com/muthuishere/tmvp/internal/codeops"
	"github.com/muthuishere/tmvp/internal/config"
	"github.com/muthuishere/tmvp/internal/errs"
)

// codeFlags are the program budgets and gates of `tmvp code`.
type codeFlags struct {
	input       string
	maxSteps    int64
	maxTime     time.Duration
	concurrency int
	allowWrites bool
	maxOps      int64
	maxMemory   int64 // MiB; 0 = no limit
}

func init() {
	addCommand(func() *cobra.Command {
		var f codeFlags
		c := &cobra.Command{
			Use:   "code <run|check|capabilities|describe|operations|docs|skill|scripts> [args]",
			Short: "Code Mode: run ONE JS program that calls tmvp operations (<system>.<verb>) in-process; only its final JSON returns",
			Long: `Code Mode: an agent sends one program; it calls tmvp operations (Promise.all,
loops) and only the final value comes back. Every call goes through the tmvp
server like a CLI call: read-only, cost guard (too_expensive), env-type
protection and the audit log (tmvp logs) all apply.

  tmvp code operations                      # db.query, db.describe, ...
  tmvp code describe db.query
  tmvp code run prog.js --json              # {ok, value, error, ms}
  tmvp code run stuck-orders                # <skill dir>/scripts/stuck-orders.js
  tmvp code scripts                         # saved scripts + first-line description
  tmvp code check prog.js                   # diagnostics, nothing runs (exit 2 on errors)

  const r = await Promise.all(["dev","staging","prod"].map(env =>
    ops.db.query({ env, args: ["select count(*) as n from orders"] })))

Operations take {env, args[], flags{}, labels, diff, limit, since, timeout, reason};
env accepts what the CLI selector accepts (dev, dev,prod, all, prod*, type:prod).
Results are {rows, lines, targets, errors, exit}. A refused call throws with
the tmvp code first (policy_blocked: ..., too_expensive: ...).
Writes are write.<system>.<verb> {env, args, reason, preview, force}; they
need --allow-writes and still go through the server write path.

Exit codes: 0 ok, 1 the program failed, 2 nothing ran (check errors, usage).
Scripts never hold secrets: credentials stay in config.yaml secret references.`,
			Args: cobra.ArbitraryArgs,
			RunE: func(cmd *cobra.Command, args []string) error { return runCode(cmd, args, f) },
		}
		fl := c.Flags()
		fl.StringVar(&f.input, "input", "", "JSON value the program reads as the global `input`")
		fl.Int64Var(&f.maxSteps, "max-steps", 1_000_000, "program step budget (statements + calls + loop iterations)")
		fl.DurationVar(&f.maxTime, "max-time", 60*time.Second, "wall-clock budget for the whole program")
		fl.IntVar(&f.concurrency, "concurrency", 4, "tmvp operations in flight at once")
		fl.Int64Var(&f.maxOps, "max-ops", 200, "tmvp operation calls per program; past it the program stops (uncatchable, exit 1)")
		fl.Int64Var(&f.maxMemory, "max-memory", 1024, "soft Go memory limit in MiB for this process (best effort: a runaway program fails instead of exhausting the machine); 0 = off")
		fl.BoolVar(&f.allowWrites, "allow-writes", false, "let write.<system>.<verb> run (server still requires reason, one env, force on protected envs)")
		return c
	})
}

func runCode(cmd *cobra.Command, args []string, f codeFlags) error {
	if len(args) == 0 {
		return cmd.Help()
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if globals.Env != "" {
		return errs.New(errs.Usage, "-e/--env does not apply to tmvp code").
			WithHint(`pass env in each operation call: ops.db.query({ env: "prod", args: [...] })`)
	}
	sub := args[0]
	if sub == "scripts" {
		return printScripts(cfg)
	}
	if f.maxSteps <= 0 || f.concurrency <= 0 || f.maxTime <= 0 || f.maxOps <= 0 || f.maxMemory < 0 {
		return errs.New(errs.Usage, "--max-steps, --max-time, --max-ops and --concurrency must be positive")
	}
	if f.maxMemory > 0 {
		debug.SetMemoryLimit(f.maxMemory << 20)
	}
	eng, err := codeEngine(cfg, f)
	if err != nil {
		return err
	}
	rest := append([]string{}, args[1:]...)
	if (sub == "run" || sub == "check" || sub == "compile") && len(rest) == 1 {
		p, err := codeops.ResolveScript(cfg, rest[0])
		if err != nil {
			printError(errs.As(err))
			return exitCode(2)
		}
		rest[0] = p
	}
	argv := append([]string{sub}, rest...)
	if globals.JSON {
		argv = append(argv, "--json")
	}
	if cmd.Flags().Changed("input") {
		argv = append(argv, "--input", f.input)
	}
	cli := codemode.NewCLI(eng, "tmvp code")
	cli.Stdin, cli.Stdout, cli.Stderr = os.Stdin, stdout, stderr
	return exitCode(cli.Main(cmd.Context(), argv))
}

// codeEngine builds the engine: registry from config + connector verbs, the
// client as the only path to a system, conservative budgets, no exec.
func codeEngine(cfg *config.Config, f codeFlags) (*codemode.Engine, error) {
	cwd, _ := os.Getwd()
	reg, skipped, err := codeops.Build(codeops.Options{Cfg: cfg, Send: client.Do, Version: version, Agent: agentName(), Cwd: cwd,
		MaxTimeout: f.maxTime})
	if err != nil {
		return nil, err
	}
	for _, s := range skipped {
		fmt.Fprintf(stderr, "# code: %s skipped (not a valid operation name: lower-case letters, digits, - and _)\n", s)
	}
	eng := codemode.NewEngine(reg)
	eng.Stdout = stderr
	eng.Policy = codemode.Policy{
		Allow:  codeops.Policy(f.allowWrites),
		Limits: js.Limits{MaxSteps: f.maxSteps, Timeout: f.maxTime, MaxConcurrency: f.concurrency, MaxOps: f.maxOps},
	}
	return eng, nil
}

func printScripts(cfg *config.Config) error {
	list := codeops.Scripts(cfg)
	if globals.JSON {
		if list == nil {
			list = []codeops.Script{}
		}
		b, _ := json.MarshalIndent(list, "", "  ")
		fmt.Fprintln(stdout, string(b))
		return nil
	}
	if len(list) == 0 {
		fmt.Fprintf(stderr, "no saved scripts in %s\n", codeops.ScriptsDir(cfg))
		return nil
	}
	for _, s := range list {
		fmt.Fprintf(stdout, "%-24s %s\n", s.Name, s.Description)
	}
	return nil
}
