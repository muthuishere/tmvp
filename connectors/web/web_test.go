package web

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

const fakePW = "FAKE_SECRET_123"

// fakeRunner mimics playwright-cli's --json envelope.
type fakeRunner struct {
	mu       sync.Mutex
	calls    [][]string
	open     map[string]bool
	visible  bool              // result of visibility evals
	url      string            // location.href
	text     string            // innerText
	failOn   map[string]string // command → error message
	notFound bool
	scripts  []string // run-code bodies (fill/select)
}

func newFake() *fakeRunner {
	return &fakeRunner{open: map[string]bool{}, visible: true, url: "https://admin.example.test/dashboard", text: "Users", failOn: map[string]string{}}
}

func (f *fakeRunner) Run(_ context.Context, _ string, args []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	sess := strings.TrimPrefix(args[0], "-s=")
	rest := args[1:]
	if len(rest) > 0 && rest[0] == "--json" {
		rest = rest[1:]
	}
	cmd := rest[0]
	jerr := func(m string) ([]byte, error) {
		b, _ := json.Marshal(map[string]any{"isError": true, "error": "Error: " + m})
		return b, nil
	}
	if m, ok := f.failOn[cmd]; ok {
		return jerr(m)
	}
	switch cmd {
	case "open":
		f.open[sess] = true
		return []byte(`{"session":"` + sess + `","result":{}}`), nil
	case "close":
		delete(f.open, sess)
		return []byte(`{"status":"closed"}`), nil
	}
	if !f.open[sess] {
		return jerr("The browser '" + sess + "' is not open, please run open first")
	}
	switch cmd {
	case "run-code":
		// fill/select arrive as a 0600 script file; record its body instead of the path
		fn := strings.TrimPrefix(rest[1], "--filename=")
		b, err := os.ReadFile(fn)
		if err != nil {
			return jerr("no script")
		}
		if fi, _ := os.Stat(fn); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
			return jerr("script not 0600")
		}
		f.scripts = append(f.scripts, string(b))
		if m, ok := f.failOn["fill"]; ok {
			return jerr(m)
		}
		return []byte(`{}`), nil
	case "state-save":
		_ = os.WriteFile(rest[1], []byte(`{"cookies":[]}`), 0o644)
		return []byte(`{"result":"saved"}`), nil
	case "eval":
		var v any
		switch {
		case strings.Contains(rest[1], "location.href"):
			v = f.url
		case strings.Contains(rest[1], "offsetWidth"):
			if f.notFound {
				return jerr(`"x" does not match any elements.`)
			}
			v = f.visible
		default:
			v = f.text
		}
		inner, _ := json.Marshal(v)
		out, _ := json.Marshal(map[string]any{"result": string(inner)})
		return out, nil
	case "snapshot":
		return []byte(`{"snapshot":"- heading \"Users\" [ref=e1]\n- button \"Delete user\" [ref=e2]"}`), nil
	}
	return []byte(`{"snapshot":{"file":".playwright-cli/x.yml"}}`), nil
}

func (f *fakeRunner) cmds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		i := 1
		if len(c) > 1 && c[1] == "--json" {
			i = 2
		}
		out = append(out, strings.Join(c[i:], " "))
	}
	return out
}

const contextMD = `---
login:
  page: login
  steps:
    - goto: /login
    - fill: { el: email, value: "${user.user}" }
    - fill: { el: password, value: "${user.password}" }
    - click: submit
    - wait: { url: /dashboard }
  success: { el: dashboard.heading }
  session: { reuse: true, ttl: 7d }
never: [users.delete, billing.refund]
---
# Admin
prose for the agent
`

const loginMD = `---
page: login
path: /login
elements:
  email:    { testid: login-email }
  password: { css: "input[type=password]" }
  submit:   { role: button, name: "Sign in" }
---
`

const dashMD = `---
page: dashboard
path: /dashboard
elements:
  heading: { role: heading, name: Dashboard }
---
`

const usersMD = `---
page: users
path: /users
elements:
  search: { testid: users-search }
  rows:   { testid: user-row }
  tab:    { text: "Active" }
  delete: { role: button, name: "Delete user", mutating: true }
  ban:    { role: button, name: "Ban", mutating: true }
actions:
  find-user: [ { fill: { el: search, value: "{email}" } }, { press: Enter }, { wait: { el: rows } } ]
  ban-user:  [ { click: ban } ]
checks:
  - visible: { role: heading, name: Users }
  - text: { el: rows, contains: Users }
---
`

