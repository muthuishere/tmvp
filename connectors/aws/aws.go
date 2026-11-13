// Package aws is the tmvp connector for AWS, driving the official `aws` CLI.
//
// Config fields (all optional; mode-aware: read.<key> / write.<key> win):
//
//	region         AWS_REGION / AWS_DEFAULT_REGION for every call
//	profile        AWS_PROFILE (ambient ~/.aws files, read only — tmvp never writes them)
//	access_key     ┐ static keys (secrets) → AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY /
//	secret_key     │ AWS_SESSION_TOKEN; the user's ~/.aws files are then hidden from the call
//	session_token  ┘
//	credentials    "${file:secrets.x}" shared-credentials file → AWS_SHARED_CREDENTIALS_FILE
//	config         "${file:secrets.x}" config file → AWS_CONFIG_FILE
//
// Every call gets its own environment; the user's global CLI state is never
// modified. Manual login (SSO) is declared in config, e.g.
//
//	login: { manual: "aws sso login --profile reqsume-prod", check: "aws sts get-caller-identity --profile reqsume-prod" }
//
// Verbs: whoami, logs <group> [pattern], call <service> <op> ... (read only
// for read operations), and the write verb `cli` (tmvp write <env> <sys> <service> <op> ...).
package aws

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/muthuishere/tmvp/connectors/internal/cli"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// DefaultLoginCheck is the suggested `login.check` for SSO setups.
const DefaultLoginCheck = "aws sts get-caller-identity"

func init() { connector.Register("aws", New) }

// New returns an unopened aws connector.
func New() connector.Connector { return &AWS{} }

// AWS runs the aws CLI for one env/system/mode.
type AWS struct {
	t   *connector.Target
	bin string
}

var verbs = []connector.Verb{
	{Name: "ls", Mode: connector.Read, Help: "ls — configured scope (profile, region), whoami, saved scripts"},
	{Name: "whoami", Mode: connector.Read, Help: "sts get-caller-identity (account, arn)"},
	{Name: "logs", Mode: connector.Read, Help: "logs <log-group> [filter-pattern] — CloudWatch, bounded by --since (≤7d) / --limit"},
	{Name: "call", Mode: connector.ReadWrite, Help: "call <service> <op> [args] — read-only ops; through tmvp write any op (--preview shows it)"},
	{Name: "cli", Mode: connector.Write, Default: true, Help: "alias of `call` for tmvp write (kept for compatibility)"},
}

func (a *AWS) Type() string            { return "aws" }
func (a *AWS) Verbs() []connector.Verb { return verbs }

// Policy classifies the aws operation (the word after the service).
var Policy = cli.Policy{
	ReadWords:    cli.Set("ls", "tail", "query", "scan", "wait", "help"),
	ReadPrefixes: []string{"describe-", "list-", "get-", "head-", "lookup-", "search-", "filter-", "batch-get-", "select-"},
	Secret: cli.Set("get-secret-value", "batch-get-secret-value", "get-login-password", "get-authorization-token",
		"get-session-token", "get-federation-token", "get-credentials-for-identity", "get-open-id-token",
		"get-open-id-token-for-developer-identity", "get-object", "get-object-torrent", "get-role-credentials",
		"get-cluster-credentials", "get-cluster-credentials-with-iam", "get-auth-token",
		// eks get-token, ec2 get-password-data, lightsail access details / db password,
		// lambda get-function (presigned code URL), cognito-identity get-id (step 1 of credential minting)
		"get-token", "get-password-data", "get-instance-access-details",
		"get-relational-database-master-user-password", "get-function", "get-id"),
}

var isolationFlags = []string{"--profile", "--endpoint-url", "--no-sign-request", "--ca-bundle"}

// Guard rejects mutating/secret-revealing calls through `call` before Open.
func (a *AWS) Guard(c connector.Call) error {
	c = cli.WriteCall(c)
	switch c.Verb {
	case "call", "cli":
		if len(c.Args) < 2 {
			return errs.New(errs.Usage, "need <service> <operation>").WithHint("call ec2 describe-instances")
		}
		if f := cli.HasFlag(c.Args, isolationFlags...); f != "" {
			return errs.New(errs.PolicyBlocked, "%s is set by tmvp from config; do not pass it", f)
		}
		if c.Verb == "call" {
			return readOnly(c.Args)
		}
		if err := noSecrets(c.Args); err != nil { // same rule on the write path as gcp/az
			return err
		}
	case "logs":
		if len(c.Args) == 0 {
			return errs.New(errs.Usage, "logs <log-group> [filter-pattern]")
		}
	}
	return nil
}

