package config

import (
	"path"
	"sort"
	"strings"

	"github.com/muthuishere/tmvp/internal/errs"
)

// Target is one selected (env, system) pair. System is nil when the env has
// no system of that name (the batch reports it as "skipped").
type Target struct {
	Env    string
	Name   string
	System *System
}

// Selector selects targets. Env/System are patterns:
//
//	Env:    "" / "all" / "*" = every env; "prod"; "prod,staging"; globs ("prod*")
//	System: "" / "*" = every system; "db"; globs ("db*")
//
// Labels further filters systems.
type Selector struct {
	Env    string
	System string
	Labels LabelSelector
}

// ParseSelector parses the combined forms "prod", "prod,staging", "all",
// "prod.db", "prod.*", "*.db". Plain words without a dot select envs only.
func ParseSelector(s string) Selector {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return Selector{Env: s[:i], System: s[i+1:]}
	}
	return Selector{Env: s}
}

// IsEnvSelector reports whether word could be an env selector for c
// ("all", "*", a known env, a comma list of envs, a dotted selector, or a glob).
func (c *Config) IsEnvSelector(word string) bool {
	if word == "all" || word == "*" || strings.ContainsAny(word, ".*?[") || strings.HasPrefix(word, "type:") {
		return true
	}
	for _, p := range strings.Split(word, ",") {
		if _, ok := c.Envs[p]; !ok && len(c.envsOfType(NormalizeEnvType(p))) == 0 {
			return false
		}
	}
	return word != ""
}

// HasSystem reports whether any env defines a system named name.
func (c *Config) HasSystem(name string) bool {
	for _, e := range c.Envs {
		if _, ok := e.Systems[name]; ok {
			return true
		}
	}
	return false
}

// Select resolves sel to targets in config order.
func (c *Config) Select(sel Selector) ([]Target, error) {
	envs, err := c.matchEnvs(sel.Env)
	if err != nil {
		return nil, err
	}
	sys := sel.System
	exact := sys != "" && sys != "*" && !strings.ContainsAny(sys, "*?[")
	var out []Target
	for _, en := range envs {
		e := c.Envs[en]
		if exact {
			s := e.Systems[sys]
			if s != nil && !sel.Labels.Matches(s.Labels) {
				continue
			}
			out = append(out, Target{Env: en, Name: sys, System: s})
			continue
		}
		for _, sn := range e.SystemOrder {
			if sys != "" && sys != "*" {
				if ok, _ := path.Match(sys, sn); !ok {
					continue
				}
			}
			s := e.Systems[sn]
			if !sel.Labels.Matches(s.Labels) {
				continue
			}
			out = append(out, Target{Env: en, Name: sn, System: s})
		}
	}
	if exact && !c.HasSystem(sys) {
		return nil, errs.New(errs.NotFound, "no system named %q in any env", sys)
	}
	if len(out) == 0 {
		return nil, errs.New(errs.NotFound, "selector matched nothing (envs=%q system=%q)", sel.Env, sel.System)
	}
	return out, nil
}

func (c *Config) matchEnvs(p string) ([]string, error) {
	if p == "" || p == "all" || p == "*" {
		return append([]string(nil), c.EnvOrder...), nil
	}
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.Split(p, ",") {
		part = strings.TrimSpace(part)
		matched := false
		// type:prod = every env of that type; a bare word that is no env's
		// name but is an env type does the same (prod → prod-eu, prod-us…).
		if t, isType := strings.CutPrefix(part, "type:"); isType || c.Envs[part] == nil {
			if !isType {
				t = part
			}
			for _, en := range c.envsOfType(NormalizeEnvType(t)) {
				matched = true
				if !seen[en] {
					seen[en] = true
					out = append(out, en)
				}
			}
			if isType && !matched {
				return nil, errs.New(errs.NotFound, "no env has type %q", t).WithHint("set type: on envs in config.yaml")
			}
			if matched {
				continue
			}
		}
		for _, en := range c.EnvOrder {
			ok := en == part
			if !ok && strings.ContainsAny(part, "*?[") {
				ok, _ = path.Match(part, en)
			}
			if ok {
				matched = true
				if !seen[en] {
					seen[en] = true
					out = append(out, en)
				}
			}
		}
		if !matched {
			known := append([]string(nil), c.EnvOrder...)
			sort.Strings(known)
			return nil, errs.New(errs.NotFound, "unknown env %q", part).WithHint("envs: " + strings.Join(known, ", "))
		}
	}
	return out, nil
}

