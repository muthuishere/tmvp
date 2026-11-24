// Package codeops builds the Code Mode operation registry for tmvp
// (`tmvp code ...`, library github.com/muthuishere/clicodemode).
//
// Every operation is `<system>.<verb>` for the systems in the loaded config
// and the verbs their connector types declare. A handler never touches a
// system itself: it builds the same protocol.Request the CLI builds and sends
// it through the client to the tmvp server, so read-only, the cost guard
// (too_expensive), env-type protection and the audit log are enforced by the
// server exactly as for a CLI call. clicodemode's Policy.Allow is a second
// layer only.
//
// Reads are SideEffectNone. Write and destructive verbs are registered as
// `write.<system>.<verb>` (SideEffectExternal, `reason` required, one env)
// and go through the unchanged server write path (OpWrite: reason, single
// target, protected envs need force, preview, audited). The policy refuses
// them unless the program was started with writes allowed.
package codeops

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/muthuishere/clicodemode/codemode"

	"github.com/muthuishere/tmvp/internal/config"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/envsel"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
)

// Sender sends one request and streams frames back (client.Do).
type Sender func(ctx context.Context, req protocol.Request, fn func(protocol.Frame)) (int, error)

// Options configure Build.
type Options struct {
	Cfg     *config.Config
	Send    Sender
	Version string
	Agent   string // caller agent recorded in the audit log
	Cwd     string
	// MaxTimeout caps a per-call `timeout` (the program's --max-time): a
	// program cannot raise the cost-guard ceiling above its own budget.
	MaxTimeout time.Duration
}

// guard enforces the per-call timeout ceiling. (The per-program operation
// budget is clicodemode's js.Limits.MaxOps.)
func (o Options) guard(a map[string]any) error {
	if s, ok := a["timeout"].(string); ok && s != "" && o.MaxTimeout > 0 {
		d, err := time.ParseDuration(s)
		if err != nil {
			return errs.New(errs.Usage, "timeout %q is not a duration", s)
		}
		if d > o.MaxTimeout {
			return errs.New(errs.PolicyBlocked, "timeout %s is above the program budget --max-time %s", s, o.MaxTimeout).WithHint("narrow the query, or raise --max-time")
		}
	}
	return nil
}

// Modules are tmvp's own JS recipes (js/drift.js is "tmvp:drift"), baked into
// the binary. They compose operations by name with call(), so they work for
// any product config and add no privilege.
//
//go:embed js
var jsModules embed.FS

var opPart = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// Build returns the registry for cfg. Systems or verbs whose names are not
// valid operation segments (lower-case, starting with a letter) are skipped
// and reported in skipped.
func Build(o Options) (reg *codemode.Registry, skipped []string, err error) {
	reg = codemode.NewRegistry("tmvp " + o.Version)
	if err := reg.RegisterModules("tmvp", jsModules, "js"); err != nil {
		return nil, nil, err
	}
	envRequired := o.Cfg.DefaultEnv == ""
	for _, sys := range systems(o.Cfg) {
		if !opPart.MatchString(sys.name) {
			skipped = append(skipped, sys.name)
			continue
		}
		for _, v := range sys.verbs {
			if !opPart.MatchString(v.Name) {
				skipped = append(skipped, sys.name+"."+v.Name)
				continue
			}
			if v.Mode.Readable() {
				if err := reg.Register(readOp(o, sys, v, envRequired)); err != nil {
					return nil, skipped, err
				}
			}
			if v.Mode.Writable() {
				if err := reg.Register(writeOp(o, sys, v, envRequired)); err != nil {
					return nil, skipped, err
				}
			}
		}
	}
	return reg, skipped, nil
}

type sysInfo struct {
	name  string
	types []string
	envs  []string
	verbs []connector.Verb
}