// noSecrets refuses operations that reveal secret material on any path.
func noSecrets(args []string) error {
	pos := cli.Positional(args, nil)
	if len(pos) < 2 {
		return nil
	}
	v, w := Policy.Classify(pos[1:2])
	if v == cli.RevealsSecret || (strings.HasPrefix(w, "get-parameter") && cli.HasFlag(args, "--with-decryption") != "") {
		return errs.New(errs.PolicyBlocked, "aws %s %s reveals secret material or writes local files; not allowed through tmvp", pos[0], w)
	}
	return nil
}

func readOnly(args []string) error {
	pos := cli.Positional(args, nil)
	if len(pos) < 2 {
		return errs.New(errs.Usage, "need <service> <operation>")
	}
	v, w := Policy.Classify(pos[1:2])
	if v == cli.ReadOnly && strings.HasPrefix(w, "get-parameter") && cli.HasFlag(args, "--with-decryption") != "" {
		v = cli.RevealsSecret
	}
	switch v {
	case cli.RevealsSecret:
		return errs.New(errs.PolicyBlocked, "aws %s %s reveals secret material or writes local files; not allowed through tmvp", pos[0], w)
	case cli.Mutating:
		return errs.New(errs.PolicyBlocked, "aws %s %s is not a read operation", pos[0], w).
			WithHint(`tmvp write <env> <system> call ` + pos[0] + " " + w + ` ... --reason "why"`)
	}
	return nil
}

// Open resolves the aws CLI.
func (a *AWS) Open(_ context.Context, t *connector.Target) error {
	a.t = t
	bin, err := cli.Find("aws")
	if err != nil {
		return err
	}
	a.bin = bin
	return nil
}

func (a *AWS) Close() error { return nil }

// Check implements connector.Checker.
func (a *AWS) Check(ctx context.Context) error {
	_, err := a.run(ctx, []string{"sts", "get-caller-identity", "--output", "json"})
	return err
}

// Do runs one verb.
func (a *AWS) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	c = cli.WriteCall(c)
	if _, ok := ctx.Value(timeoutKey{}).(time.Duration); !ok {
		ctx = context.WithValue(ctx, timeoutKey{}, connector.CallTimeout(c, a.t.Limits))
	}
	if err := a.Guard(c); err != nil {
		return err
	}
	switch c.Verb {
	case "ls":
		if err := e.Row(cli.ScopeRow(ctx, a.t, "profile", "region")); err != nil {
			return cli.IgnoreLimit(err)
		}
		if err := a.t.EmitSaved(e, "scripts", ""); err != nil {
			return errs.Wrap(errs.Runtime, err, "")
		}
		return a.Do(ctx, connector.Call{Verb: "whoami"}, e)
	case "whoami":
		out, err := a.run(ctx, []string{"sts", "get-caller-identity", "--output", "json"})
		if err != nil {
			return err
		}
		return cli.EmitJSON(e, out)
	case "logs":
		return a.logs(ctx, c, e)
	case "call":
		return a.call(ctx, c.Args, e)
	case "cli":
		if c.Preview {
			return cli.IgnoreLimit(e.Row(connector.Row{"preview": true, "would_run": "aws " + strings.Join(c.Args, " ")}))
		}
		return a.call(ctx, c.Args, e)
	}
	return errs.New(errs.Usage, "aws has no verb %q", c.Verb)
}

func (a *AWS) call(ctx context.Context, args []string, e connector.Emit) error {
	if cli.HasFlag(args, "--output") == "" {
		args = append(append([]string{}, args...), "--output", "json")
	}
	out, err := a.run(ctx, args)
	if err != nil {
		return err
	}
	return cli.EmitJSON(e, out)
}