func writeAssets(t *testing.T, manual bool) string {
	t.Helper()
	dir := t.TempDir()
	a := filepath.Join(dir, "assets", "admin")
	must(t, os.MkdirAll(filepath.Join(a, "pages"), 0o755))
	ctx := contextMD
	if manual {
		ctx = "---\nlogin:\n  manual: browser\n  page: login\n  success: { el: dashboard.heading }\n  session: { ttl: 12h }\n---\n"
	}
	must(t, os.WriteFile(filepath.Join(a, "context.md"), []byte(ctx), 0o644))
	for n, b := range map[string]string{"login": loginMD, "dashboard": dashMD, "users": usersMD} {
		must(t, os.WriteFile(filepath.Join(a, "pages", n+".md"), []byte(b), 0o644))
	}
	return dir
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type fixture struct {
	w *Web
	f *fakeRunner
}

func setup(t *testing.T, mode connector.Mode, manual bool) fixture {
	t.Helper()
	t.Setenv("TMVP_HOME", t.TempDir())
	lookPath = func(string) (string, error) { return "/fake/playwright-cli", nil }
	pollEvery, loginPollEvery, successWait = time.Millisecond, time.Millisecond, 20*time.Millisecond
	f := newFake()
	w := &Web{r: f}
	tg := connector.NewTarget(connector.Target{Product: "acme", Env: "prod", System: "admin", Type: "web", Mode: mode, Dir: writeAssets(t, manual)},
		connector.StaticFields{
			"base_url": "https://admin.example.test",
			"users":    map[string]any{"ops": map[string]any{"role": "read", "user": "ops@example.test", "password": fakePW}},
		})
	must(t, w.Open(context.Background(), tg))
	t.Cleanup(func() { _ = w.Close() })
	return fixture{w, f}
}

func code(err error) errs.Code { return errs.CodeOf(err) }

func TestVerbTable(t *testing.T) {
	w := New()
	if w.Type() != "web" {
		t.Fatal(w.Type())
	}
	reads := map[string]bool{"login": true, "goto": true, "text": true, "snapshot": true, "screenshot": true, "check": true, "pages": true, "elements": true, "ls": true}
	rw := map[string]bool{"click": true, "fill": true, "press": true, "select": true, "do": true}
	for _, v := range w.Verbs() {
		if reads[v.Name] != (v.Mode == connector.Read) || rw[v.Name] != (v.Mode == connector.ReadWrite) {
			t.Errorf("verb %s mode %s", v.Name, v.Mode)
		}
	}
	if _, ok := connector.FindVerb(w.Verbs(), "act"); ok {
		t.Fatal("act was replaced by ReadWrite verbs")
	}
	dv, ok := connector.DefaultWriteVerb(w.Verbs())
	if !ok || dv.Name != "do" {
		t.Fatalf("default write verb = %v", dv.Name)
	}
}

func TestGuard(t *testing.T) {
	g := New().(connector.Guard)
	for _, c := range []connector.Call{{Verb: "goto"}, {Verb: "fill", Args: []string{"x"}}, {Verb: "select", Args: []string{"x"}, Write: true}} {
		if code(g.Guard(c)) != errs.Usage {
			t.Errorf("%v: want usage", c)
		}
	}
	if err := g.Guard(connector.Call{Verb: "click", Write: true, Args: []string{"users.delete"}}); err != nil {
		t.Fatal(err)
	}
}

func TestAssets(t *testing.T) {
	a, err := LoadAssets(filepath.Join(writeAssets(t, false), "assets", "admin"))
	must(t, err)
	if a.Login.TTL != 7*24*time.Hour || len(a.Login.Steps) != 5 || !a.Never["users.delete"] {
		t.Fatalf("context: %+v", a.Login)
	}
	cases := map[string]string{
		"login.email":    "getByTestId('login-email')",
		"login.password": "input[type=password]",
		"login.submit":   "getByRole('button', { name: 'Sign in' })",
		"users.tab":      "getByText('Active')",
	}
	for ref, want := range cases {
		el, _, err := a.Resolve(ref, "")
		must(t, err)
		if got, _ := el.Locator(); got != want {
			t.Errorf("%s: %s want %s", ref, got, want)
		}
	}
	// bare names resolve on the current page first, then uniquely
	if _, full, err := a.Resolve("search", ""); err != nil || full != "users.search" {
		t.Fatal(full, err)
	}
	if !a.Pages["users"].Actions["ban-user"].Mutating || a.Pages["users"].Actions["find-user"].Mutating {
		t.Fatal("action mutating flags")
	}
	if !a.IsNever("users.delete") || a.IsNever("users.search") {
		t.Fatal("never")
	}
	if jsq(`it's`) != `'it\'s'` {
		t.Fatal(jsq(`it's`))
	}
}

func TestPagesElementsNoBrowser(t *testing.T) {
	fx := setup(t, connector.ModeRead, false)
	var c connector.Collector
	must(t, fx.w.Do(context.Background(), connector.Call{Verb: "pages"}, &c))
	if len(c.Rows) != 3 {
		t.Fatal(c.Rows)
	}
	c = connector.Collector{}
	must(t, fx.w.Do(context.Background(), connector.Call{Verb: "elements", Args: []string{"users"}}, &c))
	found := false
	for _, r := range c.Rows {
		if r["element"] == "users.delete" && r["mutating"] == true && r["never"] == true {
			found = true
		}
	}
	if !found || len(fx.f.calls) != 0 {
		t.Fatal(c.Rows, fx.f.calls)
	}
}

func TestScriptedLoginAndRead(t *testing.T) {
	fx := setup(t, connector.ModeRead, false)
	var c connector.Collector
	must(t, fx.w.Do(context.Background(), connector.Call{Verb: "text", Args: []string{"users.rows"}}, &c))
	if c.Rows[0]["text"] != "Users" {
		t.Fatal(c.Rows)
	}
	cmds := strings.Join(fx.f.cmds(), "\n") + "\n" + strings.Join(fx.f.scripts, "\n")
	if strings.Contains(strings.Join(fx.f.cmds(), "\n"), fakePW) {
		t.Fatal("password on playwright-cli argv")
	}
	if !strings.Contains(strings.Join(fx.f.scripts, ""), fakePW) {
		t.Fatal("password was not filled through the script file")
	}
	for _, want := range []string{"open", "goto https://admin.example.test/login", `page.getByTestId('login-email').fill("ops@example.test")`, "click getByRole('button', { name: 'Sign in' })", "state-save", "eval el => el.innerText getByTestId('user-row')"} {
		if !strings.Contains(cmds, want) {
			t.Errorf("missing %q in\n%s", want, cmds)
		}
	}
	st := filepath.Join(os.Getenv("TMVP_HOME"), "acme", "web", "prod-admin-ops.json")
	fi, err := os.Stat(st)
	must(t, err)
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("state perm %v", fi.Mode().Perm())
	}
	// second call reuses the open, logged-in session
	n := len(fx.f.calls)
	must(t, fx.w.Do(context.Background(), connector.Call{Verb: "goto", Args: []string{"users"}}, &c))
	if len(fx.f.calls) != n+1 {
		t.Fatalf("expected one goto, got %v", fx.f.cmds()[n:])
	}
}

