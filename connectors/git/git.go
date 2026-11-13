// Package git is the tmvp connector for git repositories (read only; tmvp
// never pushes). It keeps a bare clone per env/system under
// ~/.tmvp/<product>/git/<system>/<env> and fetches it at most once a minute.
//
// Config fields:
//
//	url     repository URL (ssh or https)
//	branch  branch used by log/deploys when no rev is given (default: remote HEAD)
//	key     "${file:secrets.x}" ssh private key → GIT_SSH_COMMAND -i <per-call file>
//	token   https token (secret) → Authorization header via GIT_CONFIG_* env, never argv
//	user    username for token auth (default x-access-token)
//
// The user's global/system git config is ignored for every call.
//
// Verbs: log, diff, show, tags, deploys, fetch.
package git

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/muthuishere/tmvp/connectors/internal/cli"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
	"github.com/muthuishere/tmvp/internal/secure"
)

func init() { connector.Register("git", New) }

// New returns an unopened git connector.
func New() connector.Connector { return &Git{} }

// Git is one cached repository.
type Git struct {
	t       *connector.Target
	bin     string
	dir     string
	url     string
	mu      sync.Mutex
	fetched time.Time
}

// FetchEvery is the minimum interval between automatic fetches.
var FetchEvery = time.Minute

var verbs = []connector.Verb{
	{Name: "ls", Mode: connector.Read, Help: "ls — branches and tags (kind, name, commit, date), newest first, --limit"},
	{Name: "log", Mode: connector.Read, Help: "log [rev] [-- path] — commits (hash, author, date, subject), --since / --limit"},
	{Name: "diff", Mode: connector.Read, Help: "diff [a] [b] [-- path] — default HEAD~1 HEAD; bounded text"},
	{Name: "show", Mode: connector.Read, Help: "show <rev>[:path] — commit or file at a revision; bounded text"},
	{Name: "tags", Mode: connector.Read, Help: "tags [pattern] — newest first"},
	{Name: "deploys", Mode: connector.Read, Help: "deploys [tag-pattern] — tags and commits since --since (default 7d)"},
	{Name: "fetch", Mode: connector.Read, Help: "fetch — refresh the local cache now"},
}

func (g *Git) Type() string            { return "git" }
func (g *Git) Verbs() []connector.Verb { return verbs }

// Guard refuses option-looking arguments (git options can write files, e.g. --output).
func (g *Git) Guard(c connector.Call) error {
	for i, a := range c.Args {
		if a == "--" {
			return nil
		}
		if strings.HasPrefix(a, "-") {
			return errs.New(errs.Usage, "argument %d looks like a git option (%q); options are not passed through", i+1, a).
				WithHint("pass revisions and paths only; use --since / --limit for bounds")
		}
	}
	return nil
}

