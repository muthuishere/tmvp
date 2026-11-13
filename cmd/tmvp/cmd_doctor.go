package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/muthuishere/tmvp/internal/client"
	"github.com/muthuishere/tmvp/internal/config"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
	"github.com/muthuishere/tmvp/internal/server"
)

// `tmvp doctor`: what is installed, what can I connect to, what is broken —
// in one screen. Works without a config (machine + tools + types only).
// Runs the probes in-process (never starts the server) and reports secret
// NAMES and states only, never values or the commands that produce them.

// doctorTool is an external binary some connector type (or a common secret
// source) relies on.
type doctorTool struct {
	Name     string
	Types    []string // connector types that need it
	Optional bool     // never a failure (kubectl: k8s uses client-go)
	Role     string
	Install  string // exact command when verified, else ""
}

var doctorTools = []doctorTool{
	{Name: "kubectl", Types: []string{"k8s"}, Optional: true, Role: "optional (k8s uses client-go)"},
	{Name: "aws", Types: []string{"aws"}, Role: "connector + secret source"},
	{Name: "gcloud", Types: []string{"gcp"}, Role: "connector + secret source"},
	{Name: "az", Types: []string{"az"}, Role: "connector"},
	{Name: "git", Types: []string{"git"}, Role: "connector"},
	// verified: `npm view @playwright/cli bin` → { 'playwright-cli': ... }
	{Name: "playwright-cli", Types: []string{"web"}, Role: "connector", Install: "npm i -g @playwright/cli"},
	{Name: "sec", Role: "secret source"},
	{Name: "op", Role: "secret source"},
	{Name: "lpass", Role: "secret source"},
	{Name: "vault", Role: "secret source"},
}

// types whose connector runs an external CLI (everything else is built-in).
// script runs the file's own interpreter (bash/python3/node …).
var toolTypes = map[string]string{"aws": "aws", "gcp": "gcloud", "az": "az", "git": "git", "web": "playwright-cli", "script": "interpreter"}

type doctorReport struct {
	exit  int // 0 ok, 1 failing, 2 config problem
	fixes []doctorFix
	fails int
	// health summary lines (secrets / certificates); "" = not checked
	secrets, certs string
}

type doctorFix struct {
	text string
	prio int // lower first: 0 config/install/login, 1 secrets, 2 connectivity
}

func (r *doctorReport) fail(code int, fix string, prio int) {
	r.fails++
	if code > r.exit {
		r.exit = code
	}
	if fix != "" {
		for _, f := range r.fixes {
			if f.text == fix {
				return
			}
		}
		r.fixes = append(r.fixes, doctorFix{fix, prio})
	}
}

// topFixes is the n most actionable fixes (stable by priority).
func (r *doctorReport) topFixes(n int) []string {
	fs := append([]doctorFix(nil), r.fixes...)
	sort.SliceStable(fs, func(i, j int) bool { return fs[i].prio < fs[j].prio })
	out := []string{}
	for i := 0; i < len(fs) && i < n; i++ {
		out = append(out, fs[i].text)
	}
	return out
}

func init() {
	addCommand(func() *cobra.Command {
		var offline bool
		var system string
		c := &cobra.Command{
			Use:   "doctor [--offline] [--env E] [--system S]",
			Short: "What is installed, what can I connect to, what is broken (works without a config)",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return runDoctor(cmd.Context(), offline, globals.Env, system) // --env/-e is the global env flag
			},
		}
		c.Flags().BoolVar(&offline, "offline", false, "skip secret commands, login checks and connectivity")
		c.Flags().StringVar(&system, "system", "", "only this system (name or glob)")
		return c
	})
}

