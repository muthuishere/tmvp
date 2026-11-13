// Package az is the tmvp connector for Azure, driving the official `az` CLI.
//
// Config fields (mode-aware: read.<key> / write.<key> win):
//
//	subscription   passed as --subscription per call (never `az account set` on the user's state)
//	tenant         ┐ service principal: each call runs in a fresh private AZURE_CONFIG_DIR,
//	client_id      │ logs in there with client_secret (secret) or certificate
//	client_secret  │ ("${file:secrets.x}" PEM), then runs the command; the user's ~/.azure is
//	certificate    ┘ never touched
//	workspace      Log Analytics workspace id for `logs "<kql>"`
//
// Without a service principal the user's own `az login` is used. Manual login:
//
//	login: { manual: "az login", check: "az account show" }
//
// Verbs: whoami, logs ["<kql>"] (no query → activity log), call <az args>
// (read commands only), and the write verb `cli`.
package az

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/muthuishere/tmvp/internal/secure"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/muthuishere/tmvp/connectors/internal/cli"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// DefaultLoginCheck is the suggested `login.check`.
const DefaultLoginCheck = "az account show"

func init() { connector.Register("az", New) }

// New returns an unopened az connector.
func New() connector.Connector { return &AZ{} }

// AZ runs az for one env/system/mode.
type AZ struct {
	t   *connector.Target
	bin string
}

var verbs = []connector.Verb{
	{Name: "ls", Mode: connector.Read, Help: "ls — configured scope (subscription, resource_group, workspace, tenant), whoami, saved scripts"},
	{Name: "whoami", Mode: connector.Read, Help: "az account show (subscription, tenant, user)"},
	{Name: "logs", Mode: connector.Read, Help: `logs ["<kql>"] — Log Analytics (workspace:) or, without a query, the activity log; bounded by --since (≤7d) / --limit`},
	{Name: "call", Mode: connector.ReadWrite, Help: "call <az args> — read commands only (list/show/query ...); through tmvp write any call (--preview shows it)"},
	{Name: "cli", Mode: connector.Write, Default: true, Help: "alias of `call` for tmvp write (kept for compatibility)"},
}

func (a *AZ) Type() string            { return "az" }
func (a *AZ) Verbs() []connector.Verb { return verbs }

// Policy: the first verb-like word decides.
var Policy = cli.Policy{
	ReadWords:    cli.Set("list", "show", "query", "get", "tail", "help", "version", "exists", "check-name"),
	ReadPrefixes: []string{"list-", "show-", "get-", "check-"},
	WriteWords: cli.Set("create", "delete", "update", "set", "add", "remove", "start", "stop", "restart", "deallocate",
		"scale", "deploy", "import", "export", "upload", "download", "purge", "restore", "regenerate", "renew", "reset",
		"run-command", "invoke", "login", "logout", "assign", "unassign", "swap", "move", "copy", "sync", "lock", "unlock", "up"),
	Secret: cli.Set("get-access-token", "get-credentials", "keys", "list-keys", "secret", "list-connection-strings",
		"show-connection-string", "list-publishing-credentials", "list-publishing-profiles", "list-secrets",
		"generate-sas", "list-credentials", "get-upgrade-profile-credentials",
		"credential", "list-auth-keys", "list-access-keys", "list-admin-credentials", "list-user-credentials"),
}

// credentialMetadataReads are the "credential" subcommands that return only
// metadata (key id, display name, start/end dates), never a secret value:
// `az ad app credential list` / `az ad sp credential list`. Everything else
// under "credential" (reset, create, delete, `acr credential show`, which
// prints registry passwords) stays RevealsSecret.
var credentialMetadataReads = map[string]bool{"ad app": true, "ad sp": true}

// classify is Policy.Classify plus the narrow credential-metadata exception.
// Deny-by-default holds: only `ad app|sp credential list` is let through.
func classify(pos []string) (cli.Verdict, string) {
	v, w := Policy.Classify(pos)
	if v == cli.RevealsSecret && strings.EqualFold(w, "credential") && len(pos) == 4 &&
		credentialMetadataReads[strings.ToLower(pos[0]+" "+pos[1])] &&
		strings.EqualFold(pos[2], "credential") && strings.EqualFold(pos[3], "list") {
		return cli.ReadOnly, "list"
	}
	return v, w
}

var isolationFlags = []string{"--subscription"}

