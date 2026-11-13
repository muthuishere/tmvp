package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/muthuishere/tmvp/internal/config"
	"github.com/muthuishere/tmvp/internal/envsel"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
)

// target is a parsed `[selector] [system] verb args` command line.
type target struct {
	Env, System, Verb string
	Args              []string
	// EnvGiven: the env came from the positional selector; otherwise Env is
	// "all" and runRead applies the env precedence (--env, TMVP_ENV, use, default_env).
	EnvGiven bool
}

// parseTarget resolves the positional grammar against cfg:
//
//	<envsel> <system> <verb> args   envsel: prod | prod,staging | all | prod* (system must exist)
//	<env.system> <verb> args        prod.db | prod.* | *.db
//	<system> <verb> args            envs omitted = all
//	<envsel> <verb> args            with -l: every system matching the labels
func parseTarget(cfg *config.Config, a []string, hasLabels bool) (target, error) {
	var t target
	if len(a) == 0 {
		return t, errs.New(errs.Usage, "nothing to run").WithHint("tmvp [envs] <system> <verb> [args]")
	}
	var rest []string
	switch {
	case strings.Contains(a[0], ".") && !cfg.HasSystem(a[0]):
		s := config.ParseSelector(a[0])
		t.Env, t.System, rest, t.EnvGiven = s.Env, s.System, a[1:], true
	case cfg.HasSystem(a[0]):
		t.Env, t.System, rest = "all", a[0], a[1:]
	case cfg.IsEnvSelector(a[0]):
		t.Env, t.EnvGiven = a[0], true
		if len(a) > 1 && cfg.HasSystem(a[1]) {
			t.System, rest = a[1], a[2:]
		} else if hasLabels {
			t.System, rest = "*", a[1:]
		} else {
			return t, errs.New(errs.Usage, "no system given after %q", a[0]).WithHint("tmvp " + a[0] + " <system> <verb>, or select with -l")
		}
	default:
		return t, errs.New(errs.NotFound, "%q is neither an env nor a system", a[0]).WithHint("tmvp config show --resolved")
	}
	if len(rest) == 0 {
		return t, errs.New(errs.Usage, "no verb given").WithHint("tmvp [envs] <system> <verb> [args]")
	}
	t.Verb, t.Args = rest[0], rest[1:]
	return t, nil
}

func runRead(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return cmd.Help()
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	t, err := parseTarget(cfg, args, globals.Labels != "")
	if err != nil {
		return err
	}
	sel, err := selectEnv(cfg, t.Env, t.EnvGiven)
	if err != nil {
		return err
	}
	req := baseRequest(protocol.OpRun, cfg)
	req.Env, req.System, req.Verb, req.Args = sel.Env, t.System, t.Verb, t.Args
	req.EnvSource = sel.Source
	if req.Env == "" {
		req.Env = "all"
	}
	printEnvHeader(cfg, req.Env, sel)
	return send(cmd.Context(), req)
}

// selectEnv applies the env precedence: positional selector / --env >
// TMVP_ENV > tmvp use > default_env > none (all envs).
func selectEnv(cfg *config.Config, positional string, given bool) (envsel.Selection, error) {
	explicit := ""
	if given {
		explicit = positional
	}
	if globals.Env != "" {
		if explicit != "" && explicit != globals.Env {
			return envsel.Selection{}, errs.New(errs.Usage, "env given twice: %q and --env %q", explicit, globals.Env).
				WithHint("name the env once: tmvp " + explicit + " <system> … or tmvp -e " + globals.Env + " <system> …")
		}
		explicit = globals.Env
	}
	return envsel.Resolve(cfg, explicit), nil
}

// printEnvHeader tells the reader (stderr, text mode) which envs run and why.
func printEnvHeader(cfg *config.Config, envSel string, sel envsel.Selection) {
	if globals.JSON {
		return
	}
	names, err := cfg.MatchEnvs(envSel)
	list := envSel
	if err == nil {
		list = strings.Join(names, ",")
	}
	src := sel.Source
	if src == "" {
		src = protocol.EnvNone
	}
	fmt.Fprintf(stderr, "# env: %s (source: %s — %s)\n", list, src, sel.Where)
}
