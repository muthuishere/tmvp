// Package gcp is the tmvp connector for Google Cloud, driving the official
// `gcloud` CLI.
//
// Config fields (mode-aware: read.<key> / write.<key> win):
//
//	project      CLOUDSDK_CORE_PROJECT for every call
//	region       CLOUDSDK_COMPUTE_REGION
//	account      CLOUDSDK_CORE_ACCOUNT (pick one of the user's logged-in accounts)
//	credentials  "${file:secrets.gcp_sa}" service-account / external-account JSON →
//	             CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE + GOOGLE_APPLICATION_CREDENTIALS,
//	             with a fresh private CLOUDSDK_CONFIG per call (the user's gcloud state is never touched)
//
// Without credentials the user's own gcloud login is used (properties passed
// via env per call, never `gcloud config set`). Manual login:
//
//	login: { manual: "gcloud auth login", check: "gcloud auth print-access-token" }
//
// Verbs: whoami, logs [filter] (Cloud Logging), call <gcloud args> (read
// verbs only), and the write verb `cli`.
package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/muthuishere/tmvp/connectors/internal/cli"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// DefaultLoginCheck is the suggested `login.check`.
const DefaultLoginCheck = "gcloud auth print-access-token"

func init() { connector.Register("gcp", New) }

// New returns an unopened gcp connector.
func New() connector.Connector { return &GCP{} }

// GCP runs gcloud for one env/system/mode.
type GCP struct {
	t   *connector.Target
	bin string
}

var verbs = []connector.Verb{
	{Name: "ls", Mode: connector.Read, Help: "ls — configured scope (project, region, zone, account), whoami, saved scripts"},
	{Name: "whoami", Mode: connector.Read, Help: "active account and project"},
	{Name: "logs", Mode: connector.Read, Help: `logs ["<logging filter>"] — Cloud Logging, newest first, bounded by --since (≤7d) / --limit`},
	{Name: "call", Mode: connector.ReadWrite, Help: "call <gcloud args> — read verbs only (list/describe/read ...); through tmvp write any call (--preview shows it)"},
	{Name: "cli", Mode: connector.Write, Default: true, Help: "alias of `call` for tmvp write (kept for compatibility)"},
}

func (g *GCP) Type() string            { return "gcp" }
func (g *GCP) Verbs() []connector.Verb { return verbs }

// Policy: the first verb-like word of the command decides.
var Policy = cli.Policy{
	ReadWords:    cli.Set("list", "describe", "read", "get-iam-policy", "get-value", "list-grantable-roles", "search-all-resources", "search-all-iam-policies", "help", "info", "version"),
	ReadPrefixes: []string{"list-", "describe-", "get-"},
	WriteWords: cli.Set("create", "delete", "update", "deploy", "patch", "set", "unset", "add", "remove", "start", "stop",
		"reset", "resize", "restart", "import", "export", "apply", "cancel", "rollback", "submit", "execute", "enable", "disable",
		"activate", "revoke", "login", "attach", "detach", "migrate", "promote", "move", "replace", "rotate", "undelete", "suspend", "resume",
		"ssh", "scp", "connect", "copy", "cp", "mv", "rm", "rsync", "sign-blob", "sign-jwt"),
	Secret: cli.Set("print-access-token", "print-identity-token", "print-refresh-token", "access", "get-credentials",
		"application-default", "generate-login-token", "reset-windows-password"),
}

var isolationFlags = []string{"--account", "--configuration", "--impersonate-service-account", "--access-token-file", "--credential-file-override"}

// Guard rejects non-read calls through `call` and isolation-breaking flags.
func (g *GCP) Guard(c connector.Call) error {
	c = cli.WriteCall(c)
	if c.Verb != "call" && c.Verb != "cli" {
		return nil
	}
	pos := cli.Positional(c.Args, boolFlags)
	if len(pos) == 0 {
		return errs.New(errs.Usage, "need gcloud arguments").WithHint("call compute instances list")
	}
	if f := cli.HasFlag(c.Args, isolationFlags...); f != "" {
		return errs.New(errs.PolicyBlocked, "%s is set by tmvp from config; do not pass it", f)
	}
	v, w := Policy.Classify(pos)
	if mintsKey(pos) {
		v, w = cli.RevealsSecret, "keys create"
	}
	if v == cli.RevealsSecret {
		return errs.New(errs.PolicyBlocked, "gcloud ... %s reveals secret material or writes local credentials; not allowed through tmvp", w)
	}
	if c.Verb == "cli" {
		if (pos[0] == "auth" || pos[0] == "config") && v != cli.ReadOnly {
			return errs.New(errs.PolicyBlocked, "gcloud %s %s would change gcloud's own state; tmvp sets project/account per call from config", pos[0], w)
		}
		return nil
	}
	if v != cli.ReadOnly {
		return errs.New(errs.PolicyBlocked, "gcloud %s is not a read call", strings.Join(pos, " ")).
			WithHint(`tmvp write <env> <system> call ` + strings.Join(c.Args, " ") + ` --reason "why"`)
	}
	return nil
}

