// Package web is the tmvp connector for web apps, driven through playwright-cli
// (assumed on PATH, like aws; tmvp never installs it).
//
// Assets: assets/<system>/context.md (login steps, success, session ttl, never:)
// and assets/<system>/pages/*.md (path, elements, actions, checks) — YAML
// front-matter is what runs, the prose is for the agent (DESIGN §2.0.1).
//
// Sessions: one playwright-cli browser session per product/env/system/user.
// Browser storage state is saved 0600 under ~/.tmvp/<product>/web/ and reused
// until its ttl. `login: {manual: browser}` → `tmvp login <env> <system>` opens a
// headed browser for the human; later calls run headless; expired →
// login_required (DESIGN §8.1).
//
// Policy: goto/text/snapshot/screenshot/check/pages/elements are reads.
// click/fill/press/select/do are ReadWrite verbs: as plain reads they work
// only on non-mutating declared elements in an env with destructive: true;
// anything else runs through `tmvp write <env> <system> <verb> ... --reason`
// (e.g. `tmvp write prod admin click users.ban --reason "…"`).
// Elements/actions in never: are blocked wherever destructive is false.
//
// Secrets: fill/select values never go on playwright-cli's argv (visible in
// ps). They are written into a 0600 run-code script under the 0700 state dir
// and passed with `run-code --filename=…`, removed right after the call.
package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/muthuishere/tmvp/internal/secure"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
)

func init() { connector.Register("web", New) }

// New returns an unopened web connector.
func New() connector.Connector { return &Web{r: NewRunner()} }

var verbs = []connector.Verb{
	{Name: "login", Mode: connector.Read, Help: "login [-F user=ops] — run the scripted login (manual: browser → tmvp login <env> <system>)"},
	{Name: "goto", Mode: connector.Read, Help: "goto <page|/path> — navigate (base_url + path)"},
	{Name: "text", Mode: connector.Read, Help: "text <el|page.el> — visible text of an element"},
	{Name: "snapshot", Mode: connector.Read, Help: "snapshot [el] — accessibility snapshot (refs e1, e2 ...)"},
	{Name: "screenshot", Mode: connector.Read, Help: "screenshot [el] — PNG under ~/.tmvp/<product>/web/shots"},
	{Name: "check", Mode: connector.Read, Help: "check [page|--all] — run the page checks"},
	{Name: "ls", Mode: connector.Read, Help: "ls — declared pages across all asset roots (path, elements, actions, source); offline"},
	{Name: "pages", Mode: connector.Read, Help: "pages — declared pages, paths, actions"},
	{Name: "elements", Mode: connector.Read, Help: "elements <page> — declared elements, locators, mutating/never"},
	{Name: "click", Mode: connector.ReadWrite, Help: "click <el> — read: non-mutating elements in destructive envs; else tmvp write ... click <el> --reason"},
	{Name: "fill", Mode: connector.ReadWrite, Help: "fill <el> <value> — as click (value never on argv)"},
	{Name: "press", Mode: connector.ReadWrite, Help: "press <key> — always mutating (may submit): tmvp write ... press <key> --reason"},
	{Name: "select", Mode: connector.ReadWrite, Help: "select <el> <value> — as click"},
	{Name: "do", Mode: connector.ReadWrite, Default: true, Help: "do <action> [k=v ...] — page action; mutating ones via tmvp write (--preview resolves only)"},
}

// Web is one opened system (env/system/mode). Browser sessions are shared
// between the read and write instances of the same user.
type Web struct {
	t       *connector.Target
	r       Runner
	assets  *Assets
	base    *url.URL
	manual  bool
	browser string
	noCLI   error // playwright-cli missing: only asset verbs work

	home, work, shots string

	mu      sync.Mutex
	secrets map[string]bool // user credential values used (scrubbed from errors)
	used    map[string]bool // session names this instance touched
}

func (w *Web) Type() string            { return "web" }
func (w *Web) Verbs() []connector.Verb { return verbs }

// Guard validates arguments before Open (no system access, no secrets).
func (w *Web) Guard(c connector.Call) error {
	need := map[string]int{"goto": 1, "text": 1, "elements": 1, "click": 1, "fill": 2, "press": 1, "select": 2, "do": 1}
	if n, ok := need[c.Verb]; ok && len(c.Args) < n {
		return errs.New(errs.Usage, "%s needs %d argument(s)", c.Verb, n).WithHint(helpOf(c.Verb))
	}
	return nil
}

