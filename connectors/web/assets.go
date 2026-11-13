package web

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// Element is one locator from pages/*.md. Priority: testid → role(+name) → text → css.
type Element struct {
	TestID   string `yaml:"testid"`
	Role     string `yaml:"role"`
	Name     string `yaml:"name"`
	Text     string `yaml:"text"`
	CSS      string `yaml:"css"`
	Exact    bool   `yaml:"exact"`
	Mutating bool   `yaml:"mutating"`
}

// Locator renders the element as a playwright-cli target.
func (e Element) Locator() (string, error) {
	switch {
	case e.TestID != "":
		return "getByTestId(" + jsq(e.TestID) + ")", nil
	case e.Role != "":
		opts := []string{}
		if e.Name != "" {
			opts = append(opts, "name: "+jsq(e.Name))
		}
		if e.Exact {
			opts = append(opts, "exact: true")
		}
		if len(opts) == 0 {
			return "getByRole(" + jsq(e.Role) + ")", nil
		}
		return "getByRole(" + jsq(e.Role) + ", { " + strings.Join(opts, ", ") + " })", nil
	case e.Text != "":
		if e.Exact {
			return "getByText(" + jsq(e.Text) + ", { exact: true })", nil
		}
		return "getByText(" + jsq(e.Text) + ")", nil
	case e.CSS != "":
		return e.CSS, nil
	}
	return "", fmt.Errorf("element has no locator (testid, role, text or css)")
}

func jsq(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\n", `\n`, "\r", `\r`)
	return "'" + r.Replace(s) + "'"
}

// Step is one entry of a login/action step list, e.g. {fill: {el: email, value: "x"}}.
type Step struct {
	Op  string
	Arg any
}

// Action is a named step list from pages/*.md.
type Action struct {
	Steps    []Step
	Mutating bool // declared, or any step touches a mutating element (computed)
}

// Page is pages/<name>.md front-matter.
type Page struct {
	Name     string
	Path     string
	Elements map[string]Element
	Actions  map[string]*Action
	Checks   []map[string]any
	Source   string // file it came from (which asset repo)
}

// LoginSpec is context.md login:.
type LoginSpec struct {
	Manual  string // "browser" = human logs in (context.md or system field)
	Page    string
	Steps   []Step
	Success map[string]any
	Reuse   bool
	TTL     time.Duration
}

// Assets is everything under assets/<system>/.
type Assets struct {
	Login *LoginSpec
	Never map[string]bool
	Pages map[string]*Page
}

// DefaultTTL applies when session.ttl is not declared.
const DefaultTTL = 12 * time.Hour

// LoadAssets reads context.md and pages/*.md from each asset dir in order
// (one per asset root). The first context.md wins; a page name defined in an
// earlier dir wins over a later one. Missing dirs are empty.
func LoadAssets(dirs ...string) (*Assets, error) {
	a := &Assets{Never: map[string]bool{}, Pages: map[string]*Page{}}
	for _, dir := range dirs {
		if a.Login != nil || len(a.Never) > 0 {
			break
		}
		if b, err := os.ReadFile(filepath.Join(dir, "context.md")); err == nil {
			if err := a.parseContext(b); err != nil {
				return nil, errs.Wrap(errs.Config, err, "assets %s", filepath.Join(dir, "context.md"))
			}
			break
		} else if !os.IsNotExist(err) {
			return nil, errs.Wrap(errs.Config, err, "assets context.md")
		}
	}
	for _, dir := range dirs {
		files, _ := filepath.Glob(filepath.Join(dir, "pages", "*.md"))
		sort.Strings(files)
		local := map[string]bool{}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				return nil, errs.Wrap(errs.Config, err, "assets %s", filepath.Base(f))
			}
			p, err := parsePage(b, strings.TrimSuffix(filepath.Base(f), ".md"))
			if err != nil {
				return nil, errs.Wrap(errs.Config, err, "assets pages/%s", filepath.Base(f))
			}
			if local[p.Name] {
				return nil, errs.New(errs.Config, "assets: page %q declared twice in %s", p.Name, dir)
			}
			local[p.Name] = true
			if _, earlier := a.Pages[p.Name]; earlier {
				continue // an earlier asset root wins
			}
			p.Source = f
			a.Pages[p.Name] = p
		}
	}
	// actions are mutating when they touch a mutating element
	for _, p := range a.Pages {
		for _, act := range p.Actions {
			for _, s := range act.Steps {
				if ref := stepElement(s); ref != "" {
					if el, _, err := a.Resolve(ref, p.Name); err == nil && el.Mutating {
						act.Mutating = true
					}
				}
			}
		}
	}
	return a, nil
}