func TestSessionStateReused(t *testing.T) {
	fx := setup(t, connector.ModeRead, false)
	var c connector.Collector
	must(t, fx.w.Do(context.Background(), connector.Call{Verb: "goto", Args: []string{"/users"}}, &c))
	// browser daemon dies (server restart, crash): reopen + state-load, no new login
	fx.f.mu.Lock()
	fx.f.open = map[string]bool{}
	fx.f.calls = nil
	fx.f.mu.Unlock()
	s, _ := fx.w.session(connector.Call{})
	setOpen(s.name, false)
	must(t, fx.w.Do(context.Background(), connector.Call{Verb: "goto", Args: []string{"/users"}}, &c))
	cmds := strings.Join(fx.f.cmds(), "\n")
	if !strings.Contains(cmds, "state-load") || strings.Contains(cmds, "fill") {
		t.Fatal(cmds)
	}
}

func TestReconnectWhenDaemonGone(t *testing.T) {
	fx := setup(t, connector.ModeRead, false)
	var c connector.Collector
	must(t, fx.w.Do(context.Background(), connector.Call{Verb: "goto", Args: []string{"users"}}, &c))
	fx.f.mu.Lock()
	fx.f.open = map[string]bool{} // tmvp still thinks it is open
	fx.f.mu.Unlock()
	must(t, fx.w.Do(context.Background(), connector.Call{Verb: "goto", Args: []string{"users"}}, &c))
}

func TestLoginRejectedIsAuthError(t *testing.T) {
	fx := setup(t, connector.ModeRead, false)
	fx.f.visible = false
	fx.f.notFound = true
	err := fx.w.Do(context.Background(), connector.Call{Verb: "login"}, &connector.Collector{})
	if code(err) != errs.Auth {
		t.Fatalf("want auth_error, got %v", err)
	}
}