func runDoctor(ctx context.Context, offline bool, env, system string) error {
	rep := &doctorReport{}
	// config first: which tools it needs decides what counts as failing
	cfg, cfgErr := loadConfig()
	noConfig := false
	if cfgErr != nil && globals.Config == "" && globals.Skill == "" &&
		strings.HasPrefix(errs.As(cfgErr).Message, "no tmvp config found") {
		cfg, cfgErr, noConfig = nil, nil, true
	}
	usedTypes := map[string]bool{}
	usedBins := map[string]bool{}
	if cfg != nil {
		for _, en := range cfg.EnvOrder {
			for _, s := range cfg.Envs[en].Systems {
				usedTypes[s.Type] = true
			}
		}
		for _, d := range cfg.AllSecrets() {
			if d.Command == "" {
				continue
			}
			if txt, err := cfg.CommandText(d); err == nil {
				if b := firstWord(txt); b != "" {
					usedBins[b] = true
				}
			}
		}
	}

	doctorMachine(ctx, rep)
	doctorToolsSection(ctx, rep, usedTypes, usedBins)
	doctorTypes()

	switch {
	case noConfig:
		section("config")
		line("config", map[string]any{"status": "none", "text": "no config found (machine checks only); pass --skill <dir> or --config <file>"},
			"  no config found — machine checks only (pass --skill <dir> or --config <file>)")
	case cfgErr != nil:
		section("config")
		ee := errs.As(cfgErr)
		line("config", map[string]any{"status": "error", "code": string(ee.Code), "message": ee.Message},
			"  error: "+ee.Error())
		rep.fail(2, "fix the config: tmvp validate", 0)
	default:
		doctorConfig(ctx, rep, cfg, offline, env, system)
	}

	// summary
	status := "all good"
	if rep.fails > 0 {
		status = fmt.Sprintf("%d problem(s)", rep.fails)
	}
	top := rep.topFixes(3)
	if globals.JSON {
		row := map[string]any{"section": "summary", "status": status, "exit": rep.exit, "fixes": top}
		if rep.secrets != "" {
			row["secrets"], row["certificates"] = rep.secrets, rep.certs
		}
		printFrame(protocol.Frame{Type: protocol.FrameRow, Row: row})
	} else {
		fmt.Fprintf(stdout, "\nsummary: %s (exit %d)\n", status, rep.exit)
		if rep.secrets != "" {
			fmt.Fprintf(stdout, "  secrets:      %s\n  certificates: %s\n", rep.secrets, rep.certs)
		}
		for i, f := range top {
			fmt.Fprintf(stdout, "  %d. %s\n", i+1, f)
		}
	}
	return exitCode(rep.exit)
}

func section(name string) {
	if !globals.JSON {
		fmt.Fprintf(stdout, "\n%s\n", strings.ToUpper(name))
	}
}

// line prints row as JSONL (with section) or text as-is.
func line(sec string, row map[string]any, text string) {
	if globals.JSON {
		r := map[string]any{"section": sec}
		for k, v := range row {
			r[k] = v
		}
		printFrame(protocol.Frame{Type: protocol.FrameRow, Row: r})
		return
	}
	fmt.Fprintln(stdout, text)
}

func doctorMachine(ctx context.Context, rep *doctorReport) {
	if !globals.JSON {
		fmt.Fprintln(stdout, "MACHINE")
	}
	kv := func(k string, v any, status string) {
		line("machine", map[string]any{"key": k, "value": v, "status": status}, fmt.Sprintf("  %-10s %v", k, v))
	}
	kv("tmvp", fmt.Sprintf("%s (proto %d)", version, protocol.Version), "ok")
	kv("os/arch", runtime.GOOS+"/"+runtime.GOARCH, "ok")
	home := protocol.Home()
	if os.Getenv("TMVP_HOME") != "" {
		kv("TMVP_HOME", home+" (from TMVP_HOME)", "ok")
	} else {
		kv("TMVP_HOME", home, "ok")
	}
	sock := protocol.SocketPath()
	if runtime.GOOS != "windows" && len(sock) > 103 {
		kv("socket", fmt.Sprintf("%s (%d bytes > 103: too long)", sock, len(sock)), "failed")
		rep.fail(1, "set TMVP_SOCKET (or TMVP_HOME) to a shorter path", 0)
	} else {
		kv("socket", fmt.Sprintf("%s (%d bytes, ok)", sock, len(sock)), "ok")
	}
	if client.InProcess() {
		kv("server", "in-process (TMVP_NO_SERVER set)", "ok")
		return
	}
	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var info map[string]any
	_, err := client.Do(sctx, protocol.Request{Op: protocol.OpStatus}, func(f protocol.Frame) {
		if f.Type == protocol.FrameInfo {
			info = f.Info
		}
	})
	if err != nil || info == nil {
		v := "not running (starts on the first command)"
		if err != nil && !errors.Is(err, client.ErrNotRunning) {
			v = "not reachable: " + errs.As(err).Message
		}
		kv("server", v, "info")
		return
	}
	var names []string
	if ps, ok := info["products"].([]any); ok {
		for _, p := range ps {
			if m, ok := p.(map[string]any); ok {
				names = append(names, fmt.Sprint(m["product"]))
			}
		}
	}
	sort.Strings(names)
	prods := strings.Join(names, ", ")
	if prods == "" {
		prods = "none"
	}
	pid := "?"
	if p, ok := info["pid"].(float64); ok {
		pid = fmt.Sprint(int(p))
	}
	if globals.JSON {
		printFrame(protocol.Frame{Type: protocol.FrameRow, Row: map[string]any{"section": "machine", "key": "server", "status": "ok",
			"value": "running", "pid": pid, "uptime": info["uptime"], "server_version": info["version"], "products": names}})
		return
	}
	fmt.Fprintf(stdout, "  %-10s running (pid %s, up %v, %v), products: %s\n", "server", pid, info["uptime"], info["version"], prods)
}