// systems is every system name in the config with the union of its types'
// verbs (one name may be a different type in different envs).
func systems(cfg *config.Config) []sysInfo {
	idx := map[string]*sysInfo{}
	var order []string
	for _, en := range cfg.EnvOrder {
		env := cfg.Envs[en]
		if env == nil {
			continue
		}
		for _, sn := range env.SystemOrder {
			s := env.Systems[sn]
			if s == nil {
				continue
			}
			si := idx[sn]
			if si == nil {
				si = &sysInfo{name: sn}
				idx[sn] = si
				order = append(order, sn)
			}
			si.envs = append(si.envs, en)
			if !contains(si.types, s.Type) {
				si.types = append(si.types, s.Type)
			}
		}
	}
	out := make([]sysInfo, 0, len(order))
	for _, sn := range order {
		si := idx[sn]
		seen := map[string]int{}
		for _, t := range si.types {
			for _, v := range connector.VerbsOf(t) {
				if i, ok := seen[v.Name]; ok {
					// same verb name in two types: readable if either is,
					// writable if either is
					if si.verbs[i].Mode != v.Mode {
						si.verbs[i].Mode = connector.ReadWrite
					}
					continue
				}
				seen[v.Name] = len(si.verbs)
				si.verbs = append(si.verbs, v)
			}
		}
		out = append(out, *si)
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

const envDoc = "env selector, as on the CLI: an env name, a comma list (dev,prod), all, a glob (prod*) or type:prod"

func readSchema(required bool) json.RawMessage {
	s := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"env":     map[string]any{"type": "string", "description": envDoc},
			"args":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "verb arguments, as after the verb on the CLI"},
			"flags":   map[string]any{"type": "object", "description": "connector flags (-F key=value)"},
			"labels":  map[string]any{"type": "string", "description": "label selector k=v,k2!=v"},
			"diff":    map[string]any{"type": "boolean", "description": "only rows/lines where envs disagree"},
			"limit":   map[string]any{"type": "integer", "description": "row/line cap per env"},
			"since":   map[string]any{"type": "string", "description": "time window, e.g. 1h"},
			"timeout": map[string]any{"type": "string", "description": "per-target timeout; reads estimated above it are refused (too_expensive)"},
			"reason":  map[string]any{"type": "string", "description": "why (optional on reads, recorded in the audit log)"},
		},
		"additionalProperties": false,
	}
	if required {
		s["required"] = []string{"env"}
	}
	b, _ := json.Marshal(s)
	return b
}

func writeSchema() json.RawMessage {
	s := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"env":     map[string]any{"type": "string", "description": "exactly one env name"},
			"args":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"flags":   map[string]any{"type": "object"},
			"reason":  map[string]any{"type": "string", "description": "why (required, audited)"},
			"preview": map[string]any{"type": "boolean", "description": "show the blast radius, change nothing"},
			"force":   map[string]any{"type": "boolean", "description": "required on a protected (destructive: false) env"},
			"timeout": map[string]any{"type": "string"},
		},
		"required":             []string{"env", "reason"},
		"additionalProperties": false,
	}
	b, _ := json.Marshal(s)
	return b
}

func describe(sys sysInfo, v connector.Verb) string {
	d := v.Help
	if d == "" {
		d = v.Name
	}
	return fmt.Sprintf("%s (%s %s in %s). %s", v.Name, strings.Join(sys.types, "/"), sys.name, strings.Join(sys.envs, ","), d)
}

func readOp(o Options, sys sysInfo, v connector.Verb, envRequired bool) codemode.Operation {
	return codemode.Operation{
		Name:        sys.name + "." + v.Name,
		Description: "Read: " + describe(sys, v) + " Returns {rows, lines, targets, errors}.",
		InputSchema: readSchema(envRequired),
		SideEffects: codemode.SideEffectNone,
		Idempotent:  true,
		Examples: []codemode.Example{{Description: "run on every env",
			Args: map[string]any{"env": "all", "args": []any{}}}},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			if err := o.guard(args); err != nil {
				return nil, &OpError{Errs: []Err{fromErr(err)}}
			}
			req, err := request(o, protocol.OpRun, sys.name, v.Name, args)
			if err != nil {
				return nil, err
			}
			return Send(ctx, o.Send, req)
		},
	}
}

func writeOp(o Options, sys sysInfo, v connector.Verb, _ bool) codemode.Operation {
	return codemode.Operation{
		Name:          "write." + sys.name + "." + v.Name,
		Description:   "Write (server write path: reason required, one env, force on protected envs, audited): " + describe(sys, v),
		InputSchema:   writeSchema(),
		SideEffects:   codemode.SideEffectExternal,
		Authorization: "write",
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			if err := o.guard(args); err != nil {
				return nil, &OpError{Errs: []Err{fromErr(err)}}
			}
			req, err := request(o, protocol.OpWrite, sys.name, v.Name, args)
			if err != nil {
				return nil, err
			}
			return Send(ctx, o.Send, req)
		},
	}
}