func (a *Assets) parseContext(b []byte) error {
	fm, _ := connector.SplitFrontMatter(string(b))
	var raw struct {
		Login *struct {
			Manual  string         `yaml:"manual"`
			Page    string         `yaml:"page"`
			Steps   []any          `yaml:"steps"`
			Success map[string]any `yaml:"success"`
			Session struct {
				Reuse *bool  `yaml:"reuse"`
				TTL   string `yaml:"ttl"`
			} `yaml:"session"`
		} `yaml:"login"`
		Never []string `yaml:"never"`
	}
	if err := yaml.Unmarshal([]byte(fm), &raw); err != nil {
		return err
	}
	for _, n := range raw.Never {
		a.Never[n] = true
	}
	if raw.Login == nil {
		return nil
	}
	l := &LoginSpec{Manual: raw.Login.Manual, Page: raw.Login.Page, Success: raw.Login.Success, Reuse: true, TTL: DefaultTTL}
	if raw.Login.Session.Reuse != nil {
		l.Reuse = *raw.Login.Session.Reuse
	}
	if raw.Login.Session.TTL != "" {
		d, err := parseTTL(raw.Login.Session.TTL)
		if err != nil {
			return fmt.Errorf("login.session.ttl: %w", err)
		}
		l.TTL = d
	}
	steps, err := parseSteps(raw.Login.Steps)
	if err != nil {
		return fmt.Errorf("login.steps: %w", err)
	}
	l.Steps = steps
	a.Login = l
	return nil
}

func parsePage(b []byte, file string) (*Page, error) {
	fm, _ := connector.SplitFrontMatter(string(b))
	var raw struct {
		Page     string             `yaml:"page"`
		Path     string             `yaml:"path"`
		Elements map[string]Element `yaml:"elements"`
		Actions  map[string]any     `yaml:"actions"`
		Checks   []map[string]any   `yaml:"checks"`
	}
	if err := yaml.Unmarshal([]byte(fm), &raw); err != nil {
		return nil, err
	}
	p := &Page{Name: raw.Page, Path: raw.Path, Elements: raw.Elements, Checks: raw.Checks, Actions: map[string]*Action{}}
	if p.Name == "" {
		p.Name = file
	}
	if strings.Contains(p.Name, ".") {
		return nil, fmt.Errorf("page name %q must not contain '.'", p.Name)
	}
	if p.Elements == nil {
		p.Elements = map[string]Element{}
	}
	for n, el := range p.Elements {
		if strings.Contains(n, ".") {
			return nil, fmt.Errorf("element name %q must not contain '.'", n)
		}
		if _, err := el.Locator(); err != nil {
			return nil, fmt.Errorf("element %s: %w", n, err)
		}
	}
	for n, v := range raw.Actions {
		act := &Action{}
		var list []any
		switch t := v.(type) {
		case []any:
			list = t
		case map[string]any:
			if m, ok := t["mutating"].(bool); ok {
				act.Mutating = m
			}
			list, _ = t["steps"].([]any)
		default:
			return nil, fmt.Errorf("action %s: want a step list", n)
		}
		steps, err := parseSteps(list)
		if err != nil {
			return nil, fmt.Errorf("action %s: %w", n, err)
		}
		act.Steps = steps
		p.Actions[n] = act
	}
	return p, nil
}

var stepOps = map[string]bool{"goto": true, "fill": true, "click": true, "press": true, "select": true, "wait": true, "check": true, "uncheck": true}