func helpOf(v string) string {
	for _, x := range verbs {
		if x.Name == v {
			return x.Help
		}
	}
	return ""
}

// Open reads base_url and the assets; the browser starts lazily on first use.
func (w *Web) Open(ctx context.Context, t *connector.Target) error {
	w.t = t
	if _, err := lookPath(CLI); err != nil {
		// ls/pages/elements work from assets alone; browser verbs fail in session()
		w.noCLI = errs.New(errs.Unsupported, "%s is not on PATH", CLI).
			WithHint("install it: npm install -g @playwright/cli && playwright-cli install-browser")
	}
	raw, err := t.String(ctx, "base_url")
	if err != nil {
		return errs.Wrap(errs.Config, err, "web: base_url")
	}
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errs.New(errs.Config, "web: base_url must be an http(s) URL")
	}
	w.base = u
	if w.assets, err = LoadAssets(t.AssetDirs()...); err != nil {
		return err
	}
	if w.assets.Login != nil && (w.assets.Login.Manual == "browser" || len(w.assets.Login.Steps) == 0) {
		w.manual = true
	}
	// config `login: {manual: browser}` on the system (context.md login: is the fallback)
	if t.Login != nil && t.Login.Manual == "browser" {
		w.manual = true
		if w.assets.Login == nil {
			w.assets.Login = &LoginSpec{Manual: "browser", Reuse: true, TTL: DefaultTTL}
		} else {
			w.assets.Login.Manual = "browser"
		}
	}
	if v, ok := t.Raw("manual_login"); ok && v == true {
		w.manual = true
		if w.assets.Login == nil {
			w.assets.Login = &LoginSpec{Manual: "browser", Reuse: true, TTL: DefaultTTL}
		}
	}
	w.browser, _ = t.StringOr(ctx, "browser", "")
	w.home = filepath.Join(protocol.Home(), safe(t.Product), "web")
	w.work = filepath.Join(w.home, "work")
	w.shots = filepath.Join(w.home, "shots")
	for _, d := range []string{w.home, w.work, w.shots} {
		if err := secure.MkdirAll(d); err != nil {
			return errs.Wrap(errs.Runtime, err, "web: state dir")
		}
	}
	w.secrets, w.used = map[string]bool{}, map[string]bool{}
	return nil
}

// Check (tmvp check): CLI present + config/assets parsed (done in Open).
func (w *Web) Check(ctx context.Context) error { return w.noCLI }

func (w *Web) Close() error {
	w.mu.Lock()
	names := make([]string, 0, len(w.used))
	for n := range w.used {
		names = append(names, n)
	}
	w.used = map[string]bool{}
	w.mu.Unlock()
	for _, n := range names {
		l := sessLock(n)
		l.Lock()
		if sessOpen[n] {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, _ = w.r.Run(ctx, w.work, []string{"-s=" + n, "close"})
			cancel()
			setOpen(n, false)
		}
		l.Unlock()
	}
	return nil
}

// ---- sessions (package-wide: one browser per product/env/system/user) ----

var (
	sessMu    sync.Mutex
	sessLocks = map[string]*sync.Mutex{}
	sessOpen  = map[string]bool{}
)

func sessLock(n string) *sync.Mutex {
	sessMu.Lock()
	defer sessMu.Unlock()
	if sessLocks[n] == nil {
		sessLocks[n] = &sync.Mutex{}
	}
	return sessLocks[n]
}

func setOpen(n string, v bool) { sessMu.Lock(); sessOpen[n] = v; sessMu.Unlock() }
func isOpen(n string) bool     { sessMu.Lock(); defer sessMu.Unlock(); return sessOpen[n] }

var unsafeRe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func safe(s string) string {
	s = unsafeRe.ReplaceAllString(s, "_")
	if s == "" {
		return "_"
	}
	return s
}

// sess is one call's view of a browser session.
type sess struct {
	w     *Web
	user  string
	name  string // playwright-cli session name
	state string // storage state file (0600)
	page  string // current declared page, "" if unknown
}

var curPage sync.Map // session name → current page