// request builds the protocol.Request exactly like the CLI does, from the
// program's arguments only: no global --force/--reason leaks in.
func request(o Options, op, system, verb string, a map[string]any) (protocol.Request, error) {
	r := protocol.Request{Op: op, System: system, Verb: verb, Agent: o.Agent, Cwd: o.Cwd,
		ConfigPath: o.Cfg.Path, Product: o.Cfg.Product}
	env, _ := a["env"].(string)
	if env == "" {
		sel := envsel.Resolve(o.Cfg, "")
		r.Env, r.EnvSource = sel.Env, sel.Source
	} else {
		r.Env, r.EnvSource = env, protocol.EnvExplicit
	}
	if r.Env == "" {
		if op == protocol.OpWrite {
			return r, errs.New(errs.Usage, "no env selected for the write")
		}
		r.Env = "all"
	}
	if xs, ok := a["args"].([]any); ok {
		for _, x := range xs {
			s, ok := x.(string)
			if !ok {
				return r, errs.New(errs.Usage, "args must be strings")
			}
			r.Args = append(r.Args, s)
		}
	}
	if fl, ok := a["flags"].(map[string]any); ok && len(fl) > 0 {
		r.Flags = map[string]string{}
		for k, v := range fl {
			r.Flags[k] = fmt.Sprint(v)
		}
	}
	r.Labels, _ = a["labels"].(string)
	r.Diff, _ = a["diff"].(bool)
	r.Since, _ = a["since"].(string)
	r.Timeout, _ = a["timeout"].(string)
	r.Reason, _ = a["reason"].(string)
	if n, ok := a["limit"].(float64); ok {
		r.Limit = int(n)
	}
	if op == protocol.OpWrite {
		r.Diff = false
		r.Preview, _ = a["preview"].(bool)
		r.Force, _ = a["force"].(bool)
	}
	return r, nil
}

// Result is what an operation returns to the program.
type Result struct {
	Rows    []map[string]any `json:"rows"`
	Lines   []Line           `json:"lines"`
	Targets []Target         `json:"targets"`
	Errors  []Err            `json:"errors"`
	Exit    int              `json:"exit"`
}

// Line is one text line of one target.
type Line struct {
	Env    string `json:"env"`
	System string `json:"system"`
	Text   string `json:"text"`
}

// Target is one env/system status.
type Target struct {
	Env    string  `json:"env"`
	System string  `json:"system"`
	Status string  `json:"status"`
	Note   string  `json:"note,omitempty"`
	MS     float64 `json:"ms,omitempty"`
}

