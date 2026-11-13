package config

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// ShowResolved renders the merged config as YAML with vars and env vars
// expanded and every secret masked: "<secret:name>", "<file:secret:name>".
// Env vars inside secret-ish fields are masked as "<env:NAME>". Secret
// commands themselves are shown (they are not values).
func (c *Config) ShowResolved() (string, error) {
	out := map[string]any{"version": c.Version, "product": c.Product}
	if c.Logging.Sink != "" || c.Logging.Path != "" {
		out["logging"] = map[string]any{"sink": c.Logging.Sink, "path": c.Logging.Path}
	}
	out["vars"] = c.Vars
	out["secrets"] = secretCmds(c.Secrets, false)
	if len(c.Limits) > 0 {
		out["limits"] = c.Limits
	}
	envs := yaml.Node{Kind: yaml.MappingNode}
	for _, en := range c.EnvOrder {
		e := c.Envs[en]
		systems := yaml.Node{Kind: yaml.MappingNode}
		for _, sn := range e.SystemOrder {
			s := e.Systems[sn]
			m := map[string]any{"type": s.Type}
			if len(s.Via) > 0 {
				m["via"] = s.Via
			}
			if len(s.Labels) > 0 {
				m["labels"] = s.Labels
			}
			if s.Login != nil {
				m["login"] = s.Login
			}
			if s.Destructive != nil {
				m["destructive"] = *s.Destructive
			}
			for k, v := range s.Fields {
				m[k] = c.showValue(en, k, v)
			}
			// computed, not config: the effective per-call timeout and where it comes from
			d, src := c.ConfigTimeout(en, sn)
			m["effective_timeout"] = d.String() + " (" + src + ")"
			systems.Content = append(systems.Content, scalar(sn), toNode(m))
		}
		em := map[string]any{"destructive": e.Destructive}
		if e.Type != "" {
			em["type"] = e.Type
		}
		if len(e.Vars) > 0 {
			em["vars"] = e.Vars
		}
		if len(e.Limits) > 0 {
			em["limits"] = e.Limits
		}
		if sc := secretCmds(e.Secrets, true); len(sc) > 0 {
			em["secrets"] = sc
		}
		en1 := toNode(em)
		en1.Content = append(en1.Content, scalar("systems"), &systems)
		envs.Content = append(envs.Content, scalar(en), en1)
	}
	root := toNode(out)
	root.Content = append(root.Content, scalar("envs"), &envs)
	b, err := yaml.Marshal(root)
	return string(b), err
}

func scalar(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: s} }

func toNode(v any) *yaml.Node {
	var n yaml.Node
	_ = n.Encode(v)
	return &n
}

func secretCmds(m map[string]*SecretDef, skipAnon bool) map[string]string {
	out := map[string]string{}
	for n, d := range m {
		if skipAnon && d.Anonymous {
			continue
		}
		switch {
		case d.File:
			out[n] = "$file(" + d.Command + ")"
		case d.Command != "":
			out[n] = "$(" + d.Command + ")"
		case d.DotenvKey != "":
			out[n] = "$dotenv(" + d.DotenvFile + ", " + d.DotenvKey + ")"
		default:
			out[n] = "<env-backed secret>"
		}
	}
	return out
}

func (c *Config) showValue(env, key string, v any) any {
	switch t := v.(type) {
	case string:
		secretish := SecretishKeys[strings.ToLower(key)]
		s, err := Expand(t, func(r Ref) (string, bool, error) {
			switch r.Kind {
			case RefSecret:
				return "<secret:" + r.Name + ">", false, nil
			case RefFileSecret:
				return "<file:secret:" + r.Name + ">", false, nil
			case RefVar:
				if secretish {
					// a var may itself pull in ${ENV}: never expand into a secret-ish field
					return "<var:" + r.Name + ">", false, nil
				}
				val, err := c.ExpandField(env, "${vars."+r.Name+"}", nil)
				if err != nil {
					return "<unknown var " + r.Name + ">", false, nil
				}
				return val, false, nil
			case RefEnvVar:
				if secretish || secretishEnvName(r.Name) {
					return "<env:" + r.Name + ">", false, nil
				}
				val, err := c.ExpandField(env, "${"+r.Raw+"}", nil)
				if err != nil {
					return "<unset:" + r.Name + ">", false, nil
				}
				return val, false, nil
			}
			return "", true, nil
		})
		if err != nil {
			return fmt.Sprintf("<error: %v>", err)
		}
		return urlPassword.ReplaceAllString(s, "://$1:***@")
	case map[string]any:
		m := map[string]any{}
		for k, x := range t {
			m[k] = c.showValue(env, k, x)
		}
		return m
	case []any:
		l := make([]any, len(t))
		for i, x := range t {
			l[i] = c.showValue(env, key, x)
		}
		return l
	}
	return v
}

// urlPassword matches the password part of scheme://user:password@ in a
// resolved value (a var or env var can carry one into any field).
var urlPassword = regexp.MustCompile(`://([^/:@\s]*):[^/@\s]+@`)

// secretishEnvName reports whether an env var name looks like it holds a
// credential (DB_PASSWORD, API_KEY, GITHUB_TOKEN ...): masked in any field.
func secretishEnvName(n string) bool {
	u := strings.ToUpper(n)
	for _, w := range []string{"PASSWORD", "PASSWD", "PASS", "_PW", "SECRET", "TOKEN", "KEY", "CRED", "AUTH", "DSN", "PRIVATE"} {
		if strings.Contains(u, w) {
			return true
		}
	}
	return false
}