func (a *AWS) logs(ctx context.Context, c connector.Call, e connector.Emit) error {
	since, err := cli.SinceOr(c, time.Hour, 7*24*time.Hour)
	if err != nil {
		return err
	}
	limit := cli.LimitOr(c, a.t.Limits.Lines)
	args := []string{"logs", "filter-log-events", "--log-group-name", c.Args[0],
		"--start-time", strconv.FormatInt(time.Now().Add(-since).UnixMilli(), 10),
		"--max-items", strconv.Itoa(limit), "--output", "json"}
	if len(c.Args) > 1 {
		args = append(args, "--filter-pattern", strings.Join(c.Args[1:], " "))
	}
	out, err := a.run(ctx, args)
	if err != nil {
		return err
	}
	var res struct {
		Events []struct {
			LogStreamName string `json:"logStreamName"`
			Timestamp     int64  `json:"timestamp"`
			Message       string `json:"message"`
		} `json:"events"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return errs.Wrap(errs.Runtime, err, "parse filter-log-events output")
	}
	for _, ev := range res.Events {
		r := connector.Row{"time": time.UnixMilli(ev.Timestamp).UTC().Format(time.RFC3339Nano),
			"stream": ev.LogStreamName, "message": strings.TrimRight(ev.Message, "\n")}
		if err := e.Row(r); err != nil {
			return cli.IgnoreLimit(err)
		}
	}
	return nil
}

// run executes the aws CLI with this target's isolated environment.
func (a *AWS) run(ctx context.Context, args []string) ([]byte, error) {
	env, unset, cleanup, err := a.env(ctx)
	defer cleanup()
	if err != nil {
		return nil, err
	}
	out, trunc, err := cli.Output(ctx, cli.Cmd{Path: a.bin, Args: args, Env: env, Unset: unset, Timeout: a.callTimeout(ctx)}, a.t.Limits.Bytes)
	if err != nil {
		return nil, err
	}
	if trunc {
		return nil, errs.New(errs.GuardRejected, "aws output exceeded %d bytes", a.t.Limits.Bytes).WithHint("narrow the call (--query, --max-items, filters)")
	}
	return out, nil
}

var credVars = []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE",
	"AWS_DEFAULT_PROFILE", "AWS_SHARED_CREDENTIALS_FILE", "AWS_CONFIG_FILE", "AWS_ROLE_ARN",
	"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI"}

// env builds the per-call environment. Explicit credentials hide every
// inherited credential source so the call cannot fall back to the user's.
func (a *AWS) env(ctx context.Context) (map[string]string, []string, func(), error) {
	t := a.t
	var cleanups []func()
	cleanup := func() {
		for _, f := range cleanups {
			f()
		}
	}
	env := map[string]string{"AWS_PAGER": "", "AWS_CLI_AUTO_PROMPT": "off", "AWS_EC2_METADATA_DISABLED": "true"}
	region, err := cli.Opt(ctx, t, "region")
	if err != nil {
		return nil, nil, cleanup, err
	}
	if region != "" {
		env["AWS_REGION"], env["AWS_DEFAULT_REGION"] = region, region
	}
	profile, err := cli.Opt(ctx, t, "profile")
	if err != nil {
		return nil, nil, cleanup, err
	}
	var unset []string
	explicit := cli.OptPath(t, "access_key") != "" || cli.OptPath(t, "credentials") != "" || cli.OptPath(t, "config") != ""
	if explicit {
		unset = credVars
		d, c, err := cli.TempDir("tmvp-aws-")
		cleanups = append(cleanups, c)
		if err != nil {
			return nil, nil, cleanup, err
		}
		// empty files so nothing is read from (or written to) ~/.aws
		for _, f := range []struct{ name, env string }{{"config", "AWS_CONFIG_FILE"}, {"credentials", "AWS_SHARED_CREDENTIALS_FILE"}} {
			p := d + string(pathSep) + f.name
			if err := writeEmpty(p); err != nil {
				return nil, nil, cleanup, err
			}
			env[f.env] = p
		}
	}
	if ak := cli.OptPath(t, "access_key"); ak != "" {
		for key, name := range map[string]string{"access_key": "AWS_ACCESS_KEY_ID", "secret_key": "AWS_SECRET_ACCESS_KEY", "session_token": "AWS_SESSION_TOKEN"} {
			v, err := cli.Opt(ctx, t, key)
			if err != nil {
				return nil, nil, cleanup, err
			}
			if v != "" {
				env[name] = v
			}
		}
		if env["AWS_SECRET_ACCESS_KEY"] == "" {
			return nil, nil, cleanup, errs.New(errs.Config, "%s needs secret_key with access_key", t.System)
		}
	}
	for key, name := range map[string]string{"credentials": "AWS_SHARED_CREDENTIALS_FILE", "config": "AWS_CONFIG_FILE"} {
		p := cli.OptPath(t, key)
		if p == "" {
			continue
		}
		fp, c, err := t.FilePath(ctx, p)
		if err != nil {
			return nil, nil, cleanup, err
		}
		cleanups = append(cleanups, c)
		env[name] = fp
	}
	if profile != "" {
		env["AWS_PROFILE"] = profile
		if !explicit {
			unset = []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"}
		}
	}
	return env, unset, cleanup, nil
}

// timeoutKey carries the call's effective timeout (connector.CallTimeout)
// from Do down to the CLI invocation.
type timeoutKey struct{}

// callTimeout is the CLI budget: the current call's effective timeout, else
// the target's configured one (Check and other call-less paths).
func (a *AWS) callTimeout(ctx context.Context) time.Duration {
	if v, ok := ctx.Value(timeoutKey{}).(time.Duration); ok {
		return v
	}
	return a.t.Limits.Timeout
}
