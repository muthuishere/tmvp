package web

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/muthuishere/tmvp/internal/secure"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// Do runs one verb. Calls on the same browser session are serialised.
func (w *Web) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	switch c.Verb {
	case "pages", "ls":
		return w.pages(e)
	case "elements":
		return w.elements(c.Args[0], e)
	}
	s, err := w.session(c)
	if err != nil {
		return err
	}
	l := sessLock(s.name)
	l.Lock()
	defer l.Unlock()
	switch c.Verb {
	case "login":
		return s.login(ctx, e)
	case "goto":
		if err := s.ensure(ctx); err != nil {
			return err
		}
		u, err := s.navigate(ctx, c.Args[0])
		if err != nil {
			return err
		}
		return e.Row(connector.Row{"page": s.page, "url": w.scrub(u)})
	case "text":
		return s.text(ctx, c, e)
	case "snapshot":
		return s.snapshot(ctx, c, e)
	case "screenshot":
		return s.screenshot(ctx, c, e)
	case "check":
		return s.check(ctx, c, e)
	case "click", "fill", "press", "select", "do":
		return s.interact(ctx, c, c.Verb, c.Args, c.Write, e)
	}
	return errs.New(errs.Usage, "web: unknown verb %q", c.Verb)
}

func (w *Web) pages(e connector.Emit) error {
	for _, n := range w.assets.pageNames() {
		p := w.assets.Pages[n]
		acts := make([]string, 0, len(p.Actions))
		for a := range p.Actions {
			acts = append(acts, a)
		}
		sort.Strings(acts)
		if err := e.Row(connector.Row{"kind": "page", "page": n, "path": p.Path, "elements": len(p.Elements), "actions": strings.Join(acts, ","), "checks": len(p.Checks), "source": p.Source}); err != nil {
			return stop(err)
		}
	}
	return nil
}

func (w *Web) elements(page string, e connector.Emit) error {
	p := w.assets.Pages[page]
	if p == nil {
		return errs.New(errs.NotFound, "no page %q", page).WithHint("run: pages")
	}
	names := make([]string, 0, len(p.Elements))
	for n := range p.Elements {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		el := p.Elements[n]
		loc, _ := el.Locator()
		if err := e.Row(connector.Row{"element": page + "." + n, "locator": loc, "mutating": el.Mutating, "never": w.assets.IsNever(page + "." + n)}); err != nil {
			return stop(err)
		}
	}
	for _, n := range sortedKeys(p.Actions) {
		if err := e.Row(connector.Row{"element": page + "." + n, "locator": "(action)", "mutating": p.Actions[n].Mutating, "never": w.assets.IsNever(page + "." + n)}); err != nil {
			return stop(err)
		}
	}
	return nil
}

func sortedKeys(m map[string]*Action) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func stop(err error) error {
	if err == connector.ErrLimit {
		return nil
	}
	return err
}

// login: scripted steps (re-run now); manual: report status / login_required.
func (s *sess) login(ctx context.Context, e connector.Emit) error {
	if s.w.assets.Login == nil {
		return e.Row(connector.Row{"user": s.user, "status": "no login declared"})
	}
	if s.w.manual {
		exp, ok := s.fresh()
		if !ok {
			return s.loginRequired()
		}
		return e.Row(connector.Row{"user": s.user, "status": "session valid", "expires": exp.UTC().Format(time.RFC3339)})
	}
	if !isOpen(s.name) {
		if _, err := s.run(ctx, "open"); err != nil {
			return err
		}
		setOpen(s.name, true)
	}
	if err := s.scriptedLogin(ctx); err != nil {
		return err
	}
	exp, _ := s.fresh()
	return e.Row(connector.Row{"user": s.user, "status": "logged in", "expires": exp.UTC().Format(time.RFC3339)})
}

var refRe = regexp.MustCompile(`^e[0-9]+$`)

// readLocator resolves a declared element, or a snapshot ref (e12) for reads only.
func (s *sess) readLocator(ref string) (string, error) {
	if refRe.MatchString(ref) {
		return ref, nil
	}
	loc, _, _, err := s.w.locator(ref, s.page)
	return loc, err
}

func (s *sess) text(ctx context.Context, c connector.Call, e connector.Emit) error {
	if err := s.ensure(ctx); err != nil {
		return err
	}
	loc, err := s.readLocator(c.Args[0])
	if err != nil {
		return err
	}
	rep, err := s.run(ctx, "eval", "el => el.innerText", loc)
	if err != nil {
		return err
	}
	v, _ := evalValue(rep).(string)
	if max := s.w.t.Limits.Bytes; max > 0 && len(v) > max {
		v = v[:max]
	}
	return stop(e.Row(connector.Row{"element": c.Args[0], "text": s.w.scrub(v)}))
}

