// Package gh is the tmvp connector for GitHub, driving the official `gh` CLI.
//
// Config fields (mode-aware: read.<key> / write.<key> win):
//
//	repo    owner/name (required); tmvp passes -R / GH_REPO itself
//	host    GitHub Enterprise host (GH_HOST); default github.com
//	token   optional secret; sent only as GH_TOKEN in the child's env, never argv
//
// Without token the user's own `gh auth login` is used. Manual login:
//
//	login: { manual: "gh auth login", check: "gh auth status" }
//
// Reads: ls, whoami, runs, run, run-log, workflows, prs, pr, releases,
// deployments, branches, call <gh args> (list/view/status/diff/checks, api GET).
// Writes (tmvp write only): rerun, dispatch, cancel, cli <gh args>.
package gh

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/muthuishere/tmvp/connectors/internal/cli"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// DefaultLoginCheck is the suggested `login.check`.
const DefaultLoginCheck = "gh auth status"

func init() { connector.Register("gh", New) }

// New returns an unopened gh connector.
func New() connector.Connector { return &GH{} }

// GH runs gh for one env/system/mode.
type GH struct {
	t   *connector.Target
	bin string
}

var verbs = []connector.Verb{
	{Name: "ls", Mode: connector.Read, Help: "ls — configured repo/host, whoami, saved scripts"},
	{Name: "whoami", Mode: connector.Read, Help: "the GitHub account gh is using (login, name)"},
	{Name: "runs", Mode: connector.Read, Help: "runs [--workflow W] [--branch B] [--status S] — recent workflow runs (bounded by --limit)"},
	{Name: "run", Mode: connector.Read, Help: "run <id> — one run and its jobs (status, conclusion, times)"},
	{Name: "run-log", Mode: connector.Read, Help: "run-log <id> [--failed] — the run's log (--failed: failed steps only), line-capped"},
	{Name: "workflows", Mode: connector.Read, Help: "workflows — the repo's workflows (name, path, state)"},
	{Name: "prs", Mode: connector.Read, Help: "prs [--state open|closed|merged|all] — pull requests"},
	{Name: "pr", Mode: connector.Read, Help: "pr <n> — one pull request with checks and review state"},
	{Name: "releases", Mode: connector.Read, Help: "releases — tags, names, latest/draft/prerelease, dates"},
	{Name: "deployments", Mode: connector.Read, Help: "deployments [--environment E] — GitHub deployments (environment, ref, sha, creator, time)"},
	{Name: "branches", Mode: connector.Read, Help: "branches — branch names, head sha, protected"},
	{Name: "call", Mode: connector.ReadWrite, Help: "call <gh args> — reads only (run/pr/issue/release/workflow/label/cache list|view|status|diff|checks, api GET); through tmvp write any allowed call"},
	{Name: "rerun", Mode: connector.Write, Help: "rerun <run-id> [--failed] — re-run a workflow run (tmvp write; --preview shows it)"},
	{Name: "dispatch", Mode: connector.Write, Help: "dispatch <workflow> [--ref R] [-f k=v ...] — trigger a workflow_dispatch run (tmvp write)"},
	{Name: "cancel", Mode: connector.Write, Help: "cancel <run-id> — cancel a running workflow run (tmvp write)"},
	{Name: "cli", Mode: connector.Write, Default: true, Help: "cli <gh args> — any gh call that is not blocked (tmvp write; --preview shows it)"},
}

func (g *GH) Type() string            { return "gh" }
func (g *GH) Verbs() []connector.Verb { return verbs }

// repoGroups take -R; tmvp adds it (api uses GH_REPO / explicit paths).
var repoGroups = cli.Set("run", "pr", "issue", "release", "workflow", "label", "cache", "variable", "secret", "repo")

// readGroups / readSubs: `<group> <sub>` that only reads.
var (
	readGroups = cli.Set("run", "pr", "issue", "release", "workflow", "label", "cache", "variable", "secret", "repo", "search", "ruleset", "project")
	readSubs   = cli.Set("list", "view", "status", "diff", "checks")
)

// blocked groups never run through tmvp: they reveal credentials or change
// gh's own state on this machine.
var blocked = cli.Set("auth", "config", "ssh-key", "gpg-key", "extension", "codespace", "alias", "gist", "attestation", "completion")