// Guard rejects non-read calls through `call`.
func (a *AZ) Guard(c connector.Call) error {
	c = cli.WriteCall(c)
	if c.Verb != "call" && c.Verb != "cli" {
		return nil
	}
	pos := cli.Positional(c.Args, boolFlags)
	if len(pos) == 0 {
		return errs.New(errs.Usage, "need az arguments").WithHint("call vm list")
	}
	if f := cli.HasFlag(c.Args, isolationFlags...); f != "" {
		return errs.New(errs.PolicyBlocked, "%s is set by tmvp from config; do not pass it", f)
	}
	v, w := classify(pos)
	if v == cli.RevealsSecret {
		return errs.New(errs.PolicyBlocked, "az ... %s reveals secret material or writes local credentials; not allowed through tmvp", w)
	}
	if c.Verb == "cli" {
		if pos[0] == "login" || pos[0] == "logout" || (pos[0] == "account" && v != cli.ReadOnly) || pos[0] == "config" {
			return errs.New(errs.PolicyBlocked, "az %s would change az's own state; tmvp sets subscription/credentials per call", pos[0])
		}
		return nil
	}
	if v != cli.ReadOnly {
		return errs.New(errs.PolicyBlocked, "az %s is not a read call", strings.Join(pos, " ")).
			WithHint(`tmvp write <env> <system> call ` + strings.Join(c.Args, " ") + ` --reason "why"`)
	}
	return nil
}

var boolFlags = cli.Set("--only-show-errors", "--debug", "--verbose", "--yes", "-y", "--all", "--no-wait", "--show-details", "-d")

// Open resolves the az CLI.
func (a *AZ) Open(_ context.Context, t *connector.Target) error {
	a.t = t
	bin, err := cli.Find("az")
	if err != nil {
		return err
	}
	a.bin = bin
	return nil
}

func (a *AZ) Close() error { return nil }

// Check implements connector.Checker.
func (a *AZ) Check(ctx context.Context) error {
	_, err := a.run(ctx, []string{"account", "show", "-o", "json"}, true)
	return err
}