func TestNoSecretInErrors(t *testing.T) {
	fx := setup(t, connector.ModeRead, false)
	fx.f.failOn["fill"] = "fill failed for value " + fakePW + " on getByTestId('login-email')"
	err := fx.w.Do(context.Background(), connector.Call{Verb: "goto", Args: []string{"users"}}, &connector.Collector{})
	if err == nil {
		t.Fatal("want error")
	}
	// the email fill errors first; force the password one
	delete(fx.f.failOn, "fill")
	fx.f.failOn["click"] = "boom " + fakePW
	err = fx.w.Do(context.Background(), connector.Call{Verb: "login"}, &connector.Collector{})
	if err == nil || strings.Contains(err.Error(), fakePW) {
		t.Fatalf("secret leaked or no error: %v", err)
	}
}

func TestReadCannotMutate(t *testing.T) {
	fx := setup(t, connector.ModeRead, false)
	ctx := context.Background()
	// mutating element through a read verb, even where destructive: true
	err := fx.w.Do(ctx, connector.Call{Verb: "click", Args: []string{"users.ban"}, Destructive: true}, &connector.Collector{})
	if code(err) != errs.PolicyBlocked || !strings.Contains(errs.As(err).Hint, "tmvp write prod admin click users.ban") {
		t.Fatalf("got %v", err)
	}
	// non-mutating element in a protected env is still a write
	err = fx.w.Do(ctx, connector.Call{Verb: "click", Args: []string{"users.tab"}}, &connector.Collector{})
	if code(err) != errs.PolicyBlocked {
		t.Fatalf("got %v", err)
	}
	// press may submit a form: always a write
	err = fx.w.Do(ctx, connector.Call{Verb: "press", Args: []string{"Enter"}, Destructive: true}, &connector.Collector{})
	if code(err) != errs.PolicyBlocked {
		t.Fatalf("got %v", err)
	}
	// mutating action
	err = fx.w.Do(ctx, connector.Call{Verb: "do", Args: []string{"ban-user"}, Destructive: true}, &connector.Collector{})
	if code(err) != errs.PolicyBlocked {
		t.Fatalf("got %v", err)
	}
	// snapshot refs never drive interactions
	err = fx.w.Do(ctx, connector.Call{Verb: "click", Args: []string{"e2"}, Destructive: true}, &connector.Collector{})
	if code(err) != errs.Usage {
		t.Fatalf("got %v", err)
	}
	if len(fx.f.calls) != 0 {
		t.Fatalf("browser touched: %v", fx.f.cmds())
	}
	// non-mutating + destructive env → allowed as a read
	must(t, fx.w.Do(ctx, connector.Call{Verb: "click", Args: []string{"users.tab"}, Destructive: true}, &connector.Collector{}))
	// a write call is refused on a read-mode connection
	err = fx.w.Do(ctx, connector.Call{Verb: "click", Write: true, Args: []string{"users.tab"}, Destructive: true}, &connector.Collector{})
	if code(err) != errs.PolicyBlocked {
		t.Fatalf("got %v", err)
	}
}

func TestWriteVerbsNeverPreview(t *testing.T) {
	fx := setup(t, connector.ModeWrite, false)
	ctx := context.Background()
	// never: blocked where destructive: false, even through tmvp write
	err := fx.w.Do(ctx, connector.Call{Verb: "click", Write: true, Args: []string{"users.delete"}, Reason: "r"}, &connector.Collector{})
	if code(err) != errs.PolicyBlocked {
		t.Fatalf("got %v", err)
	}
	// preview resolves only
	var c connector.Collector
	must(t, fx.w.Do(ctx, connector.Call{Verb: "click", Write: true, Args: []string{"users.ban"}, Preview: true, Reason: "r"}, &c))
	if c.Rows[0]["preview"] != true || c.Rows[0]["locator"] != "getByRole('button', { name: 'Ban' })" || len(fx.f.calls) != 0 {
		t.Fatal(c.Rows, fx.f.cmds())
	}
	// real write on a mutating element in a protected env
	must(t, fx.w.Do(ctx, connector.Call{Verb: "click", Write: true, Args: []string{"users.ban"}, Reason: "r"}, &c))
	// action with params
	must(t, fx.w.Do(ctx, connector.Call{Verb: "do", Write: true, Args: []string{"find-user", "email=a@example.test"}, Reason: "r"}, &c))
	cmds := strings.Join(fx.f.cmds(), "\n") + "\n" + strings.Join(fx.f.scripts, "\n")
	for _, want := range []string{"click getByRole('button', { name: 'Ban' })", `page.getByTestId('users-search').fill("a@example.test")`, "press Enter"} {
		if !strings.Contains(cmds, want) {
			t.Errorf("missing %q in\n%s", want, cmds)
		}
	}
	err = fx.w.Do(ctx, connector.Call{Verb: "do", Write: true, Args: []string{"find-user"}, Reason: "r"}, &c)
	if code(err) != errs.Usage {
		t.Fatalf("missing param: %v", err)
	}
	// never is allowed where destructive: true
	must(t, fx.w.Do(ctx, connector.Call{Verb: "click", Write: true, Args: []string{"users.delete"}, Reason: "r", Destructive: true}, &c))
}