// boolFlags never take a value (for positional parsing).
var boolFlags = cli.Set("--failed", "--log", "--log-failed", "--web", "-w", "--paginate", "--slurp", "-i", "--include",
	"--verbose", "--exit-status", "--draft", "-d", "--latest", "--all", "-a", "--yes", "-y", "--force")

// postFlags make `gh api` send a body (implicit POST).
var postFlags = []string{"-f", "-F", "--field", "--raw-field", "--input"}

// verdict classifies gh args: ok to read, a write, or blocked.
func verdict(args []string) (cli.Verdict, string) {
	pos := cli.Positional(args, boolFlags)
	if len(pos) == 0 {
		return cli.Mutating, ""
	}
	g := strings.ToLower(pos[0])
	if blocked[g] {
		return cli.RevealsSecret, g
	}
	if g == "secret" && (len(pos) < 2 || pos[1] != "list") {
		return cli.RevealsSecret, "secret " + strings.Join(pos[1:], " ")
	}
	if g == "run" && len(pos) > 1 && pos[1] == "download" {
		return cli.RevealsSecret, "run download (writes local files)"
	}
	if g == "repo" && len(pos) > 1 && (pos[1] == "delete" || pos[1] == "clone" || pos[1] == "fork" || pos[1] == "sync") {
		return cli.RevealsSecret, "repo " + pos[1]
	}
	if g == "api" {
		m := strings.ToUpper(flagValue(args, "-X", "--method"))
		if m == "" {
			m = "GET"
			if cli.HasFlag(args, postFlags...) != "" {
				m = "POST"
			}
		}
		if m == "GET" {
			return cli.ReadOnly, "api GET"
		}
		return cli.Mutating, "api " + m
	}
	if readGroups[g] && len(pos) > 1 && readSubs[strings.ToLower(pos[1])] {
		return cli.ReadOnly, g + " " + pos[1]
	}
	return cli.Mutating, strings.Join(pos, " ")
}

// flagValue returns the value of the first of names in args ("--k v" or "--k=v").
func flagValue(args []string, names ...string) string {
	for i, a := range args {
		for _, n := range names {
			if a == n && i+1 < len(args) {
				return args[i+1]
			}
			if strings.HasPrefix(a, n+"=") {
				return a[len(n)+1:]
			}
		}
	}
	return ""
}

// Guard enforces the policy for call/cli and the shape of other verbs.
func (g *GH) Guard(c connector.Call) error {
	c = cli.WriteCall(c)
	if c.Verb != "call" && c.Verb != "cli" {
		return nil
	}
	if len(c.Args) == 0 {
		return errs.New(errs.Usage, "need gh arguments").WithHint("call run list")
	}
	if f := cli.HasFlag(c.Args, "-R", "--repo", "--hostname"); f != "" {
		return errs.New(errs.PolicyBlocked, "%s is set by tmvp from config (repo/host); do not pass it", f)
	}
	if p := cli.Positional(c.Args, boolFlags); len(p) > 2 && strings.EqualFold(p[0], "repo") {
		return errs.New(errs.PolicyBlocked, "gh repo %s %s: the repo is set by tmvp from config; do not name one", p[1], p[2])
	}
	v, w := verdict(c.Args)
	if v == cli.RevealsSecret {
		return errs.New(errs.PolicyBlocked, "gh %s reveals secrets or changes gh's local state; not allowed through tmvp", w)
	}
	if c.Verb == "call" && v != cli.ReadOnly {
		return errs.New(errs.PolicyBlocked, "gh %s is not a read call", w).
			WithHint(`tmvp write <env> <system> call ` + strings.Join(c.Args, " ") + ` --reason "why"`)
	}
	return nil
}

// Open resolves the gh CLI and checks repo.
func (g *GH) Open(ctx context.Context, t *connector.Target) error {
	g.t = t
	repo, err := cli.Opt(ctx, t, "repo")
	if err != nil {
		return err
	}
	if strings.Count(repo, "/") != 1 || strings.HasPrefix(repo, "/") || strings.HasSuffix(repo, "/") || strings.ContainsAny(repo, " \t\n") {
		return errs.New(errs.Config, "%s: repo must be owner/name", t.System)
	}
	bin, err := cli.Find("gh")
	if err != nil {
		return err
	}
	g.bin = bin
	return nil
}