type toolResult struct {
	path, version string
}

func doctorToolsSection(ctx context.Context, rep *doctorReport, usedTypes, usedBins map[string]bool) {
	section("tools")
	res := make([]toolResult, len(doctorTools))
	var wg sync.WaitGroup
	for i, t := range doctorTools {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			p, err := exec.LookPath(name)
			if err != nil {
				return
			}
			args := []string{"--version"}
			if name == "kubectl" {
				args = []string{"version", "--client"} // kubectl has no --version
			}
			res[i] = toolResult{path: p, version: toolVersion(ctx, p, args...)}
		}(i, t.Name)
	}
	wg.Wait()
	if !globals.JSON {
		fmt.Fprintf(stdout, "  %-15s %-6s %-40s %s\n", "TOOL", "FOUND", "VERSION", "FOR")
	}
	known := map[string]bool{}
	for i, t := range doctorTools {
		known[t.Name] = true
		var used []string
		for _, ty := range t.Types {
			if usedTypes[ty] {
				used = append(used, "type "+ty)
			}
		}
		if usedBins[t.Name] {
			used = append(used, "secrets $(…)")
		}
		r := res[i]
		status := "ok"
		if r.path == "" {
			status = "missing"
			if len(used) > 0 && !t.Optional {
				status = "failed"
				fix := "install " + t.Name
				if t.Install != "" {
					fix += ": " + t.Install
				}
				rep.fail(1, fix, 0)
			}
		}
		for_ := t.Role
		if len(t.Types) > 0 {
			for_ = strings.Join(t.Types, ",") + " — " + t.Role
		}
		if len(used) > 0 {
			for_ += " [config: " + strings.Join(used, ", ") + "]"
		}
		found := "yes"
		if r.path == "" {
			found = "no"
			if status == "failed" {
				found = "NO"
			}
		}
		line("tools", map[string]any{"tool": t.Name, "found": r.path != "", "path": r.path, "version": r.version,
			"types": t.Types, "role": t.Role, "used_by_config": used, "status": status},
			fmt.Sprintf("  %-15s %-6s %-40s %s", t.Name, found, trunc(r.version, 40), for_))
	}
	// other binaries the config's $(…) commands run (names only)
	var extra []string
	for b := range usedBins {
		if !known[b] {
			extra = append(extra, b)
		}
	}
	sort.Strings(extra)
	for _, b := range extra {
		p, err := exec.LookPath(b)
		status, found := "ok", "yes"
		if err != nil {
			status, found = "failed", "NO"
			rep.fail(1, "install "+b+" (used by a secret $(…) command)", 0)
		}
		line("tools", map[string]any{"tool": b, "found": err == nil, "path": p, "role": "secret source", "used_by_config": []string{"secrets $(…)"}, "status": status},
			fmt.Sprintf("  %-15s %-6s %-40s %s", b, found, "", "secret source [config: secrets $(…)]"))
	}
	var builtin []string
	for _, ty := range connector.Types() {
		if _, ok := toolTypes[ty]; !ok {
			builtin = append(builtin, ty)
		}
	}
	line("tools", map[string]any{"tool": "built-in", "types": builtin, "status": "ok"},
		"  built-in, no tool needed: "+strings.Join(builtin, ", ")+" (ssh client built in)")
}

