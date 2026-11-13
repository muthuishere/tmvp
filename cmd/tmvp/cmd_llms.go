package main

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/muthuishere/tmvp"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func init() {
	addCommand(func() *cobra.Command {
		var full bool
		c := &cobra.Command{
			Use:   "llms [topic] [--full]",
			Short: "Print the agent docs: llms.txt index, one topic (config, writes, postgres, …), or everything",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				switch {
				case full:
					return printDoc("llms-full.txt")
				case len(args) == 0:
					return printDoc("llms.txt")
				}
				p, ok := topicPath(args[0])
				if !ok {
					return errs.New(errs.NotFound, "no topic %q", args[0]).WithHint("topics: " + strings.Join(llmsTopics(), ", "))
				}
				return printDoc(p)
			},
		}
		c.Flags().BoolVar(&full, "full", false, "print llms-full.txt (every topic)")
		return c
	})
}

func printDoc(p string) error {
	b, err := fs.ReadFile(tmvp.Docs, p)
	if err != nil {
		return errs.Wrap(errs.NotFound, err, "doc")
	}
	_, err = stdout.Write(b)
	return err
}

// topicPath maps "config", "postgres", "types/postgres", "config.md" to an embedded file.
func topicPath(t string) (string, bool) {
	t = strings.TrimSuffix(strings.TrimPrefix(t, "docs/llms/"), ".md")
	for _, p := range []string{"docs/llms/" + t + ".md", "docs/llms/types/" + t + ".md"} {
		if _, err := fs.Stat(tmvp.Docs, p); err == nil {
			return p, true
		}
	}
	return "", false
}

func llmsTopics() []string {
	var out []string
	_ = fs.WalkDir(tmvp.Docs, "docs/llms", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".md") {
			out = append(out, strings.TrimSuffix(path.Base(p), ".md"))
		}
		return err
	})
	sort.Strings(out)
	return out
}

// llmDoc is the hand-written part of a command's --llmhelp.
type llmDoc struct {
	Mode     string // read | write | local | server
	Examples []string
	Topic    string
}

var llmDocs = map[string]llmDoc{
	"":               {"read", []string{`tmvp envs`, `tmvp systems`, `tmvp all db ls`, `tmvp prod,staging db query "select count(*) from users" --diff`, `tmvp prod db query @big-report --timeout 2m   # cost guard: runs if its estimate ≤ 2m`}, "overview"},
	"envs":           {"local (config only, no server)", []string{"tmvp envs", "tmvp envs --json"}, "overview"},
	"systems":        {"local (config only; --check runs login checks)", []string{"tmvp systems", "tmvp systems prod", "tmvp systems --check --json"}, "overview"},
	"verbs":          {"local (connector registry)", []string{"tmvp verbs", "tmvp verbs postgres web", "tmvp verbs --json"}, "overview"},
	"llms":           {"local", []string{"tmvp llms", "tmvp llms config", "tmvp llms postgres", "tmvp llms --full"}, "overview"},
	"write":          {"write (one env, one system; --reason required; audited)", []string{`tmvp write prod db "update orders set status='paid' where id=42" --reason "support #812" --preview`, `tmvp write prod cluster restart deploy/api --reason "stuck workers" --force   # prod is protected`, `tmvp write prod bastion @rotate-logs --reason "disk 95%" --force`, `tmvp write db "delete from tmp" --reason "cleanup"   # env from -e/TMVP_ENV/use/default_env`}, "writes"},
	"validate":       {"local (never runs a secret command)", []string{"tmvp validate", "tmvp --skill .agents/skills/acme-tmvp validate --json"}, "config"},
	"check":          {"server (resolves secrets, connects)", []string{"tmvp check", "tmvp check prod db"}, "troubleshooting"},
	"config show":    {"local (secrets masked)", []string{"tmvp config show", "tmvp config show --resolved"}, "config"},
	"config path":    {"local", []string{"tmvp config path"}, "config"},
	"login":          {"server", []string{"tmvp login prod admin", "tmvp login prod admin --user ops"}, "troubleshooting"},
	"logs":           {"local (audit files)", []string{"tmvp logs --since 1d", "tmvp logs --writes --json"}, "writes"},
	"export":         {"local (never secret values)", []string{"tmvp export pg-service prod", "tmvp export kubeconfig"}, "config"},
	"new skill":      {"local", []string{"tmvp new skill acme --systems db:postgres,cluster:k8s,admin:web", "tmvp new skill acme --envs dev,prod --path .agents/skills"}, "skills"},
	"skill add":      {"local (edits config.yaml, regenerates SKILL.md)", []string{"tmvp skill add cache redis", "tmvp skill add api http --env prod"}, "skills"},
	"skill sync":     {"local", []string{"tmvp skill sync", "tmvp --skill .agents/skills/acme-tmvp skill sync"}, "skills"},
	"skill lint":     {"local (structure only; never connects)", []string{"tmvp skill lint", "tmvp skill lint .agents/skills/acme-tmvp --json"}, "skills"},
	"secrets reload": {"server", []string{"tmvp secrets reload"}, "server"},
	"secrets clear":  {"server", []string{"tmvp secrets clear"}, "server"},
	"secrets status": {"server (fetches each secret; prints names, states, dates — never values)", []string{"tmvp secrets status", "tmvp secrets status -e prod --days 14", "tmvp secrets status --json"}, "secrets"},
	"secrets audit":  {"local (audit log; no values)", []string{"tmvp secrets audit --since 1d", "tmvp secrets audit --json"}, "secrets"},
	"certs":          {"local (in-process TLS handshakes via `via:`; read-only; 5s timeout per endpoint)", []string{"tmvp certs", "tmvp certs -e prod --days 14", "tmvp certs --json"}, "secrets"},
	"use":            {"local (state file TMVP_HOME/state/env.json, 0600, per product + config path)", []string{"tmvp use", "tmvp use dev", "tmvp use prod   # allowed; writes there still need --force", "tmvp use --clear", "TMVP_ENV=staging tmvp db ls"}, "environments"},
	"server status":  {"server", []string{"tmvp server status"}, "server"},
	"server stop":    {"server", []string{"tmvp server stop"}, "server"},
	"server start":   {"server", []string{"tmvp server start"}, "server"},
	"server run":     {"server (foreground)", []string{"TMVP_SOCKET=/tmp/t.sock tmvp server run"}, "server"},
	"version":        {"local", []string{"tmvp version"}, "install"},
	"doctor":         {"local (in-process; never starts the server; secret names/states only)", []string{"tmvp doctor", "tmvp doctor --offline", "tmvp --skill .agents/skills/acme-tmvp doctor --env prod --system db --json"}, "doctor"},
}