func (g *GH) Close() error { return nil }

// Check implements connector.Checker: the repo is visible with these credentials.
func (g *GH) Check(ctx context.Context) error {
	_, err := g.run(ctx, []string{"repo", "view", "--json", "name"})
	return err
}

// Do runs one verb.
func (g *GH) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	c = cli.WriteCall(c)
	if _, ok := ctx.Value(timeoutKey{}).(time.Duration); !ok {
		ctx = context.WithValue(ctx, timeoutKey{}, connector.CallTimeout(c, g.t.Limits))
	}
	if err := g.Guard(c); err != nil {
		return err
	}
	for _, v := range verbs {
		if v.Name == c.Verb && v.Mode == connector.Write && g.t.Mode != connector.ModeWrite {
			return errs.New(errs.PolicyBlocked, "%s needs tmvp write", c.Verb).WithHint(`tmvp write <env> <system> ` + c.Verb + ` … --reason "why"`)
		}
	}
	limit := strconv.Itoa(cli.LimitOr(c, 20))
	opt := func(k string) string {
		if v := c.Flags[k]; v != "" {
			return v
		}
		return flagValue(c.Args, "--"+k)
	}
	arg := func(what string) (string, error) {
		for _, a := range cli.Positional(c.Args, boolFlags) {
			return a, nil
		}
		return "", errs.New(errs.Usage, "%s needs <%s>", c.Verb, what)
	}
	switch c.Verb {
	case "ls":
		if err := e.Row(cli.ScopeRow(ctx, g.t, "repo", "host")); err != nil {
			return cli.IgnoreLimit(err)
		}
		if err := g.t.EmitSaved(e, "scripts", ""); err != nil {
			return errs.Wrap(errs.Runtime, err, "")
		}
		return g.Do(ctx, connector.Call{Verb: "whoami"}, e)
	case "whoami":
		out, err := g.run(ctx, []string{"api", "user"})
		if err != nil {
			return err
		}
		var u struct{ Login, Name string }
		if err := json.Unmarshal(out, &u); err != nil {
			return errs.Wrap(errs.Runtime, err, "parse gh api user")
		}
		return cli.IgnoreLimit(e.Row(connector.Row{"login": u.Login, "name": u.Name}))
	case "runs":
		a := []string{"run", "list", "-L", limit, "--json", "databaseId,displayTitle,workflowName,headBranch,headSha,event,status,conclusion,createdAt,updatedAt,url"}
		for _, k := range []string{"workflow", "branch", "status", "event", "commit"} {
			if v := opt(k); v != "" {
				a = append(a, "--"+k, v)
			}
		}
		return g.emit(ctx, a, e)
	case "run":
		id, err := arg("run-id")
		if err != nil {
			return err
		}
		out, err := g.run(ctx, []string{"run", "view", id, "--json", "databaseId,displayTitle,workflowName,headBranch,headSha,event,status,conclusion,createdAt,updatedAt,url,jobs"})
		if err != nil {
			return err
		}
		var r map[string]any
		if err := json.Unmarshal(out, &r); err != nil {
			return errs.Wrap(errs.Runtime, err, "parse gh run view")
		}
		jobs, _ := r["jobs"].([]any)
		delete(r, "jobs")
		r["kind"] = "run"
		if err := e.Row(r); err != nil {
			return cli.IgnoreLimit(err)
		}
		for _, j := range jobs {
			m, _ := j.(map[string]any)
			if m == nil {
				continue
			}
			row := connector.Row{"kind": "job", "name": m["name"], "status": m["status"], "conclusion": m["conclusion"],
				"started_at": m["startedAt"], "completed_at": m["completedAt"], "url": m["url"]}
			if steps, ok := m["steps"].([]any); ok {
				var failed []string
				for _, s := range steps {
					if sm, ok := s.(map[string]any); ok && sm["conclusion"] == "failure" {
						failed = append(failed, fmtAny(sm["name"]))
					}
				}
				if len(failed) > 0 {
					row["failed_steps"] = strings.Join(failed, "; ")
				}
			}
			if err := e.Row(row); err != nil {
				return cli.IgnoreLimit(err)
			}
		}
		return nil
	case "run-log":
		id, err := arg("run-id")
		if err != nil {
			return err
		}
		mode := "--log"
		if c.Flags["failed"] == "true" || cli.HasFlag(c.Args, "--failed") != "" {
			mode = "--log-failed"
		}
		cmd, cleanup, err := g.cmd(ctx, g.withRepo([]string{"run", "view", id, mode}))
		defer cleanup()
		if err != nil {
			return err
		}
		return g.mapErr(cli.Lines(ctx, cmd, e, cli.LimitOr(c, g.t.Limits.Lines), g.t.Limits.Bytes))
	case "workflows":
		return g.emit(ctx, []string{"workflow", "list", "--all", "-L", limit, "--json", "id,name,path,state"}, e)
	case "prs":
		st := opt("state")
		if st == "" {
			st = "open"
		}
		return g.emit(ctx, []string{"pr", "list", "--state", st, "-L", limit, "--json", "number,title,author,headRefName,baseRefName,state,isDraft,updatedAt,url"}, e)
	case "pr":
		n, err := arg("number")
		if err != nil {
			return err
		}
		return g.emit(ctx, []string{"pr", "view", n, "--json", "number,title,state,author,headRefName,baseRefName,mergeable,reviewDecision,statusCheckRollup,updatedAt,url"}, e)
	case "releases":
		return g.emit(ctx, []string{"release", "list", "-L", limit, "--json", "tagName,name,isLatest,isDraft,isPrerelease,publishedAt"}, e)
	case "deployments":
		q := url.Values{"per_page": {limit}}
		if v := opt("environment"); v != "" {
			q.Set("environment", v)
		}
		out, err := g.run(ctx, []string{"api", "repos/{owner}/{repo}/deployments?" + q.Encode()})
		if err != nil {
			return err
		}
		var ds []map[string]any
		if err := json.Unmarshal(out, &ds); err != nil {
			return errs.Wrap(errs.Runtime, err, "parse deployments")
		}
		for _, d := range ds {
			creator, _ := d["creator"].(map[string]any)
			if err := e.Row(connector.Row{"id": d["id"], "environment": d["environment"], "ref": d["ref"], "sha": d["sha"],
				"task": d["task"], "creator": creator["login"], "created_at": d["created_at"], "description": d["description"]}); err != nil {
				return cli.IgnoreLimit(err)
			}
		}
		return nil
	case "branches":
		out, err := g.run(ctx, []string{"api", "repos/{owner}/{repo}/branches?per_page=" + limit})
		if err != nil {
			return err
		}
		var bs []struct {
			Name      string `json:"name"`
			Protected bool   `json:"protected"`
			Commit    struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		if err := json.Unmarshal(out, &bs); err != nil {
			return errs.Wrap(errs.Runtime, err, "parse branches")
		}
		for _, b := range bs {
			if err := e.Row(connector.Row{"name": b.Name, "sha": b.Commit.SHA, "protected": b.Protected}); err != nil {
				return cli.IgnoreLimit(err)
			}
		}
		return nil
	case "call":
		return g.emit(ctx, c.Args, e)
	case "rerun", "cancel":
		id, err := arg("run-id")
		if err != nil {
			return err
		}
		a := []string{"run", c.Verb, id}
		if c.Verb == "rerun" && (c.Flags["failed"] == "true" || cli.HasFlag(c.Args, "--failed") != "") {
			a = append(a, "--failed")
		}
		return g.write(ctx, c, a, e)
	case "dispatch":
		wf, err := arg("workflow")
		if err != nil {
			return err
		}
		a := []string{"workflow", "run", wf}
		if r := opt("ref"); r != "" {
			a = append(a, "--ref", r)
		}
		for i := 0; i < len(c.Args); i++ {
			if (c.Args[i] == "-f" || c.Args[i] == "--field" || c.Args[i] == "-F" || c.Args[i] == "--raw-field") && i+1 < len(c.Args) {
				a = append(a, "-f", c.Args[i+1])
				i++
			}
		}
		return g.write(ctx, c, a, e)
	case "cli":
		return g.write(ctx, c, c.Args, e)
	}
	return errs.New(errs.Usage, "gh has no verb %q", c.Verb)
}

func fmtAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// write runs (or, with --preview, shows) a mutating gh call.
func (g *GH) write(ctx context.Context, c connector.Call, args []string, e connector.Emit) error {
	if v, w := verdict(args); v == cli.RevealsSecret {
		return errs.New(errs.PolicyBlocked, "gh %s is not allowed through tmvp", w)
	}
	full := g.withRepo(args)
	if c.Preview {
		return cli.IgnoreLimit(e.Row(connector.Row{"preview": true, "would_run": "gh " + strings.Join(full, " "), "note": "not executed"}))
	}
	out, err := g.run(ctx, args)
	if err != nil {
		return err
	}
	return cli.EmitJSON(e, out)
}

func (g *GH) emit(ctx context.Context, args []string, e connector.Emit) error {
	out, err := g.run(ctx, args)
	if err != nil {
		return err
	}
	return cli.EmitJSON(e, out)
}

// withRepo appends -R <repo> for commands that take it.
func (g *GH) withRepo(args []string) []string {
	pos := cli.Positional(args, boolFlags)
	if len(pos) == 0 || !repoGroups[strings.ToLower(pos[0])] {
		return args
	}
	repo, _ := cli.Opt(context.Background(), g.t, "repo")
	if strings.EqualFold(pos[0], "repo") {
		// `gh repo <sub>` takes the repo as a positional, not -R
		if len(pos) != 2 {
			return args // Guard refuses a user-named repo before we get here
		}
		for i, a := range args {
			if a == pos[1] {
				return append(append(append([]string{}, args[:i+1]...), repo), args[i+1:]...)
			}
		}
	}
	return append(append([]string{}, args...), "-R", repo)
}