// toolVersion runs `<tool> --version` (5s, no stdin) and returns its first
// meaningful line (update nags like az's "WARNING: …" are skipped).
// toolProbeTimeout bounds each `--version` probe; tests raise it because a
// loaded CI machine can take seconds just to fork a process.
var toolProbeTimeout = 5 * time.Second

func toolVersion(ctx context.Context, path string, args ...string) string {
	vctx, cancel := context.WithTimeout(ctx, toolProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(vctx, path, args...)
	cmd.Stdin = nil
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if vctx.Err() != nil {
		return "(timed out)"
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" && !strings.HasPrefix(strings.ToUpper(l), "WARNING") {
			return strings.Join(strings.Fields(l), " ")
		}
	}
	if err != nil {
		return "(no version)"
	}
	return ""
}

func doctorTypes() {
	section("types")
	if !globals.JSON {
		fmt.Fprintf(stdout, "  %-12s %-5s %-6s %s\n", "TYPE", "READ", "WRITE", "NEEDS")
	}
	for _, ty := range connector.Types() {
		var r, w int
		for _, v := range connector.VerbsOf(ty) {
			if v.Mode.Readable() {
				r++
			}
			if v.Mode.Writable() {
				w++
			}
		}
		needs := toolTypes[ty]
		if needs == "" {
			needs = "built-in"
		}
		line("types", map[string]any{"type": ty, "read": r, "write": w, "needs": needs},
			fmt.Sprintf("  %-12s %-5d %-6d %s", ty, r, w, needs))
	}
}

func doctorConfig(ctx context.Context, rep *doctorReport, cfg *config.Config, offline bool, env, system string) {
	section("config")
	line("config", map[string]any{"path": cfg.Path, "product": cfg.Product, "status": "ok"},
		fmt.Sprintf("  %s (product %s)", cfg.Path, cfg.Product))
	for _, is := range cfg.Validate(server.ValidateOptions()) {
		line("config", map[string]any{"status": is.Level, "path": is.Path, "message": is.Message}, "  "+is.String())
		if is.Level == "error" {
			rep.fail(2, "fix the config: tmvp validate", 0)
		}
	}
	timeout := 10 * time.Second
	if d, err := time.ParseDuration(globals.Timeout); err == nil && d > 0 {
		timeout = d
	}
	eng := server.NewEngine()
	defer doctorHealth(ctx, rep, eng, cfg, offline, env, timeout)
	diags, err := eng.Diagnose(ctx, cfg.Path, server.DiagnoseOptions{Env: env, System: system, Offline: offline, Timeout: timeout})
	if err != nil {
		ee := errs.As(err)
		line("config", map[string]any{"status": "error", "code": string(ee.Code), "message": ee.Message}, "  error: "+ee.Error())
		rep.fail(errs.ExitCode(ee.Code), "", 0)
		return
	}
	section("systems")
	if !globals.JSON {
		fmt.Fprintf(stdout, "  %-22s %-11s %-30s %-12s %-18s %s\n", "ENV.SYSTEM", "TYPE", "SECRETS", "LOGIN", "VIA", "CONNECT")
	}
	for _, d := range diags {
		var secs []string
		for _, s := range d.Secrets {
			secs = append(secs, s.Name+"="+s.State)
		}
		connect := d.Connect
		if d.Error != nil {
			connect += " (" + string(d.Error.Code) + ": " + d.Error.Message + ")"
		}
		typ := d.Type
		if !d.TypeKnown {
			typ += " (unknown)"
		}
		login := d.Login
		if d.LoginCmd != "" && d.Login != "ok" {
			login += ": " + d.LoginCmd
		}
		via := d.Via
		if via == "" {
			via = "-"
		}
		sec := strings.Join(secs, ",")
		if sec == "" {
			sec = "-"
		}
		// effective per-call timeout (--timeout > system > env > global > 10s)
		to, toSrc, terr := cfg.EffectiveTimeout(d.Env, d.System, globals.Timeout)
		toText := to.String() + " (" + toSrc + ")"
		if terr != nil {
			toText = "invalid"
		}
		line("systems", map[string]any{"env": d.Env, "system": d.System, "type": d.Type, "type_known": d.TypeKnown,
			"timeout": to.String(), "timeout_source": toSrc,
			"secrets": d.Secrets, "login": d.Login, "login_cmd": d.LoginCmd, "via": d.Via, "connect": d.Connect, "error": d.Error,
			"status": map[bool]string{true: "failed", false: "ok"}[d.Failing()]},
			fmt.Sprintf("  %-22s %-11s %-30s %-12s %-18s %-14s %s", d.Env+"."+d.System, typ, sec, login, via, toText, connect))
		if !d.Failing() {
			continue
		}
		code, prio := 1, 0
		var fix string
		switch {
		case !d.TypeKnown:
			code, fix = 2, fmt.Sprintf("%s.%s: unknown type %q (tmvp verbs lists types)", d.Env, d.System, d.Type)
		case d.Login == "required":
			fix = "run: " + d.LoginCmd
		}
		if fix == "" {
			for _, s := range d.Secrets {
				if s.State != "ok" && s.State != "skipped" {
					prio = 1
					fix = fmt.Sprintf("secret %s (%s.%s) is %s: check its $(…) command / vault access, then tmvp secrets reload", s.Name, d.Env, d.System, s.State)
					break
				}
			}
		}
		if fix == "" && d.Error != nil {
			prio = 2
			fix = fmt.Sprintf("%s.%s: %s", d.Env, d.System, string(d.Error.Code))
			if d.Error.Hint != "" {
				fix += " — " + d.Error.Hint
			} else {
				fix += fmt.Sprintf(" — tmvp check %s %s", d.Env, d.System)
			}
		}
		// an unknown type is a config problem (2); anything else is "failing" (1)
		rep.fail(code, fix, prio)
	}
}