func (s *sess) snapshot(ctx context.Context, c connector.Call, e connector.Emit) error {
	if err := s.ensure(ctx); err != nil {
		return err
	}
	args := []string{"snapshot"}
	if len(c.Args) > 0 {
		loc, err := s.readLocator(c.Args[0])
		if err != nil {
			return err
		}
		args = append(args, loc)
	}
	if d := c.Flags["depth"]; d != "" {
		args = append(args, "--depth="+d)
	}
	rep, err := s.run(ctx, args...)
	if err != nil {
		return err
	}
	var snap string
	if len(rep.Snapshot) > 0 {
		_ = jsonString(rep.Snapshot, &snap)
	}
	total := 0
	for _, line := range strings.Split(strings.TrimRight(snap, "\n"), "\n") {
		total += len(line) + 1
		if max := s.w.t.Limits.Bytes; max > 0 && total > max {
			return nil
		}
		if err := e.Text(s.w.scrub(line)); err != nil {
			return stop(err)
		}
	}
	return nil
}

func (s *sess) screenshot(ctx context.Context, c connector.Call, e connector.Emit) error {
	if err := s.ensure(ctx); err != nil {
		return err
	}
	file := filepath.Join(s.w.shots, fmt.Sprintf("%s-%s-%s.png", safe(s.w.t.Env), safe(s.w.t.System), time.Now().UTC().Format("20060102T150405.000")))
	args := []string{"screenshot"}
	if len(c.Args) > 0 {
		loc, err := s.readLocator(c.Args[0])
		if err != nil {
			return err
		}
		args = append(args, loc)
	}
	args = append(args, "--filename="+file)
	if c.Flags["full"] == "true" {
		args = append(args, "--full-page")
	}
	if _, err := s.run(ctx, args...); err != nil {
		return err
	}
	_ = secure.File(file)
	return e.Row(connector.Row{"file": file, "page": s.page})
}

// check runs pages' checks: visible:{locator}, text:{el,contains}, url: substring.
func (s *sess) check(ctx context.Context, c connector.Call, e connector.Emit) error {
	var names []string
	if len(c.Args) == 0 || c.Args[0] == "--all" || c.Args[0] == "all" || c.Flags["all"] == "true" {
		for _, n := range s.w.assets.pageNames() {
			if len(s.w.assets.Pages[n].Checks) > 0 {
				names = append(names, n)
			}
		}
	} else {
		if s.w.assets.Pages[c.Args[0]] == nil {
			return errs.New(errs.NotFound, "no page %q", c.Args[0]).WithHint("run: pages")
		}
		names = []string{c.Args[0]}
	}
	if len(names) == 0 {
		return e.Text("no checks declared")
	}
	if err := s.ensure(ctx); err != nil {
		return err
	}
	failed := 0
	for _, n := range names {
		if _, err := s.navigate(ctx, n); err != nil {
			return err
		}
		for i, chk := range s.w.assets.Pages[n].Checks {
			ok, desc, err := s.runCheck(ctx, chk, n)
			if err != nil {
				return err
			}
			if !ok {
				failed++
			}
			if err := e.Row(connector.Row{"page": n, "check": i + 1, "what": desc, "ok": ok}); err != nil {
				return stop(err)
			}
		}
	}
	if failed > 0 {
		return errs.New(errs.Runtime, "%d check(s) failed", failed)
	}
	return nil
}

func (s *sess) runCheck(ctx context.Context, chk map[string]any, page string) (bool, string, error) {
	for kind, v := range chk {
		switch kind {
		case "visible":
			m, _ := v.(map[string]any)
			loc, err := s.w.specLocator(m, page)
			if err != nil {
				return false, "", err
			}
			ok, err := s.waitVisible(ctx, loc, 5*time.Second)
			return ok, "visible " + loc, err
		case "text":
			m, _ := v.(map[string]any)
			loc, err := s.w.specLocator(m, page)
			if err != nil {
				return false, "", err
			}
			want, _ := m["contains"].(string)
			rep, err := s.run(ctx, "eval", "el => el.innerText", loc)
			if errs.CodeOf(err) == errs.NotFound {
				return false, "text " + loc, nil
			} else if err != nil {
				return false, "", err
			}
			got, _ := evalValue(rep).(string)
			return strings.Contains(got, want), fmt.Sprintf("text %s contains %q", loc, want), nil
		case "url":
			want := fmt.Sprint(v)
			cur, err := s.url(ctx)
			return strings.Contains(cur, want), "url contains " + want, err
		}
		return false, "", errs.New(errs.Config, "page %s: unknown check %q (visible, text, url)", page, kind)
	}
	return false, "", errs.New(errs.Config, "page %s: empty check", page)
}