// Open clones (or reuses) the cache.
func (g *Git) Open(ctx context.Context, t *connector.Target) error {
	g.t = t
	bin, err := cli.Find("git")
	if err != nil {
		return err
	}
	g.bin = bin
	if g.url, err = t.String(ctx, "url"); err != nil {
		return err
	}
	g.dir = filepath.Join(protocol.Home(), t.Product, "git", t.System, t.Env)
	if _, err := os.Stat(filepath.Join(g.dir, "HEAD")); err == nil {
		return nil
	}
	if err := secure.MkdirAll(filepath.Dir(g.dir)); err != nil {
		return errs.Wrap(errs.Runtime, err, "git cache dir")
	}
	tmp := g.dir + ".tmp" + strconv.Itoa(os.Getpid())
	_ = os.RemoveAll(tmp)
	if _, err := g.git(ctx, true, "", "clone", "--bare", "--quiet", "--", g.url, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	for _, kv := range [][2]string{
		{"remote.origin.fetch", "+refs/heads/*:refs/heads/*"},
		{"remote.origin.pushurl", "tmvp-never-pushes"},
		{"core.hooksPath", noHooksPath()},
	} {
		if _, err := g.git(ctx, false, tmp, "config", kv[0], kv[1]); err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, g.dir); err != nil {
		_ = os.RemoveAll(tmp)
		if _, statErr := os.Stat(filepath.Join(g.dir, "HEAD")); statErr != nil {
			return errs.Wrap(errs.Runtime, err, "git cache")
		}
	}
	g.fetched = time.Now()
	return nil
}

func (g *Git) Close() error { return nil }

// Check implements connector.Checker.
func (g *Git) Check(ctx context.Context) error { return g.fetch(ctx, true) }

func (g *Git) fetch(ctx context.Context, force bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !force && time.Since(g.fetched) < FetchEvery {
		return nil
	}
	if _, err := g.git(ctx, true, g.dir, "fetch", "--quiet", "--prune", "--tags", "--force", "origin"); err != nil {
		return err
	}
	g.fetched = time.Now()
	return nil
}

// Do runs one verb.
func (g *Git) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	ctx = context.WithValue(ctx, timeoutKey{}, connector.CallTimeout(c, g.t.Limits))
	if err := g.Guard(c); err != nil {
		return err
	}
	if err := g.fetch(ctx, c.Verb == "fetch"); err != nil {
		return err
	}
	switch c.Verb {
	case "fetch":
		return cli.IgnoreLimit(e.Row(connector.Row{"ok": true, "fetched_at": g.fetched.UTC().Format(time.RFC3339)}))
	case "ls":
		return g.ls(ctx, c, e)
	case "log":
		return g.log(ctx, c, e)
	case "diff":
		args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color"}
		revs, paths := splitPaths(c.Args)
		if len(revs) == 0 {
			revs = []string{"HEAD~1", "HEAD"}
		}
		args = append(append(args, revs...), "--")
		return g.lines(ctx, append(args, paths...), e)
	case "show":
		if len(c.Args) == 0 {
			c.Args = []string{"HEAD"}
		}
		revs, paths := splitPaths(c.Args)
		args := append([]string{"show", "--no-ext-diff", "--no-textconv", "--no-color", "--stat", "--patch"}, revs...)
		return g.lines(ctx, append(append(args, "--"), paths...), e)
	case "tags":
		return g.tags(ctx, c, e, 0)
	case "deploys":
		return g.deploys(ctx, c, e)
	}
	return errs.New(errs.Usage, "git has no verb %q", c.Verb)
}

func splitPaths(a []string) (revs, paths []string) {
	for i, x := range a {
		if x == "--" {
			return a[:i], a[i+1:]
		}
	}
	return a, nil
}

const sep = "\x1f"

var logFormat = "--format=%H" + "%x1f%an%x1f%aI%x1f%s"

// ls lists branches (local and remote-tracking) and tags, newest first.
func (g *Git) ls(ctx context.Context, c connector.Call, e connector.Emit) error {
	out, err := g.git(ctx, false, g.dir, "for-each-ref", "--sort=-creatordate", "--count="+strconv.Itoa(cli.LimitOr(c, g.t.Limits.Rows)),
		"--format=%(refname)%1f%(objectname:short)%1f%(creatordate:iso-strict)", "refs/heads", "refs/remotes", "refs/tags")
	if err != nil {
		return err
	}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.SplitN(l, sep, 3)
		if len(f) < 3 {
			continue
		}
		kind, name := "branch", f[0]
		switch {
		case strings.HasPrefix(name, "refs/tags/"):
			kind, name = "tag", strings.TrimPrefix(name, "refs/tags/")
		case strings.HasPrefix(name, "refs/remotes/"):
			name = strings.TrimPrefix(name, "refs/remotes/")
			if strings.HasSuffix(name, "/HEAD") {
				continue
			}
		default:
			name = strings.TrimPrefix(name, "refs/heads/")
		}
		if err := e.Row(connector.Row{"kind": kind, "name": name, "commit": f[1], "date": f[2]}); err != nil {
			return cli.IgnoreLimit(err)
		}
	}
	return nil
}