// cmd builds the gh invocation with this target's environment.
func (g *GH) cmd(ctx context.Context, args []string) (cli.Cmd, func(), error) {
	env := map[string]string{"GH_PROMPT_DISABLED": "1", "NO_COLOR": "1", "GH_NO_UPDATE_NOTIFIER": "1",
		"GH_PAGER": "cat", "PAGER": "cat", "GH_SPINNER_DISABLED": "1", "CLICOLOR": "0"}
	repo, err := cli.Opt(ctx, g.t, "repo")
	if err != nil {
		return cli.Cmd{}, func() {}, err
	}
	env["GH_REPO"] = repo
	host, err := cli.Opt(ctx, g.t, "host")
	if err != nil {
		return cli.Cmd{}, func() {}, err
	}
	if host != "" {
		env["GH_HOST"] = host
		env["GH_REPO"] = host + "/" + repo
	}
	tok, err := cli.Opt(ctx, g.t, "token")
	if err != nil {
		return cli.Cmd{}, func() {}, err
	}
	var unset []string
	if tok != "" {
		if host != "" && host != "github.com" {
			env["GH_ENTERPRISE_TOKEN"] = tok
		} else {
			env["GH_TOKEN"] = tok
		}
	} else {
		unset = []string{"GH_TOKEN", "GITHUB_TOKEN"} // the user's gh login, not a stray env token
	}
	return cli.Cmd{Path: g.bin, Args: args, Env: env, Unset: unset, Timeout: g.callTimeout(ctx)}, func() {}, nil
}

// run executes gh (adding -R where the command takes it) and returns stdout.
func (g *GH) run(ctx context.Context, args []string) ([]byte, error) {
	cmd, cleanup, err := g.cmd(ctx, g.withRepo(args))
	defer cleanup()
	if err != nil {
		return nil, err
	}
	out, trunc, err := cli.Output(ctx, cmd, g.t.Limits.Bytes)
	if err != nil {
		return nil, g.mapErr(err)
	}
	if trunc {
		return nil, errs.New(errs.GuardRejected, "gh output exceeded %d bytes", g.t.Limits.Bytes).WithHint("narrow the call (--limit, filters)")
	}
	return out, nil
}

// mapErr turns "not logged in" into login_required.
func (g *GH) mapErr(err error) error {
	if err == nil {
		return nil
	}
	m := strings.ToLower(err.Error())
	if strings.Contains(m, "gh auth login") || strings.Contains(m, "not logged into") || strings.Contains(m, "no oauth token") {
		return errs.New(errs.LoginRequired, "gh is not logged in").WithHint("gh auth login")
	}
	return err
}

type timeoutKey struct{}

func (g *GH) callTimeout(ctx context.Context) time.Duration {
	if v, ok := ctx.Value(timeoutKey{}).(time.Duration); ok {
		return v
	}
	return g.t.Limits.Timeout
}