// Err is one per-target (or request-level) error.
type Err struct {
	Env     string `json:"env,omitempty"`
	System  string `json:"system,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

func (e Err) String() string {
	s := e.Code + ": " + e.Message
	if e.Hint != "" {
		s += " (hint: " + e.Hint + ")"
	}
	return s
}

// OpError is thrown into the program; its text starts with the tmvp error
// code (policy_blocked, too_expensive, ...).
type OpError struct{ Errs []Err }

func (e *OpError) Error() string {
	parts := make([]string, 0, len(e.Errs))
	for _, x := range e.Errs {
		parts = append(parts, x.String())
	}
	return strings.Join(parts, "; ")
}

// Send runs req through send and collects the frames into a Result. It
// returns an *OpError (the program sees a thrown error) when the request was
// refused before any target ran, or when no target succeeded; otherwise the
// per-target errors are in Result.Errors.
func Send(ctx context.Context, send Sender, req protocol.Request) (any, error) {
	res := Result{Rows: []map[string]any{}, Lines: []Line{}, Targets: []Target{}, Errors: []Err{}}
	code, err := send(ctx, req, func(f protocol.Frame) {
		switch f.Type {
		case protocol.FrameRow:
			row := map[string]any{}
			for k, v := range f.Row {
				row[k] = v
			}
			if _, ok := row["env"]; !ok && f.Env != "" {
				row["env"] = f.Env
			}
			if _, ok := row["system"]; !ok && f.System != "" {
				row["system"] = f.System
			}
			res.Rows = append(res.Rows, row)
		case protocol.FrameText:
			res.Lines = append(res.Lines, Line{Env: f.Env, System: f.System, Text: f.Text})
		case protocol.FrameStatus:
			res.Targets = append(res.Targets, Target{Env: f.Env, System: f.System, Status: f.Status, Note: f.Text, MS: f.Elapsed})
		case protocol.FrameError:
			if f.Error != nil {
				res.Errors = append(res.Errors, Err{Env: firstNonEmpty(f.Error.Env, f.Env), System: firstNonEmpty(f.Error.System, f.System),
					Code: string(f.Error.Code), Message: f.Error.Message, Hint: f.Error.Hint})
			}
		}
	})
	res.Exit = code
	if err != nil {
		e := errs.As(err)
		res.Errors = append(res.Errors, Err{Code: string(e.Code), Message: e.Message, Hint: e.Hint})
		return nil, &OpError{Errs: res.Errors}
	}
	if code == 0 {
		return res, nil
	}
	ok := 0
	for _, t := range res.Targets {
		if t.Status == "ok" || t.Status == "truncated" {
			ok++
		}
	}
	if ok == 0 && len(res.Rows) == 0 && len(res.Lines) == 0 {
		if len(res.Errors) == 0 {
			res.Errors = append(res.Errors, Err{Code: string(errs.Runtime), Message: fmt.Sprintf("exit %d", code)})
		}
		return nil, &OpError{Errs: res.Errors}
	}
	return res, nil
}

func fromErr(err error) Err {
	e := errs.As(err)
	return Err{Code: string(e.Code), Message: e.Message, Hint: e.Hint}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Policy is the second-layer gate: reads always; writes only when allowed.
// The server still enforces every rule on whatever gets through.
func Policy(allowWrites bool) func(context.Context, *codemode.Operation) error {
	return func(_ context.Context, op *codemode.Operation) error {
		if op.SideEffects == codemode.SideEffectNone {
			return nil
		}
		if allowWrites && strings.HasPrefix(op.Name, "write.") {
			return nil
		}
		return fmt.Errorf("policy_blocked: %s changes a system; start the program with --allow-writes (the server still requires reason, one env, and force on protected envs)", op.Name)
	}
}

// ScriptsDir is <skill dir>/scripts.
func ScriptsDir(cfg *config.Config) string { return cfg.Dir + string(os.PathSeparator) + "scripts" }

// ResolveScript maps a `code run` argument to a file: "-" and anything that
// looks like a path (has a separator, ends in .js, or exists) stay as they
// are; a bare name is <skill dir>/scripts/<name>.js.
func ResolveScript(cfg *config.Config, arg string) (string, error) {
	if arg == "-" || strings.ContainsAny(arg, `/\`) || strings.HasSuffix(arg, ".js") {
		return arg, nil
	}
	if _, err := os.Stat(arg); err == nil {
		return arg, nil
	}
	if cfg == nil || cfg.Dir == "" {
		return "", errs.New(errs.NotFound, "no saved script %q (no skill config found)", arg).WithHint("tmvp --skill <dir> code run <name>")
	}
	p := ScriptsDir(cfg) + string(os.PathSeparator) + arg + ".js"
	if _, err := os.Stat(p); err != nil {
		names := []string{}
		for _, s := range Scripts(cfg) {
			names = append(names, s.Name)
		}
		h := "tmvp code scripts"
		if len(names) > 0 {
			h = "saved scripts: " + strings.Join(names, ", ")
		}
		return "", errs.New(errs.NotFound, "no saved script %q in %s", arg, ScriptsDir(cfg)).WithHint(h)
	}
	return p, nil
}

// Script is one saved script.
type Script struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Description string `json:"description"`
}

// Scripts lists <skill dir>/scripts/*.js; the description is the first
// line's `//` comment.
func Scripts(cfg *config.Config) []Script {
	dir := ScriptsDir(cfg)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Script
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		p := dir + string(os.PathSeparator) + e.Name()
		out = append(out, Script{Name: strings.TrimSuffix(e.Name(), ".js"), Path: p, Description: firstComment(p)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func firstComment(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(b), "\n")
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "//") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "//"))
}