func (g *Git) log(ctx context.Context, c connector.Call, e connector.Emit) error {
	revs, paths := splitPaths(c.Args)
	args := []string{"log", "--no-color", logFormat, "-n", strconv.Itoa(cli.LimitOr(c, g.t.Limits.Rows))}
	if c.Since > 0 {
		args = append(args, "--since="+time.Now().Add(-c.Since).UTC().Format(time.RFC3339))
	}
	if len(revs) == 0 {
		revs = g.defaultRev(ctx)
	}
	args = append(append(append(args, revs...), "--"), paths...)
	out, err := g.git(ctx, false, g.dir, args...)
	if err != nil {
		return err
	}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.SplitN(l, sep, 4)
		if len(f) < 4 {
			continue
		}
		if err := e.Row(connector.Row{"commit": f[0], "author": f[1], "date": f[2], "subject": f[3]}); err != nil {
			return cli.IgnoreLimit(err)
		}
	}
	return nil
}

func (g *Git) defaultRev(ctx context.Context) []string {
	if b, _ := g.t.StringOr(ctx, "branch", ""); b != "" {
		return []string{b}
	}
	return []string{"HEAD"}
}

var tagFormat = "--format=%(refname:short)%1f%(objectname)%1f%(*objectname)%1f%(creatordate:iso-strict)%1f%(subject)"

func (g *Git) tags(ctx context.Context, c connector.Call, e connector.Emit, since time.Duration) error {
	args := []string{"for-each-ref", "--sort=-creatordate", tagFormat, "--count=" + strconv.Itoa(cli.LimitOr(c, g.t.Limits.Rows))}
	if len(c.Args) > 0 {
		args = append(args, "refs/tags/"+c.Args[0])
	} else {
		args = append(args, "refs/tags")
	}
	out, err := g.git(ctx, false, g.dir, args...)
	if err != nil {
		return err
	}
	cut := time.Time{}
	if since > 0 {
		cut = time.Now().Add(-since)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.SplitN(l, sep, 5)
		if len(f) < 5 {
			continue
		}
		commit := f[2]
		if commit == "" {
			commit = f[1]
		}
		if !cut.IsZero() {
			if ts, err := time.Parse(time.RFC3339, f[3]); err == nil && ts.Before(cut) {
				break // sorted newest first
			}
		}
		r := connector.Row{"tag": f[0], "commit": commit, "date": f[3], "subject": f[4]}
		if since > 0 {
			r = connector.Row{"kind": "tag", "ref": f[0], "commit": commit, "date": f[3], "subject": f[4]}
		}
		if err := e.Row(r); err != nil {
			return cli.IgnoreLimit(err)
		}
	}
	return nil
}

// deploys lists tags created within --since, then commits on the branch.
func (g *Git) deploys(ctx context.Context, c connector.Call, e connector.Emit) error {
	since := c.Since
	if since <= 0 {
		since = 7 * 24 * time.Hour
	}
	stop := false
	wrapped := connector.EmitFuncs{
		RowFn:  func(r connector.Row) error { err := e.Row(r); stop = stop || err != nil; return err },
		TextFn: e.Text,
	}
	if err := g.tags(ctx, c, wrapped, since); err != nil || stop {
		return err
	}
	rev := g.defaultRev(ctx)
	args := append([]string{"log", "--no-color", "--first-parent", logFormat, "-n", strconv.Itoa(cli.LimitOr(c, g.t.Limits.Rows)),
		"--since=" + time.Now().Add(-since).UTC().Format(time.RFC3339)}, rev...)
	out, err := g.git(ctx, false, g.dir, append(args, "--")...)
	if err != nil {
		return err
	}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.SplitN(l, sep, 4)
		if len(f) < 4 {
			continue
		}
		if err := e.Row(connector.Row{"kind": "commit", "ref": rev[0], "commit": f[0], "date": f[2], "subject": f[3], "author": f[1]}); err != nil {
			return cli.IgnoreLimit(err)
		}
	}
	return nil
}