func (w *Web) userFor(c connector.Call) (string, error) {
	if u := c.Flags["user"]; u != "" {
		if !w.t.Has("users." + u) {
			return "", errs.New(errs.Config, "web: no user %q under users:", u)
		}
		return u, nil
	}
	m, _ := w.t.Raw("users")
	users, _ := m.(map[string]any)
	if len(users) == 0 {
		return "default", nil
	}
	names := make([]string, 0, len(users))
	for n := range users {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 1 {
		return names[0], nil
	}
	for _, n := range names {
		if r, _ := w.t.StringOr(context.Background(), "users."+n+".role", ""); r == string(w.t.Mode) {
			return n, nil
		}
	}
	return names[0], nil
}

func (w *Web) session(c connector.Call) (*sess, error) {
	if w.noCLI != nil {
		return nil, w.noCLI
	}
	u, err := w.userFor(c)
	if err != nil {
		return nil, err
	}
	key := safe(w.t.Env) + "-" + safe(w.t.System) + "-" + safe(u)
	// playwright-cli puts the session name in a unix socket path under $TMPDIR
	// (104-byte limit on macOS), so the name is a short hash.
	sum := sha256.Sum256([]byte(w.t.Product + "\x00" + key))
	name := "tmvp-" + hex.EncodeToString(sum[:5])
	w.mu.Lock()
	w.used[name] = true
	w.mu.Unlock()
	s := &sess{w: w, user: u, name: name, state: filepath.Join(w.home, key+".json")}
	if p, ok := curPage.Load(name); ok {
		s.page = p.(string)
	}
	return s, nil
}

func (s *sess) setPage(p string) { s.page = p; curPage.Store(s.name, p) }

func (w *Web) scrub(m string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	for v := range w.secrets {
		if v != "" {
			m = strings.ReplaceAll(m, v, "***")
		}
	}
	return m
}

// run calls playwright-cli on the session, reopening once if the daemon went away.
func (s *sess) run(ctx context.Context, args ...string) (*reply, error) {
	rep, err := call(ctx, s.w.r, s.w.work, s.name, s.w.scrub, args...)
	if err == errNotOpen {
		setOpen(s.name, false)
		if err = s.ensure(ctx); err != nil {
			return nil, err
		}
		rep, err = call(ctx, s.w.r, s.w.work, s.name, s.w.scrub, args...)
	}
	if err == errNotOpen {
		return nil, errs.New(errs.Runtime, "playwright-cli session %s is not open", s.name)
	}
	return rep, err
}

func (s *sess) fresh() (time.Time, bool) {
	l := s.w.assets.Login
	fi, err := os.Stat(s.state)
	if err != nil || l == nil {
		return time.Time{}, false
	}
	exp := fi.ModTime().Add(l.TTL)
	return exp, l.Reuse && time.Now().Before(exp)
}

func (s *sess) loginRequired() error {
	return errs.New(errs.LoginRequired, "web login required for %s.%s (user %s)", s.w.t.Env, s.w.t.System, s.user).
		WithHint(fmt.Sprintf("ask the user to run: tmvp login %s %s --user %s", s.w.t.Env, s.w.t.System, s.user))
}

// ensure opens the headless browser and makes sure it is logged in. Caller holds the session lock.
func (s *sess) ensure(ctx context.Context) error {
	if !isOpen(s.name) {
		args := []string{"open"}
		if s.w.browser != "" {
			args = append(args, "--browser="+s.w.browser)
		}
		if _, err := call(ctx, s.w.r, s.w.work, s.name, s.w.scrub, args...); err != nil {
			return err
		}
		setOpen(s.name, true)
		curPage.Delete(s.name)
		s.page = ""
		if _, ok := s.fresh(); ok {
			if _, err := call(ctx, s.w.r, s.w.work, s.name, s.w.scrub, "state-load", s.state); err != nil {
				return err
			}
			return nil
		}
	} else if _, ok := s.fresh(); ok || s.w.assets.Login == nil {
		return nil
	}
	if s.w.assets.Login == nil {
		return nil
	}
	if s.w.manual {
		return s.loginRequired()
	}
	return s.scriptedLogin(ctx)
}

var userRe = regexp.MustCompile(`\$\{user\.([A-Za-z0-9_]+)\}`)
var paramRe = regexp.MustCompile(`\{([A-Za-z0-9_]+)\}`)

// subst fills ${user.x} from users.<user>.x (secret, recorded for scrubbing) and {param}.
func (s *sess) subst(ctx context.Context, v string, params map[string]string) (string, error) {
	var ferr error
	out := userRe.ReplaceAllStringFunc(v, func(m string) string {
		k := userRe.FindStringSubmatch(m)[1]
		val, err := s.w.t.String(ctx, "users."+s.user+"."+k)
		if err != nil {
			ferr = errs.As(err)
			return ""
		}
		s.w.mu.Lock()
		s.w.secrets[val] = true
		s.w.mu.Unlock()
		return val
	})
	if ferr != nil {
		return "", ferr
	}
	out = paramRe.ReplaceAllStringFunc(out, func(m string) string {
		k := paramRe.FindStringSubmatch(m)[1]
		val, ok := params[k]
		if !ok {
			ferr = errs.New(errs.Usage, "missing parameter %q (pass %s=value)", k, k)
		}
		return val
	})
	return out, ferr
}

func (s *sess) scriptedLogin(ctx context.Context) error {
	l := s.w.assets.Login
	for i, st := range l.Steps {
		if err := s.step(ctx, st, l.Page, nil); err != nil {
			if errs.Is(err, errs.SecretMissing, errs.Config, errs.Usage) {
				return err
			}
			return errs.Wrap(errs.Runtime, err, "login step %d (%s)", i+1, st.Op)
		}
	}
	if len(l.Success) > 0 {
		loc, err := s.w.specLocator(l.Success, l.Page)
		if err != nil {
			return err
		}
		if ok, err := s.waitVisible(ctx, loc, successWait); err != nil {
			return err
		} else if !ok {
			// credentials rejected (or app changed): let the server refetch secrets once
			return errs.AuthError(fmt.Errorf("login did not reach its success element"))
		}
	}
	return s.saveState(ctx)
}

func (s *sess) saveState(ctx context.Context) error {
	if _, err := s.run(ctx, "state-save", s.state); err != nil {
		return err
	}
	if err := secure.File(s.state); err != nil {
		return errs.Wrap(errs.Runtime, err, "web: protect session state")
	}
	return nil
}

// ---- locators / steps ----

func (w *Web) specLocator(m map[string]any, page string) (string, error) {
	if ref, ok := m["el"].(string); ok {
		el, _, err := w.assets.Resolve(ref, page)
		if err != nil {
			return "", err
		}
		return el.Locator()
	}
	el := Element{}
	el.TestID, _ = m["testid"].(string)
	el.Role, _ = m["role"].(string)
	el.Name, _ = m["name"].(string)
	el.Text, _ = m["text"].(string)
	el.CSS, _ = m["css"].(string)
	el.Exact, _ = m["exact"].(bool)
	loc, err := el.Locator()
	if err != nil {
		return "", errs.New(errs.Config, "web: %v", err)
	}
	return loc, nil
}

func (w *Web) locator(ref, page string) (string, string, Element, error) {
	el, full, err := w.assets.Resolve(ref, page)
	if err != nil {
		return "", "", el, err
	}
	loc, err := el.Locator()
	if err != nil {
		return "", "", el, errs.New(errs.Config, "web: %s: %v", full, err)
	}
	return loc, full, el, nil
}

// target resolves the URL for a page name or /path; only base_url's origin is allowed.
func (w *Web) target(p string) (string, string, error) {
	if pg := w.assets.Pages[p]; pg != nil {
		if pg.Path == "" {
			return "", "", errs.New(errs.Config, "page %q has no path", p)
		}
		return w.base.String() + "/" + strings.TrimLeft(pg.Path, "/"), p, nil
	}
	if strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") {
		// match a declared page by path, so element refs resolve against it
		page := ""
		for _, n := range w.assets.pageNames() {
			if w.assets.Pages[n].Path == p {
				page = n
			}
		}
		return w.base.String() + p, page, nil
	}
	return "", "", errs.New(errs.Usage, "goto takes a page name or a /path under base_url, got %q", p).WithHint("run: pages")
}