// mintsKey reports `... keys create` / `... hmac-keys create` (e.g. iam
// service-accounts keys create, storage hmac create): the response holds a new private key.
func mintsKey(pos []string) bool {
	for i := 0; i+1 < len(pos); i++ {
		if (pos[i] == "keys" || pos[i] == "hmac-keys" || pos[i] == "hmac") && pos[i+1] == "create" {
			return true
		}
	}
	return false
}

var boolFlags = cli.Set("--quiet", "-q", "--verbose", "--log-http", "--no-user-output-enabled", "--all", "--recursive")

// Open resolves the gcloud CLI.
func (g *GCP) Open(_ context.Context, t *connector.Target) error {
	g.t = t
	bin, err := cli.Find("gcloud")
	if err != nil {
		return err
	}
	g.bin = bin
	return nil
}

func (g *GCP) Close() error { return nil }

// Check implements connector.Checker.
func (g *GCP) Check(ctx context.Context) error {
	_, err := g.run(ctx, []string{"projects", "list", "--limit=1", "--format=json"})
	return err
}

// Do runs one verb.
func (g *GCP) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	c = cli.WriteCall(c)
	if _, ok := ctx.Value(timeoutKey{}).(time.Duration); !ok {
		ctx = context.WithValue(ctx, timeoutKey{}, connector.CallTimeout(c, g.t.Limits))
	}
	if err := g.Guard(c); err != nil {
		return err
	}
	switch c.Verb {
	case "ls":
		if err := e.Row(cli.ScopeRow(ctx, g.t, "project", "region", "zone", "account")); err != nil {
			return cli.IgnoreLimit(err)
		}
		if err := g.t.EmitSaved(e, "scripts", ""); err != nil {
			return errs.Wrap(errs.Runtime, err, "")
		}
		return g.Do(ctx, connector.Call{Verb: "whoami"}, e)
	case "whoami":
		return g.whoami(ctx, e)
	case "logs":
		return g.logs(ctx, c, e)
	case "call":
		return g.call(ctx, c.Args, e)
	case "cli":
		if c.Preview {
			return cli.IgnoreLimit(e.Row(connector.Row{"preview": true, "would_run": "gcloud " + strings.Join(c.Args, " ")}))
		}
		return g.call(ctx, c.Args, e)
	}
	return errs.New(errs.Usage, "gcp has no verb %q", c.Verb)
}

func (g *GCP) call(ctx context.Context, args []string, e connector.Emit) error {
	if cli.HasFlag(args, "--format") == "" {
		args = append(append([]string{}, args...), "--format=json")
	}
	out, err := g.run(ctx, args)
	if err != nil {
		return err
	}
	return cli.EmitJSON(e, out)
}

func (g *GCP) whoami(ctx context.Context, e connector.Emit) error {
	out, err := g.run(ctx, []string{"config", "list", "--format=json"})
	if err != nil {
		return err
	}
	var cfg struct {
		Core map[string]any `json:"core"`
	}
	_ = json.Unmarshal(out, &cfg)
	row := connector.Row{"account": cfg.Core["account"], "project": cfg.Core["project"]}
	if p := cli.OptPath(g.t, "credentials"); p != "" {
		if b, err := g.t.File(ctx, p); err == nil {
			var sa struct {
				Type        string `json:"type"`
				ClientEmail string `json:"client_email"`
			}
			if json.Unmarshal(b, &sa) == nil {
				if sa.ClientEmail != "" {
					row["account"] = sa.ClientEmail
				}
				row["credential_type"] = sa.Type
			}
		}
	}
	return cli.IgnoreLimit(e.Row(row))
}

func (g *GCP) logs(ctx context.Context, c connector.Call, e connector.Emit) error {
	since, err := cli.SinceOr(c, time.Hour, 7*24*time.Hour)
	if err != nil {
		return err
	}
	limit := cli.LimitOr(c, g.t.Limits.Lines)
	args := []string{"logging", "read"}
	if len(c.Args) > 0 {
		args = append(args, strings.Join(c.Args, " "))
	}
	args = append(args, "--freshness="+strconv.Itoa(int(since.Seconds()))+"s", "--limit="+strconv.Itoa(limit), "--order=desc", "--format=json")
	out, err := g.run(ctx, args)
	if err != nil {
		return err
	}
	var entries []map[string]any
	if err := json.Unmarshal(out, &entries); err != nil {
		return errs.Wrap(errs.Runtime, err, "parse gcloud logging output")
	}
	for _, en := range entries {
		if err := e.Row(LogRow(en)); err != nil {
			return cli.IgnoreLimit(err)
		}
	}
	return nil
}