func (g *Git) lines(ctx context.Context, args []string, e connector.Emit) error {
	env, cleanup, err := g.env(ctx, false)
	defer cleanup()
	if err != nil {
		return err
	}
	return cli.Lines(ctx, cli.Cmd{Path: g.bin, Args: append([]string{"--git-dir", g.dir}, args...), Env: env, Timeout: g.callTimeout(ctx)},
		e, g.t.Limits.Lines, g.t.Limits.Bytes)
}

// git runs one git command. remote=true attaches credentials.
func (g *Git) git(ctx context.Context, remote bool, gitDir string, args ...string) ([]byte, error) {
	env, cleanup, err := g.env(ctx, remote)
	defer cleanup()
	if err != nil {
		return nil, err
	}
	if gitDir != "" {
		args = append([]string{"--git-dir", gitDir}, args...)
	}
	timeout := g.callTimeout(ctx)
	if remote && timeout < 5*time.Minute {
		timeout = 5 * time.Minute // first clone of a big repo
	}
	out, trunc, err := cli.Output(ctx, cli.Cmd{Path: g.bin, Args: args, Env: env, Timeout: timeout}, g.t.Limits.Bytes)
	if err != nil {
		return nil, scrub(err, g.url)
	}
	if trunc {
		return nil, errs.New(errs.GuardRejected, "git output exceeded %d bytes", g.t.Limits.Bytes).WithHint("narrow with --limit / --since / a path")
	}
	return out, nil
}

var userinfo = regexp.MustCompile(`://[^/@\s]+@`)

// scrub removes the URL's userinfo from error text.
func scrub(err error, url string) error {
	e := errs.As(err)
	e.Message = userinfo.ReplaceAllString(e.Message, "://***@")
	if u := userinfo.FindString(url); u != "" {
		e.Message = strings.ReplaceAll(e.Message, u, "://***@")
	}
	e.Err = nil
	return e
}

func (g *Git) env(ctx context.Context, remote bool) (map[string]string, func(), error) {
	var cleanups []func()
	cleanup := func() {
		for _, f := range cleanups {
			f()
		}
	}
	env := map[string]string{
		"GIT_TERMINAL_PROMPT": "0", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": emptyConfigPath(),
		"GIT_ASKPASS": "", "SSH_ASKPASS": "", "GIT_PAGER": "cat", "PAGER": "cat", "LC_ALL": "C",
	}
	if !remote {
		return env, cleanup, nil
	}
	n := 0
	cfg := func(k, v string) {
		env["GIT_CONFIG_KEY_"+strconv.Itoa(n)] = k
		env["GIT_CONFIG_VALUE_"+strconv.Itoa(n)] = v
		n++
		env["GIT_CONFIG_COUNT"] = strconv.Itoa(n)
	}
	sshCmd := "ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=" +
		shellQuote(filepath.Join(protocol.Home(), g.t.Product, "git", "known_hosts"))
	if g.t.Has("key") {
		p, c, err := g.t.FilePath(ctx, "key")
		if err != nil {
			return nil, cleanup, err
		}
		cleanups = append(cleanups, c)
		sshCmd += " -o IdentitiesOnly=yes -i " + shellQuote(p)
	}
	env["GIT_SSH_COMMAND"] = sshCmd
	if g.t.Has("token") {
		tok, err := g.t.String(ctx, "token")
		if err != nil {
			return nil, cleanup, err
		}
		user, err := g.t.StringOr(ctx, "user", "x-access-token")
		if err != nil {
			return nil, cleanup, err
		}
		cfg("http.extraHeader", "Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+tok)))
	}
	cfg("credential.helper", "")
	return env, cleanup, nil
}

func shellQuote(s string) string {
	if !strings.ContainsAny(s, " '\"\\$`") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// timeoutKey carries the call's effective timeout (connector.CallTimeout)
// from Do down to the git invocation.
type timeoutKey struct{}

// callTimeout is the current call's effective timeout, else the target's
// configured one (Open/Check fetches).
func (g *Git) callTimeout(ctx context.Context) time.Duration {
	if v, ok := ctx.Value(timeoutKey{}).(time.Duration); ok {
		return v
	}
	return g.t.Limits.Timeout
}