func (s *sess) navigate(ctx context.Context, p string) (string, error) {
	u, page, err := s.w.target(p)
	if err != nil {
		return "", err
	}
	if _, err := s.run(ctx, "goto", u); err != nil {
		return "", err
	}
	s.setPage(page)
	return u, nil
}

func (s *sess) step(ctx context.Context, st Step, page string, params map[string]string) error {
	if page == "" {
		page = s.page
	}
	argMap, _ := st.Arg.(map[string]any)
	str := func(k string) string { v, _ := argMap[k].(string); return v }
	elOf := func() (string, error) {
		ref := stepElement(st)
		if ref == "" {
			return "", errs.New(errs.Config, "step %s: no el", st.Op)
		}
		loc, _, _, err := s.w.locator(ref, page)
		return loc, err
	}
	switch st.Op {
	case "goto":
		p, _ := st.Arg.(string)
		if p == "" {
			p = str("page")
		}
		_, err := s.navigate(ctx, p)
		return err
	case "click", "check", "uncheck":
		loc, err := elOf()
		if err != nil {
			return err
		}
		_, err = s.run(ctx, st.Op, loc)
		return err
	case "fill", "select":
		loc, err := elOf()
		if err != nil {
			return err
		}
		v, err := s.subst(ctx, fmt.Sprint(argMap["value"]), params)
		if err != nil {
			return err
		}
		return s.fillSecure(ctx, st.Op, loc, v)
	case "press":
		key, _ := st.Arg.(string)
		if key == "" {
			key = str("key")
		}
		if ref := str("el"); ref != "" {
			loc, _, _, err := s.w.locator(ref, page)
			if err != nil {
				return err
			}
			if _, err := s.run(ctx, "click", loc); err != nil {
				return err
			}
		}
		_, err := s.run(ctx, "press", key)
		return err
	case "wait":
		return s.wait(ctx, st.Arg, page)
	}
	return errs.New(errs.Config, "unknown step %q", st.Op)
}