// firstWord is the program a secret command runs (base name only, no args).
func firstWord(cmd string) string {
	for _, f := range strings.Fields(cmd) {
		if strings.Contains(f, "=") && !strings.HasPrefix(f, "-") {
			continue // FOO=bar prefix
		}
		f = strings.Trim(f, `"'`)
		return filepath.Base(f)
	}
	return ""
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// doctorHealth fills the two health summary lines: secrets (ok / missing /
// expiring certs) and certificates expiring within 30 days, with fixes.
func doctorHealth(ctx context.Context, rep *doctorReport, eng *server.Engine, cfg *config.Config, offline bool, env string, timeout time.Duration) {
	if offline {
		rep.secrets, rep.certs = "not checked (--offline)", "not checked (--offline)"
		return
	}
	sr, err := eng.SecretsReport(ctx, cfg.Path, env, 30, timeout)
	if err != nil {
		rep.secrets = "error: " + errs.As(err).Message
	} else {
		var ok, bad, expiring, expired int
		for _, r := range sr {
			if r.State == "ok" {
				ok++
			} else {
				bad++
			}
			switch r.CertStatus {
			case "expiring":
				expiring++
			case "expired":
				expired++
				rep.fail(1, fmt.Sprintf("secret %s holds an EXPIRED certificate: rotate it, then tmvp secrets reload", r.Name), 1)
			}
		}
		rep.secrets = fmt.Sprintf("%d ok, %d missing/invalid, %d cert(s) expiring < 30d, %d expired (tmvp secrets status)", ok, bad, expiring, expired)
		if expiring > 0 {
			rep.fixes = append(rep.fixes, doctorFix{"renew certificates held in secrets expiring within 30 days: tmvp secrets status", 3})
		}
	}
	cr, err := certsSummary(ctx, cfg.Path, env, timeout)
	if err != nil {
		rep.certs = "error: " + errs.As(err).Message
		return
	}
	var expiring, expired, failed int
	for _, r := range cr {
		where := r.Env + "." + r.System
		switch r.Status {
		case "expiring":
			expiring++
			rep.fixes = append(rep.fixes, doctorFix{fmt.Sprintf("%s: TLS cert expires in %d days — renew it (tmvp certs)", where, r.Cert.DaysLeft), 3})
		case "expired":
			expired++
			rep.fail(1, fmt.Sprintf("%s: TLS cert EXPIRED — renew it (tmvp certs)", where), 1)
		case "error":
			failed++
		}
	}
	rep.certs = fmt.Sprintf("%d endpoint(s) checked, %d expiring < 30d, %d expired, %d unreachable (tmvp certs)", len(cr), expiring, expired, failed)
}