// LogRow flattens one Cloud Logging entry.
func LogRow(en map[string]any) connector.Row {
	r := connector.Row{"time": en["timestamp"], "severity": en["severity"]}
	if ln, ok := en["logName"].(string); ok {
		if i := strings.LastIndex(ln, "/logs/"); i >= 0 {
			ln = ln[i+6:]
		}
		r["log"] = ln
	}
	if res, ok := en["resource"].(map[string]any); ok {
		r["resource"] = res["type"]
	}
	switch {
	case en["textPayload"] != nil:
		r["message"] = en["textPayload"]
	case en["jsonPayload"] != nil:
		if m, ok := en["jsonPayload"].(map[string]any); ok && m["message"] != nil {
			r["message"] = m["message"]
		} else {
			b, _ := json.Marshal(en["jsonPayload"])
			r["message"] = truncate(string(b), 2000)
		}
	case en["protoPayload"] != nil:
		if m, ok := en["protoPayload"].(map[string]any); ok {
			r["message"] = fmt.Sprintf("%v %v", m["methodName"], m["resourceName"])
		}
	}
	return r
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// run executes gcloud with this target's isolated environment.
func (g *GCP) run(ctx context.Context, args []string) ([]byte, error) {
	env, unset, cleanup, err := g.env(ctx)
	defer cleanup()
	if err != nil {
		return nil, err
	}
	out, trunc, err := cli.Output(ctx, cli.Cmd{Path: g.bin, Args: args, Env: env, Unset: unset, Timeout: g.callTimeout(ctx)}, g.t.Limits.Bytes)
	if err != nil {
		return nil, err
	}
	if trunc {
		return nil, errs.New(errs.GuardRejected, "gcloud output exceeded %d bytes", g.t.Limits.Bytes).WithHint("narrow the call (--filter, --limit)")
	}
	return out, nil
}

func (g *GCP) env(ctx context.Context) (map[string]string, []string, func(), error) {
	t := g.t
	var cleanups []func()
	cleanup := func() {
		for _, f := range cleanups {
			f()
		}
	}
	env := map[string]string{"CLOUDSDK_CORE_DISABLE_PROMPTS": "1", "CLOUDSDK_CORE_DISABLE_USAGE_REPORTING": "1"}
	for key, name := range map[string]string{"project": "CLOUDSDK_CORE_PROJECT", "region": "CLOUDSDK_COMPUTE_REGION", "account": "CLOUDSDK_CORE_ACCOUNT"} {
		v, err := cli.Opt(ctx, t, key)
		if err != nil {
			return nil, nil, cleanup, err
		}
		if v != "" {
			env[name] = v
		}
	}
	unset := []string{"CLOUDSDK_AUTH_ACCESS_TOKEN_FILE", "CLOUDSDK_AUTH_IMPERSONATE_SERVICE_ACCOUNT"}
	if p := cli.OptPath(t, "credentials"); p != "" {
		dir, c, err := cli.TempDir("tmvp-gcloud-")
		cleanups = append(cleanups, c)
		if err != nil {
			return nil, nil, cleanup, err
		}
		fp, c2, err := t.FilePath(ctx, p)
		if err != nil {
			return nil, nil, cleanup, err
		}
		cleanups = append([]func(){c2}, cleanups...)
		env["CLOUDSDK_CONFIG"] = dir
		env["CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE"] = fp
		env["GOOGLE_APPLICATION_CREDENTIALS"] = fp
		unset = append(unset, "CLOUDSDK_CORE_ACCOUNT", "CLOUDSDK_ACTIVE_CONFIG_NAME")
		delete(env, "CLOUDSDK_CORE_ACCOUNT")
	}
	return env, unset, cleanup, nil
}

// timeoutKey carries the call's effective timeout (connector.CallTimeout)
// from Do down to the CLI invocation.
type timeoutKey struct{}

// callTimeout is the CLI budget: the current call's effective timeout, else
// the target's configured one (Check and other call-less paths).
func (g *GCP) callTimeout(ctx context.Context) time.Duration {
	if v, ok := ctx.Value(timeoutKey{}).(time.Duration); ok {
		return v
	}
	return g.t.Limits.Timeout
}