func parseSteps(list []any) ([]Step, error) {
	out := make([]Step, 0, len(list))
	for i, it := range list {
		m, ok := it.(map[string]any)
		if !ok || len(m) != 1 {
			return nil, fmt.Errorf("step %d: want one {op: arg}", i+1)
		}
		for op, arg := range m {
			if !stepOps[op] {
				return nil, fmt.Errorf("step %d: unknown op %q", i+1, op)
			}
			out = append(out, Step{Op: op, Arg: arg})
		}
	}
	return out, nil
}

// stepElement returns the element ref a step acts on ("" if none).
func stepElement(s Step) string {
	switch a := s.Arg.(type) {
	case string:
		if s.Op == "click" || s.Op == "check" || s.Op == "uncheck" {
			return a
		}
	case map[string]any:
		if s.Op == "wait" {
			return ""
		}
		if el, ok := a["el"].(string); ok {
			return el
		}
	}
	return ""
}

// Resolve finds "page.element" or "element" (current page first, then a unique match).
func (a *Assets) Resolve(ref, current string) (Element, string, error) {
	if pg, el, ok := strings.Cut(ref, "."); ok {
		p := a.Pages[pg]
		if p == nil {
			return Element{}, "", errs.New(errs.NotFound, "no page %q", pg).WithHint("run: pages")
		}
		e, ok := p.Elements[el]
		if !ok {
			return Element{}, "", errs.New(errs.NotFound, "no element %q on page %q", el, pg).WithHint("run: elements " + pg)
		}
		return e, pg + "." + el, nil
	}
	if p := a.Pages[current]; p != nil {
		if e, ok := p.Elements[ref]; ok {
			return e, current + "." + ref, nil
		}
	}
	var found []string
	var hit Element
	for _, pn := range a.pageNames() {
		if e, ok := a.Pages[pn].Elements[ref]; ok {
			found = append(found, pn+"."+ref)
			hit = e
		}
	}
	switch len(found) {
	case 1:
		return hit, found[0], nil
	case 0:
		return Element{}, "", errs.New(errs.NotFound, "no element %q in any page", ref).WithHint("run: pages / elements <page>")
	}
	return Element{}, "", errs.New(errs.Usage, "element %q is ambiguous: %s", ref, strings.Join(found, ", ")).WithHint("use page.element")
}

// FindAction finds "page.action" or a unique "action".
func (a *Assets) FindAction(ref string) (*Action, string, string, error) {
	if pg, an, ok := strings.Cut(ref, "."); ok {
		if p := a.Pages[pg]; p != nil {
			if act := p.Actions[an]; act != nil {
				return act, pg, ref, nil
			}
		}
		return nil, "", "", errs.New(errs.NotFound, "no action %q", ref)
	}
	var hits []string
	var act *Action
	var page string
	for _, pn := range a.pageNames() {
		if x := a.Pages[pn].Actions[ref]; x != nil {
			hits, act, page = append(hits, pn+"."+ref), x, pn
		}
	}
	switch len(hits) {
	case 1:
		return act, page, hits[0], nil
	case 0:
		return nil, "", "", errs.New(errs.NotFound, "no action %q in any page", ref).WithHint("run: pages")
	}
	return nil, "", "", errs.New(errs.Usage, "action %q is ambiguous: %s", ref, strings.Join(hits, ", ")).WithHint("use page.action")
}

func (a *Assets) pageNames() []string {
	out := make([]string, 0, len(a.Pages))
	for n := range a.Pages {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// IsNever reports whether ref (page.element / page.action, or bare name) is in never:.
func (a *Assets) IsNever(full string) bool {
	if a.Never[full] {
		return true
	}
	_, short, _ := strings.Cut(full, ".")
	return short != "" && a.Never[short]
}

func parseTTL(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if n, ok := strings.CutSuffix(s, "d"); ok {
		v, err := strconv.Atoi(n)
		if err != nil || v <= 0 {
			return 0, fmt.Errorf("bad ttl %q", s)
		}
		return time.Duration(v) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("bad ttl %q", s)
	}
	return d, nil
}