func TestManualLoginRequired(t *testing.T) {
	fx := setup(t, connector.ModeRead, true)
	err := fx.w.Do(context.Background(), connector.Call{Verb: "goto", Args: []string{"users"}}, &connector.Collector{})
	e := errs.As(err)
	if e.Code != errs.LoginRequired || !strings.Contains(e.Hint, "tmvp login prod admin --user ops") {
		t.Fatalf("got %v", err)
	}
	for _, c := range fx.f.cmds() {
		if strings.HasPrefix(c, "fill") {
			t.Fatal("manual login must not run steps")
		}
	}
}

func TestManualBrowserLogin(t *testing.T) {
	fx := setup(t, connector.ModeRead, true)
	var c connector.Collector
	must(t, fx.w.Login(context.Background(), connector.Call{Verb: "login", Flags: map[string]string{"user": "ops"}}, &c))
	cmds := strings.Join(fx.f.cmds(), "\n")
	if !strings.Contains(cmds, "open --headed https://admin.example.test/login") || !strings.Contains(cmds, "state-save") {
		t.Fatal(cmds)
	}
	if c.Rows[0]["status"] != "session saved" {
		t.Fatal(c.Rows)
	}
	// later calls run headless with the saved state
	fx.f.calls = nil
	must(t, fx.w.Do(context.Background(), connector.Call{Verb: "goto", Args: []string{"users"}}, &c))
	cmds = strings.Join(fx.f.cmds(), "\n")
	if strings.Contains(cmds, "--headed") || !strings.Contains(cmds, "state-load") {
		t.Fatal(cmds)
	}
	// expired → login_required again
	st := filepath.Join(os.Getenv("TMVP_HOME"), "acme", "web", "prod-admin-ops.json")
	old := time.Now().Add(-13 * time.Hour)
	must(t, os.Chtimes(st, old, old))
	err := fx.w.Do(context.Background(), connector.Call{Verb: "goto", Args: []string{"users"}}, &c)
	if code(err) != errs.LoginRequired {
		t.Fatalf("got %v", err)
	}
}

func TestManualLoginTimesOut(t *testing.T) {
	fx := setup(t, connector.ModeRead, true)
	fx.f.notFound = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := fx.w.Login(ctx, connector.Call{Verb: "login"}, &connector.Collector{})
	if code(err) != errs.LoginRequired && code(err) != errs.Timeout {
		t.Fatalf("got %v", err)
	}
}

func TestCheckSnapshotScreenshot(t *testing.T) {
	fx := setup(t, connector.ModeRead, false)
	ctx := context.Background()
	var c connector.Collector
	must(t, fx.w.Do(ctx, connector.Call{Verb: "check", Args: []string{"--all"}}, &c))
	if len(c.Rows) != 2 || c.Rows[0]["ok"] != true {
		t.Fatal(c.Rows)
	}
	fx.f.text = "nothing"
	err := fx.w.Do(ctx, connector.Call{Verb: "check", Args: []string{"users"}}, &connector.Collector{})
	if err == nil || !strings.Contains(err.Error(), "1 check(s) failed") {
		t.Fatal(err)
	}
	c = connector.Collector{}
	must(t, fx.w.Do(ctx, connector.Call{Verb: "snapshot"}, &c))
	if len(c.Lines) != 2 {
		t.Fatal(c.Lines)
	}
	c = connector.Collector{}
	must(t, fx.w.Do(ctx, connector.Call{Verb: "screenshot", Args: []string{"e1"}}, &c))
	if f, _ := c.Rows[0]["file"].(string); !strings.HasPrefix(f, os.Getenv("TMVP_HOME")) {
		t.Fatal(c.Rows)
	}
}