// interact is click/fill/press/select/do; write=true when called through `tmvp write`.
func (s *sess) interact(ctx context.Context, c connector.Call, verb string, args []string, write bool, e connector.Emit) error {
	w := s.w
	var (
		full, loc string
		mutating  bool
		act       *Action
		actPage   string
		params    = map[string]string{}
	)
	switch verb {
	case "click", "fill", "select":
		if refRe.MatchString(args[0]) {
			return errs.New(errs.Usage, "%s needs a declared element (page.element), not a snapshot ref", verb).
				WithHint("declare it in pages/*.md so its mutating flag is known")
		}
		l, f, el, err := w.locator(args[0], s.page)
		if err != nil {
			return err
		}
		loc, full, mutating = l, f, el.Mutating
		if verb != "click" && len(args) < 2 {
			return errs.New(errs.Usage, "%s <el> <value>", verb)
		}
	case "press":
		full, mutating = "key:"+args[0], true
	case "do":
		a, pg, f, err := w.assets.FindAction(args[0])
		if err != nil {
			return err
		}
		act, actPage, full, mutating = a, pg, f, a.Mutating
		for _, kv := range args[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return errs.New(errs.Usage, "do: parameters are key=value, got %q", kv)
			}
			params[k] = v
		}
		for k, v := range c.Flags {
			if k != "user" {
				if _, set := params[k]; !set {
					params[k] = v
				}
			}
		}
	default:
		return errs.New(errs.Usage, "unknown interaction %q", verb)
	}
	if w.assets.IsNever(full) && !c.Destructive {
		return errs.New(errs.PolicyBlocked, "%s is in never: and %s.%s has destructive: false", full, w.t.Env, w.t.System)
	}
	if act != nil {
		for _, st := range act.Steps {
			if ref := stepElement(st); ref != "" {
				if _, f, err := w.assets.Resolve(ref, actPage); err == nil && w.assets.IsNever(f) && !c.Destructive {
					return errs.New(errs.PolicyBlocked, "action %s touches %s, which is in never: (destructive: false)", full, f)
				}
			}
		}
	}
	if !write && (mutating || !c.Destructive) {
		why := "it is mutating"
		if !mutating {
			why = w.t.Env + " is protected (destructive: false)"
		}
		return errs.New(errs.PolicyBlocked, "%s %s is a write: %s", verb, full, why).
			WithHint(fmt.Sprintf(`tmvp write %s %s %s %s --reason "why"`, w.t.Env, w.t.System, verb, strings.Join(args, " ")))
	}
	if write && w.t.Mode != connector.ModeWrite {
		return errs.New(errs.PolicyBlocked, "%s as a write runs only through tmvp write", verb)
	}
	if c.Preview {
		return e.Row(connector.Row{"action": verb, "target": full, "locator": loc, "mutating": mutating, "user": s.user, "preview": true})
	}
	if err := s.ensure(ctx); err != nil {
		return err
	}
	var err error
	switch verb {
	case "click":
		_, err = s.run(ctx, "click", loc)
	case "fill", "select":
		err = s.fillSecure(ctx, verb, loc, args[1])
	case "press":
		_, err = s.run(ctx, "press", args[0])
	case "do":
		if p := w.assets.Pages[actPage]; p != nil && p.Path != "" && s.page != actPage {
			if _, err := s.navigate(ctx, actPage); err != nil {
				return err
			}
		}
		for i, st := range act.Steps {
			if err = s.step(ctx, st, actPage, params); err != nil {
				err = errs.Wrap(errs.CodeOf(err), err, "%s step %d (%s)", full, i+1, st.Op)
				break
			}
		}
	}
	if err != nil {
		return err
	}
	return e.Row(connector.Row{"action": verb, "target": full, "user": s.user, "ok": true})
}

// fillSecure fills/selects value without putting it on playwright-cli's argv
// (argv is world-readable through ps): the value goes into a 0600 run-code
// script in the 0700 work dir, passed by --filename and removed afterwards.
func (s *sess) fillSecure(ctx context.Context, op, loc, value string) error {
	v, err := json.Marshal(value)
	if err != nil {
		return errs.Wrap(errs.Runtime, err, "web: encode value")
	}
	method := "fill"
	if op == "select" {
		method = "selectOption"
	}
	code := "async page => { await " + jsLocator(loc) + "." + method + "(" + string(v) + "); }\n"
	f, err := os.CreateTemp(s.w.work, "fill-*.js")
	if err != nil {
		return errs.Wrap(errs.Runtime, err, "web: temp script")
	}
	name := f.Name()
	defer os.Remove(name)
	if err := secure.File(name); err != nil {
		f.Close()
		return errs.Wrap(errs.Runtime, err, "web: temp script")
	}
	_, werr := f.WriteString(code)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return errs.Wrap(errs.Runtime, werr, "web: temp script")
	}
	_, err = s.run(ctx, "run-code", "--filename="+name)
	return err
}

// jsLocator turns a locator string (getByRole(...) / css) into page-relative JS.
func jsLocator(loc string) string {
	if strings.HasPrefix(loc, "getBy") {
		return "page." + loc
	}
	return "page.locator(" + jsq(loc) + ")"
}