// llmHelp prints agent-oriented help for the command line in args (with
// --llmhelp removed). Needs no config and no server.
func llmHelp(root *cobra.Command, args []string) {
	valued := map[string]bool{"--skill": true, "--config": true, "--limit": true, "--since": true, "--reason": true,
		"-l": true, "--labels": true, "--timeout": true, "-F": true, "--set": true, "--path": true, "--envs": true,
		"--systems": true, "--system": true, "--env": true, "-e": true, "--days": true, "--user": true, "--to": true}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			if valued[a] {
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	cmd, rest, err := root.Find(pos)
	if err == nil && cmd != root {
		printCmdHelp(cmd)
		if cmd.Name() == "verbs" {
			for _, t := range rest {
				if _, ok := connector.Lookup(t); ok {
					fmt.Fprintf(stdout, "\n## %s verbs\n", t)
					printVerbTable(t)
					fmt.Fprintf(stdout, "related: tmvp llms %s\n", t)
				}
			}
		}
		return
	}
	if len(pos) == 0 {
		printCmdHelp(root)
		return
	}
	verbHelp(pos)
}

func cmdPath(c *cobra.Command) string {
	p := strings.TrimPrefix(c.CommandPath(), c.Root().Name())
	return strings.TrimSpace(p)
}

func printCmdHelp(c *cobra.Command) {
	key := cmdPath(c)
	d := llmDocs[key]
	name := "tmvp " + key
	if key == "" {
		name = "tmvp (read dispatch)"
	}
	fmt.Fprintf(stdout, "# %s\n", strings.TrimSpace(name))
	fmt.Fprintf(stdout, "purpose: %s\n", c.Short)
	use := c.UseLine()
	if key == "" {
		use = "tmvp [envsel] <system> <verb> [args] [flags]"
	}
	fmt.Fprintf(stdout, "syntax: %s\n", use)
	if d.Mode != "" {
		fmt.Fprintf(stdout, "mode: %s\n", d.Mode)
	}
	if key == "" {
		fmt.Fprintln(stdout, "  envsel: prod | prod,staging | all | 'prod*' | prod.db | '*.db'; -l k=v selects by labels")
		fmt.Fprintln(stdout, "  envsel omitted → --env/-e > TMVP_ENV > `tmvp use` > default_env > all envs (header/JSONL say which: env_source)")
		fmt.Fprintln(stdout, "  reads batch across envs in parallel; mutations only via `tmvp write`; @name runs a saved item")
	}
	if subs := c.Commands(); len(subs) > 0 {
		fmt.Fprintln(stdout, "subcommands:")
		for _, s := range subs {
			if !s.Hidden && s.Name() != "help" && s.Name() != "completion" {
				fmt.Fprintf(stdout, "  %-10s %s\n", s.Name(), s.Short)
			}
		}
	}
	var flags []string
	c.NonInheritedFlags().VisitAll(func(f *pflag.Flag) {
		if f.Name != "help" && f.Name != "llmhelp" {
			flags = append(flags, fmt.Sprintf("  --%s  %s", f.Name, f.Usage))
		}
	})
	if len(flags) > 0 {
		fmt.Fprintln(stdout, "flags:")
		fmt.Fprintln(stdout, strings.Join(flags, "\n"))
	}
	fmt.Fprintln(stdout, "global: -e/--env <envsel> --skill <dir> --config <file> --json --limit N --since 1h --reason \"…\" -l k=v -F key=value --llmhelp")
	if len(d.Examples) > 0 {
		fmt.Fprintln(stdout, "examples:")
		for _, e := range d.Examples {
			fmt.Fprintln(stdout, "  "+e)
		}
	}
	topic := d.Topic
	if topic == "" {
		topic = "overview"
	}
	fmt.Fprintf(stdout, "related: tmvp llms %s\n", topic)
}

// verbHelp explains `tmvp <sel> <system> <verb> …`. With a loadable config
// the system's type is exact; without one every type having that verb is shown.
func verbHelp(pos []string) {
	var typ, verb, sys string
	if cfg, err := loadConfig(); err == nil {
		if t, err := parseTarget(cfg, pos, globals.Labels != ""); err == nil {
			verb, sys = t.Verb, t.System
			for _, en := range cfg.EnvOrder {
				if s := cfg.System(en, t.System); s != nil {
					typ = s.Type
					break
				}
			}
		}
	}
	if verb == "" {
		for i := len(pos) - 1; i >= 0 && verb == ""; i-- {
			for _, t := range connector.Types() {
				if _, ok := connector.FindVerb(connector.VerbsOf(t), pos[i]); ok {
					verb = pos[i]
					if i > 0 {
						sys = pos[i-1]
					}
					break
				}
			}
		}
	}
	if verb == "" {
		fmt.Fprintf(stdout, "# tmvp %s\nno verb recognised in %q.\n", strings.Join(pos, " "), strings.Join(pos, " "))
		fmt.Fprintln(stdout, "syntax: tmvp [envsel] <system> <verb> [args]; list verbs: tmvp verbs [type]; systems: tmvp systems")
		fmt.Fprintln(stdout, "related: tmvp llms overview")
		return
	}
	if sys == "" {
		sys = "<system>"
	}
	types := []string{typ}
	if typ == "" {
		types = nil
		for _, t := range connector.Types() {
			if _, ok := connector.FindVerb(connector.VerbsOf(t), verb); ok {
				types = append(types, t)
			}
		}
	}
	fmt.Fprintf(stdout, "# tmvp <envsel> %s %s\n", sys, verb)
	if typ == "" {
		fmt.Fprintf(stdout, "no config loaded: the verb exists in types %s (`tmvp systems` shows which type %s is)\n", strings.Join(types, ", "), sys)
	}
	for _, t := range types {
		v, _ := connector.FindVerb(connector.VerbsOf(t), verb)
		fmt.Fprintf(stdout, "\n## %s %s — %s\n", t, v.Name, modeText(v))
		fmt.Fprintf(stdout, "purpose/syntax: %s\n", v.Help)
		if v.Mode.Readable() {
			fmt.Fprintf(stdout, "examples:\n  tmvp all %s %s …            # every env, in parallel\n  tmvp prod %s %s … --json     # one env, JSONL\n", sys, v.Name, sys, v.Name)
		}
		if v.Mode.Writable() {
			fmt.Fprintf(stdout, "  tmvp write prod %s %s … --reason \"why\" --preview\n", sys, v.Name)
		}
		if v.Saved {
			fmt.Fprintf(stdout, "  tmvp prod %s @<saved-name> …   # runs a saved item (see: tmvp prod %s ls)\n", sys, sys)
		}
		fmt.Fprintf(stdout, "related: tmvp llms %s · tmvp verbs %s\n", t, t)
	}
}

func modeText(v connector.Verb) string {
	s := v.Mode.String()
	switch v.Mode {
	case connector.Read:
		s += " (plain call; batches across envs)"
	case connector.Write:
		s += " (only `tmvp write <env> <system> … --reason`)"
	case connector.Destructive:
		s += " (tmvp write + destructive: true on the env/system)"
	case connector.ReadWrite:
		s += " (read when it cannot mutate; mutations via tmvp write)"
	}
	if v.Default {
		s += " [default write verb]"
	}
	if v.Saved {
		s += " [runs @saved items]"
	}
	return s
}

func printVerbTable(typ string) {
	for _, v := range connector.VerbsOf(typ) {
		fmt.Fprintf(stdout, "  %-14s %-12s %s\n", v.Name, v.Mode.String(), v.Help)
	}
}

// VerbTableMarkdown is the generated per-type verb table in docs/llms/types/<type>.md.
func VerbTableMarkdown(typ string) string {
	var b strings.Builder
	b.WriteString("| Verb | Mode | Help |\n|---|---|---|\n")
	for _, v := range connector.VerbsOf(typ) {
		m := v.Mode.String()
		if v.Default {
			m += ", default"
		}
		if v.Saved {
			m += ", @item"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", v.Name, m, strings.ReplaceAll(v.Help, "|", `\|`))
	}
	return b.String()
}