const maxWait = 30 * time.Second

func (s *sess) wait(ctx context.Context, arg any, page string) error {
	switch a := arg.(type) {
	case int:
		return sleep(ctx, min(time.Duration(a)*time.Millisecond, maxWait))
	case string:
		d, err := time.ParseDuration(a)
		if err != nil {
			return errs.New(errs.Config, "wait: bad duration %q", a)
		}
		return sleep(ctx, min(d, maxWait))
	case map[string]any:
		if u, ok := a["url"].(string); ok {
			ok, err := s.poll(ctx, 15*time.Second, func() (bool, error) {
				cur, err := s.url(ctx)
				return strings.Contains(cur, u), err
			})
			if err == nil && !ok {
				err = errs.New(errs.Timeout, "wait: url never contained %q", u)
			}
			return err
		}
		loc, err := s.w.specLocator(a, page)
		if err != nil {
			return err
		}
		ok, err := s.waitVisible(ctx, loc, 15*time.Second)
		if err == nil && !ok {
			err = errs.New(errs.Timeout, "wait: element never became visible")
		}
		return err
	}
	return errs.New(errs.Config, "wait: want {url: ...}, {el: ...} or a duration")
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return errs.Wrap(errs.Timeout, ctx.Err(), "wait")
	case <-time.After(d):
		return nil
	}
}

// pollEvery is the wait poll interval (shortened in tests).
var pollEvery = 250 * time.Millisecond

// successWait bounds how long a scripted login waits for its success element.
var successWait = 15 * time.Second

func (s *sess) poll(ctx context.Context, d time.Duration, f func() (bool, error)) (bool, error) {
	end := time.Now().Add(d)
	for {
		ok, err := f()
		if err != nil || ok {
			return ok, err
		}
		if time.Now().After(end) {
			return false, nil
		}
		if err := sleep(ctx, pollEvery); err != nil {
			return false, err
		}
	}
}

func (s *sess) url(ctx context.Context) (string, error) {
	rep, err := s.run(ctx, "eval", "() => location.href")
	if err != nil {
		return "", err
	}
	v, _ := evalValue(rep).(string)
	return v, nil
}

const visibleJS = "el => !!(el.offsetWidth || el.offsetHeight || el.getClientRects().length)"

func (s *sess) visible(ctx context.Context, loc string) (bool, error) {
	rep, err := s.run(ctx, "eval", visibleJS, loc)
	if err != nil {
		if errs.CodeOf(err) == errs.NotFound {
			return false, nil
		}
		return false, err
	}
	b, _ := evalValue(rep).(bool)
	return b, nil
}

func (s *sess) waitVisible(ctx context.Context, loc string, d time.Duration) (bool, error) {
	return s.poll(ctx, d, func() (bool, error) { return s.visible(ctx, loc) })
}