// envsOfType lists envs whose type is t (config order).
func (c *Config) envsOfType(t string) []string {
	if t == "" {
		return nil
	}
	var out []string
	for _, en := range c.EnvOrder {
		if c.Envs[en].Type == t {
			out = append(out, en)
		}
	}
	return out
}

// LabelSelector is a parsed -l expression (AND of requirements).
type LabelSelector []LabelReq

// LabelReq is one requirement.
type LabelReq struct {
	Key    string
	Op     string // "=", "!=", "in", "notin", "exists", "!exists"
	Values []string
}

// ParseLabels parses "k=v,k2!=v2,k3 in (a,b),k4 notin (c),k5,!k6".
func ParseLabels(s string) (LabelSelector, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var parts []string
	depth, start := 0, 0
	for i, ch := range s {
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, s[start:])
	var out LabelSelector
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		var r LabelReq
		low := p
		switch {
		case strings.Contains(low, " notin "):
			i := strings.Index(low, " notin ")
			r = LabelReq{Key: strings.TrimSpace(p[:i]), Op: "notin"}
			vals, err := parseSet(p[i+7:])
			if err != nil {
				return nil, err
			}
			r.Values = vals
		case strings.Contains(low, " in "):
			i := strings.Index(low, " in ")
			r = LabelReq{Key: strings.TrimSpace(p[:i]), Op: "in"}
			vals, err := parseSet(p[i+4:])
			if err != nil {
				return nil, err
			}
			r.Values = vals
		case strings.Contains(p, "!="):
			kv := strings.SplitN(p, "!=", 2)
			r = LabelReq{Key: strings.TrimSpace(kv[0]), Op: "!=", Values: []string{strings.TrimSpace(kv[1])}}
		case strings.Contains(p, "="):
			kv := strings.SplitN(strings.Replace(p, "==", "=", 1), "=", 2)
			r = LabelReq{Key: strings.TrimSpace(kv[0]), Op: "=", Values: []string{strings.TrimSpace(kv[1])}}
		case strings.HasPrefix(p, "!"):
			r = LabelReq{Key: strings.TrimSpace(p[1:]), Op: "!exists"}
		default:
			r = LabelReq{Key: p, Op: "exists"}
		}
		if r.Key == "" {
			return nil, errs.New(errs.Usage, "bad label selector %q", p)
		}
		out = append(out, r)
	}
	return out, nil
}

func parseSet(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "(") || !strings.HasSuffix(s, ")") {
		return nil, errs.New(errs.Usage, "label set must be (a,b): %q", s)
	}
	var out []string
	for _, v := range strings.Split(s[1:len(s)-1], ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out, nil
}

// Matches reports whether labels satisfy every requirement.
func (ls LabelSelector) Matches(labels map[string]string) bool {
	for _, r := range ls {
		v, has := labels[r.Key]
		switch r.Op {
		case "=":
			if !has || v != r.Values[0] {
				return false
			}
		case "!=":
			if has && v == r.Values[0] {
				return false
			}
		case "in":
			if !has || !contains(r.Values, v) {
				return false
			}
		case "notin":
			if has && contains(r.Values, v) {
				return false
			}
		case "exists":
			if !has {
				return false
			}
		case "!exists":
			if has {
				return false
			}
		}
	}
	return true
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// MatchEnvs resolves an env selector ("", "all", "prod", "dev,prod",
// "prod*") to env names in config order.
func (c *Config) MatchEnvs(sel string) ([]string, error) { return c.matchEnvs(sel) }