// Do runs one verb.
func (a *AZ) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	c = cli.WriteCall(c)
	if _, ok := ctx.Value(timeoutKey{}).(time.Duration); !ok {
		ctx = context.WithValue(ctx, timeoutKey{}, connector.CallTimeout(c, a.t.Limits))
	}
	if err := a.Guard(c); err != nil {
		return err
	}
	switch c.Verb {
	case "ls":
		if err := e.Row(cli.ScopeRow(ctx, a.t, "subscription", "resource_group", "workspace", "tenant")); err != nil {
			return cli.IgnoreLimit(err)
		}
		if err := a.t.EmitSaved(e, "scripts", ""); err != nil {
			return errs.Wrap(errs.Runtime, err, "")
		}
		return a.Do(ctx, connector.Call{Verb: "whoami"}, e)
	case "whoami":
		out, err := a.run(ctx, []string{"account", "show", "-o", "json"}, true)
		if err != nil {
			return err
		}
		var acc struct {
			Name     string `json:"name"`
			ID       string `json:"id"`
			TenantID string `json:"tenantId"`
			User     struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"user"`
		}
		if err := json.Unmarshal(out, &acc); err != nil {
			return errs.Wrap(errs.Runtime, err, "parse az account show")
		}
		return cli.IgnoreLimit(e.Row(connector.Row{"subscription": acc.Name, "subscription_id": acc.ID,
			"tenant_id": acc.TenantID, "user": acc.User.Name, "user_type": acc.User.Type}))
	case "logs":
		return a.logs(ctx, c, e)
	case "call":
		return a.call(ctx, c.Args, e)
	case "cli":
		if c.Preview {
			return cli.IgnoreLimit(e.Row(connector.Row{"preview": true, "would_run": "az " + strings.Join(c.Args, " ")}))
		}
		return a.call(ctx, c.Args, e)
	}
	return errs.New(errs.Usage, "az has no verb %q", c.Verb)
}

func (a *AZ) call(ctx context.Context, args []string, e connector.Emit) error {
	if cli.HasFlag(args, "--output", "-o") == "" {
		args = append(append([]string{}, args...), "-o", "json")
	}
	out, err := a.run(ctx, args, true)
	if err != nil {
		return err
	}
	return cli.EmitJSON(e, out)
}

func (a *AZ) logs(ctx context.Context, c connector.Call, e connector.Emit) error {
	since, err := cli.SinceOr(c, time.Hour, 7*24*time.Hour)
	if err != nil {
		return err
	}
	limit := cli.LimitOr(c, a.t.Limits.Lines)
	if len(c.Args) == 0 {
		args := []string{"monitor", "activity-log", "list", "--offset", fmt.Sprintf("%dh", max(1, int((since+time.Hour-1)/time.Hour))),
			"--max-events", strconv.Itoa(limit), "-o", "json"}
		out, err := a.run(ctx, args, true)
		if err != nil {
			return err
		}
		var evs []map[string]any
		if err := json.Unmarshal(out, &evs); err != nil {
			return errs.Wrap(errs.Runtime, err, "parse activity log")
		}
		for _, ev := range evs {
			if err := e.Row(activityRow(ev)); err != nil {
				return cli.IgnoreLimit(err)
			}
		}
		return nil
	}
	ws := c.Flags["workspace"]
	if ws == "" {
		if ws, err = cli.Opt(ctx, a.t, "workspace"); err != nil {
			return err
		}
	}
	if ws == "" {
		return errs.New(errs.Config, "logs with a KQL query needs workspace: on the system (or -F workspace=<id>)")
	}
	q := strings.Join(c.Args, " ") + fmt.Sprintf(" | take %d", limit)
	args := []string{"monitor", "log-analytics", "query", "-w", ws, "--analytics-query", q,
		"--timespan", fmt.Sprintf("PT%dS", int(since.Seconds())), "-o", "json"}
	out, err := a.run(ctx, args, false)
	if err != nil {
		return err
	}
	return cli.EmitJSON(e, out)
}

func activityRow(ev map[string]any) connector.Row {
	val := func(k string) any {
		if m, ok := ev[k].(map[string]any); ok {
			if v, ok := m["localizedValue"]; ok {
				return v
			}
			return m["value"]
		}
		return ev[k]
	}
	return connector.Row{"time": ev["eventTimestamp"], "level": ev["level"], "operation": val("operationName"),
		"status": val("status"), "resource": ev["resourceId"], "caller": ev["caller"]}
}

// run executes az with this target's environment. withSub appends
// --subscription when configured.
func (a *AZ) run(ctx context.Context, args []string, withSub bool) ([]byte, error) {
	env, cleanup, err := a.env(ctx)
	defer cleanup()
	if err != nil {
		return nil, err
	}
	if withSub {
		sub, err := cli.Opt(ctx, a.t, "subscription")
		if err != nil {
			return nil, err
		}
		if sub != "" {
			args = append(append([]string{}, args...), "--subscription", sub)
		}
	}
	out, trunc, err := cli.Output(ctx, cli.Cmd{Path: a.bin, Args: args, Env: env, Timeout: a.callTimeout(ctx)}, a.t.Limits.Bytes)
	if err != nil {
		return nil, err
	}
	if trunc {
		return nil, errs.New(errs.GuardRejected, "az output exceeded %d bytes", a.t.Limits.Bytes).WithHint("narrow the call (--query, filters)")
	}
	return out, nil
}

// env builds the per-call environment; with a service principal it logs in
// inside a private AZURE_CONFIG_DIR that is deleted after the call.
func (a *AZ) env(ctx context.Context) (map[string]string, func(), error) {
	t := a.t
	var cleanups []func()
	cleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	env := map[string]string{"AZURE_CORE_NO_COLOR": "1", "AZURE_CORE_COLLECT_TELEMETRY": "0",
		"AZURE_CORE_ONLY_SHOW_ERRORS": "1", "AZURE_CORE_SURVEY_MESSAGE": "0"}
	clientID, err := cli.Opt(ctx, t, "client_id")
	if err != nil || clientID == "" {
		return env, cleanup, err
	}
	tenant, err := cli.Opt(ctx, t, "tenant")
	if err != nil {
		return nil, cleanup, err
	}
	if tenant == "" {
		return nil, cleanup, errs.New(errs.Config, "%s: client_id needs tenant", t.System)
	}
	dir, c, err := cli.TempDir("tmvp-az-")
	cleanups = append(cleanups, c)
	if err != nil {
		return nil, cleanup, err
	}
	env["AZURE_CONFIG_DIR"] = dir
	login := []string{"login", "--service-principal", "--username", clientID, "--tenant", tenant, "--allow-no-subscriptions", "--output", "none"}
	if p := cli.OptPath(t, "certificate"); p != "" {
		fp, c2, err := t.FilePath(ctx, p)
		if err != nil {
			return nil, cleanup, err
		}
		cleanups = append(cleanups, c2)
		login = append(login, "--certificate", fp)
	} else {
		secret, err := cli.Opt(ctx, t, "client_secret")
		if err != nil {
			return nil, cleanup, err
		}
		if secret == "" {
			return nil, cleanup, errs.New(errs.Config, "%s: client_id needs client_secret or certificate", t.System)
		}
		// Never on argv (visible in ps): az expands "--password=@<file>"
		// itself, so the secret goes into a 0600 file inside the private
		// 0700 AZURE_CONFIG_DIR, removed with it after the call.
		pf := filepath.Join(dir, "sp-secret")
		if err := secure.WriteFile(pf, []byte(secret)); err != nil {
			return nil, cleanup, errs.Wrap(errs.Runtime, err, "az: secret file")
		}
		login = append(login, "--password=@"+pf)
		defer os.Remove(pf) // only needed for the login itself
	}
	if _, _, err := cli.Output(ctx, cli.Cmd{Path: a.bin, Args: login, Env: env, Timeout: a.callTimeout(ctx)}, 64<<10); err != nil {
		if errs.CodeOf(err) == errs.Runtime {
			return nil, cleanup, errs.AuthError(fmt.Errorf("az service-principal login failed"))
		}
		return nil, cleanup, err
	}
	return env, cleanup, nil
}

// timeoutKey carries the call's effective timeout (connector.CallTimeout)
// from Do down to the CLI invocation.
type timeoutKey struct{}

// callTimeout is the CLI budget: the current call's effective timeout, else
// the target's configured one (Check and other call-less paths).
func (a *AZ) callTimeout(ctx context.Context) time.Duration {
	if v, ok := ctx.Value(timeoutKey{}).(time.Duration); ok {
		return v
	}
	return a.t.Limits.Timeout
}