func TestGotoRejectsForeignURL(t *testing.T) {
	fx := setup(t, connector.ModeRead, false)
	for _, p := range []string{"https://evil.example.test/", "//evil.example.test/x", "nopage"} {
		err := fx.w.Do(context.Background(), connector.Call{Verb: "goto", Args: []string{p}}, &connector.Collector{})
		if code(err) != errs.Usage {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestErrorMapping(t *testing.T) {
	cases := map[string]errs.Code{
		`"x" does not match any elements.`: errs.NotFound,
		"Timeout 5000ms exceeded":          errs.Timeout,
		"net::ERR_NAME_NOT_RESOLVED at x":  errs.Unreachable,
		"Executable doesn't exist at /x/y": errs.Unsupported,
		"something odd":                    errs.Runtime,
	}
	for m, want := range cases {
		if got := classify("goto", m).Code; got != want {
			t.Errorf("%s: %s want %s", m, got, want)
		}
	}
}

func TestOpenErrors(t *testing.T) {
	t.Setenv("TMVP_HOME", t.TempDir())
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	w := &Web{r: newFake()}
	err := w.Open(context.Background(), connector.NewTarget(connector.Target{Product: "p", Env: "e", System: "s", Dir: t.TempDir()}, connector.StaticFields{"base_url": "https://x.test"}))
	if err != nil { // asset verbs (ls/pages/elements) work without playwright-cli
		t.Fatal(err)
	}
	if err := w.Do(context.Background(), connector.Call{Verb: "ls"}, &connector.Collector{}); err != nil {
		t.Fatal(err)
	}
	if err := w.Do(context.Background(), connector.Call{Verb: "goto", Args: []string{"/"}}, &connector.Collector{}); code(err) != errs.Unsupported {
		t.Fatal(err)
	}
	if code(w.Check(context.Background())) != errs.Unsupported {
		t.Fatal("check should report the missing CLI")
	}
	lookPath = func(string) (string, error) { return "/x", nil }
	err = w.Open(context.Background(), connector.NewTarget(connector.Target{Product: "p", Env: "e", System: "s", Dir: t.TempDir()}, connector.StaticFields{"base_url": "ftp://x"}))
	if code(err) != errs.Config {
		t.Fatal(err)
	}
}

// TestLiveWeb drives the real playwright-cli against TMVP_TEST_WEB_URL (a page with an <h1>).
func TestLiveWeb(t *testing.T) {
	base := os.Getenv("TMVP_TEST_WEB_URL")
	if base == "" {
		t.Skip("TMVP_TEST_WEB_URL not set")
	}
	t.Setenv("TMVP_HOME", t.TempDir())
	lookPath = exec.LookPath
	w := New().(*Web)
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "assets", "site", "pages"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "assets", "site", "pages", "home.md"), []byte("---\npath: /\nelements:\n  title: { css: h1 }\nchecks:\n  - visible: { css: h1 }\n---\n"), 0o644))
	must(t, w.Open(context.Background(), connector.NewTarget(connector.Target{Product: "live", Env: "test", System: "site", Mode: connector.ModeRead, Dir: dir}, connector.StaticFields{"base_url": base})))
	defer w.Close()
	var c connector.Collector
	must(t, w.Do(context.Background(), connector.Call{Verb: "check", Args: []string{"home"}}, &c))
	must(t, w.Do(context.Background(), connector.Call{Verb: "text", Args: []string{"home.title"}}, &c))
	t.Log(c.Rows)
}

// login: {manual: browser} in config.yaml (Target.Login) makes the system manual
// even when context.md declares scripted steps.
func TestConfigLoginManualBrowser(t *testing.T) {
	fx := setup(t, connector.ModeRead, false)
	w := New().(*Web)
	w.r = fx.f
	tg := connector.NewTarget(connector.Target{Product: "acme", Env: "prod", System: "admin", Type: "web", Mode: connector.ModeRead,
		Dir: fx.w.t.Dir, Login: &connector.LoginSpec{Manual: "browser"}}, connector.StaticFields{"base_url": "https://admin.example.test"})
	must(t, w.Open(context.Background(), tg))
	if !w.manual {
		t.Fatal("config login.manual=browser ignored")
	}
	err := w.Do(context.Background(), connector.Call{Verb: "goto", Args: []string{"users"}}, &connector.Collector{})
	if code(err) != errs.LoginRequired {
		t.Fatalf("got %v", err)
	}
}
