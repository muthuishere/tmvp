package config

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
)

// Issue is one validation finding.
type Issue struct {
	Level   string `json:"level"` // "error" | "warn"
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (i Issue) String() string { return fmt.Sprintf("%s %s: %s", i.Level, i.Path, i.Message) }

// ValidateOptions supplies knowledge config cannot import (the connector registry).
type ValidateOptions struct {
	KnownType func(typ string) bool     // nil = accept all
	Verbs     func(typ string) []string // verbs of a type, for collision checks
	Reserved  []string                  // CLI command words (write, validate, config, ...)
}

// SecretishKeys are field names whose values must come from a secret.
var SecretishKeys = map[string]bool{
	"dsn": true, "password": true, "passwd": true, "token": true, "key": true, "creds": true,
	"kubeconfig": true, "credentials": true, "secret": true, "uri": true, "api_key": true,
	"apikey": true, "private_key": true, "client_secret": true, "access_key": true, "secret_key": true,
}

// DefaultReserved are words the CLI owns.
var DefaultReserved = []string{"all", "write", "validate", "config", "check", "logs", "login",
	"secrets", "server", "version", "help", "new", "skill", "export", "completion", "verbs",
	"envs", "systems", "llms", "doctor", "use", "certs"}

// Validate checks schema, refs, name collisions, via cycles and literal
// secrets. It never runs a secret command.
func (c *Config) Validate(o ValidateOptions) []Issue {
	var is []Issue
	add := func(level, p, f string, a ...any) { is = append(is, Issue{level, p, fmt.Sprintf(f, a...)}) }
	if c.Version != 1 {
		add("warn", "version", "expected version: 1, got %d", c.Version)
	}
	if c.Logging.Sink != "" && c.Logging.Sink != "file" && c.Logging.Sink != "s3" {
		add("error", "logging.sink", "unknown sink %q (file|s3)", c.Logging.Sink)
	}
	if c.Dir != "" {
		for i, r := range c.AssetRoots() {
			if len(c.Assets) == 0 {
				break // default <skill>/assets may not exist yet
			}
			if st, err := os.Stat(r); err != nil || !st.IsDir() {
				add("error", fmt.Sprintf("assets.%d", i), "asset path %q does not exist or is not a directory", c.Assets[i])
			}
		}
	}
	if c.DefaultEnv != "" {
		if _, ok := c.Envs[c.DefaultEnv]; !ok {
			add("error", "default_env", "default_env %q is not an env (envs: %s)", c.DefaultEnv, strings.Join(c.EnvOrder, ", "))
		}
	}
	reserved := map[string]string{}
	for _, r := range append(append([]string(nil), DefaultReserved...), o.Reserved...) {
		reserved[r] = "command"
	}
	sysNames := map[string]bool{}
	verbs := map[string]string{}
	for _, en := range c.EnvOrder {
		for sn, s := range c.Envs[en].Systems {
			sysNames[sn] = true
			if o.Verbs != nil {
				for _, v := range o.Verbs(s.Type) {
					verbs[v] = s.Type
				}
			}
		}
	}
	for _, d := range c.Secrets {
		checkSecretDef(d, add)
	}
	validateLimits(c.Limits, "limits", add)
	for _, en := range c.EnvOrder {
		e := c.Envs[en]
		p := "envs." + en
		if strings.ContainsAny(en, ".,*?[ ") {
			add("error", p, "env name must not contain . , * ? [ or spaces")
		}
		if sysNames[en] {
			add("error", p, "env %q collides with a system name", en)
		}
		if reserved[en] != "" {
			add("error", p, "env %q collides with the tmvp command %q", en, en)
		}
		if t, ok := verbs[en]; ok {
			add("error", p, "env %q collides with a %s verb", en, t)
		}
		for _, d := range e.Secrets {
			checkSecretDef(d, add)
		}
		validateLimits(e.Limits, p+".limits", add)
		names := append([]string(nil), e.SystemOrder...)
		sort.Strings(names)
		for _, sn := range names {
			s := e.Systems[sn]
			sp := p + ".systems." + sn
			if strings.ContainsAny(sn, ".,*?[ ") {
				add("error", sp, "system name must not contain . , * ? [ or spaces")
			}
			if reserved[sn] != "" {
				add("error", sp, "system %q collides with the tmvp command %q", sn, sn)
			}
			if t, ok := verbs[sn]; ok {
				add("error", sp, "system %q collides with a %s verb", sn, t)
			}
			if o.KnownType != nil && !o.KnownType(s.Type) {
				add("warn", sp+".type", "unknown type %q (no connector registered)", s.Type)
			}
			for i, v := range s.Via {
				if ch, ok := ParseContainerHop(v); ok {
					if err := ch.Validate(); err != nil {
						add("error", sp+".via", "via %s: %v", v, err)
					}
					if i == 0 || e.Systems[s.Via[i-1]] == nil || e.Systems[s.Via[i-1]].Type != "ssh" {
						add("error", sp+".via", "via %s: a container hop must directly follow an ssh hop (via: [box, {container: name}])", v)
					}
					if i != len(s.Via)-1 {
						add("error", sp+".via", "via %s: a container hop must be the last hop", v)
					}
					continue
				}
				hop := e.Systems[v]
				if hop == nil {
					add("error", sp+".via", "via %q: no such system in env %s", v, en)
				} else if hop.Type != "ssh" && hop.Type != "portforward" && hop.Type != "k8s" {
					add("warn", sp+".via", "via %q has type %s (expected ssh, portforward or k8s)", v, hop.Type)
				}
			}
			if s.Login != nil && s.Login.Check == "" && s.Login.Manual != "browser" {
				add("warn", sp+".login", "login.manual without login.check: tmvp cannot tell when login is needed")
			}
			if lm, ok := s.Fields["limits"].(map[string]any); ok {
				validateLimits(lm, sp+".limits", add)
			}
			checkFields(c, en, sp, "", s.Fields, add)
		}
		for _, cyc := range viaCycles(e) {
			add("error", p+".systems", "via cycle: %s", strings.Join(cyc, " -> "))
		}
	}
	return is
}

func checkSecretDef(d *SecretDef, add func(level, p, f string, a ...any)) {
	if d.Anonymous || d.DotenvKey != "" {
		return
	}
	if d.Command == "" {
		refs := Refs(d.Expr)
		if len(refs) == 0 {
			add("error", d.Origin, "literal secret value; use \"$(command)\" that prints it, or \"$dotenv(.env.prod, KEY)\"")
			return
		}
		for _, r := range refs {
			if r.Kind != RefEnvVar {
				add("error", d.Origin, "a secret must be $(cmd), $file(cmd), $dotenv(file, KEY) or ${ENV_VAR}")
			}
		}
	}
	if !d.File && strings.Contains(d.Command, "${out}") {
		add("warn", d.Origin, "${out} only has meaning inside $file(cmd)")
	}
}

func checkFields(c *Config, env, base, prefix string, m map[string]any, add func(level, p, f string, a ...any)) {
	for k, v := range m {
		p := k
		if prefix != "" {
			p = prefix + "." + k
		}
		switch t := v.(type) {
		case map[string]any:
			checkFields(c, env, base, p, t, add)
		case string:
			for _, r := range Refs(t) {
				switch r.Kind {
				case RefInvalid:
					add("error", base+"."+p, "invalid reference ${%s}", r.Raw)
				case RefVar:
					if _, ok := c.lookupVar(env, r.Name); !ok {
						add("error", base+"."+p, "unknown var %q", r.Name)
					}
				case RefSecret, RefFileSecret:
					if c.LookupSecret(env, r.Name) == nil {
						add("error", base+"."+p, "unknown secret %q", r.Name)
					}
				}
			}
			if _, ok := FileRef(t); !ok {
				for _, r := range Refs(t) {
					if r.Kind == RefFileSecret {
						add("error", base+"."+p, "${file:secrets.%s} must be the whole field", r.Name)
					}
				}
			}
			if lit := literalSecret(k, t); lit != "" {
				add("error", base+"."+p, "%s; use ${secrets.x} or \"$(command)\"", lit)
			}
		}
	}
}

// literalSecret returns a reason if a secret-ish field holds a literal value.
func literalSecret(key, v string) string {
	if v == "" {
		return ""
	}
	k := strings.ToLower(key)
	hasRef := false
	for _, r := range Refs(v) {
		if r.Kind == RefSecret || r.Kind == RefFileSecret || r.Kind == RefEnvVar {
			hasRef = true
		}
	}
	if SecretishKeys[k] && !hasRef {
		return "literal value in secret field " + key
	}
	// url-ish values with userinfo password, any key
	if !hasRef && strings.Contains(v, "://") {
		if u, err := url.Parse(v); err == nil && u.User != nil {
			if _, has := u.User.Password(); has {
				return "credentials embedded in URL"
			}
		}
	}
	return ""
}

func viaCycles(e *Env) [][]string {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	var out [][]string
	var stack []string
	var visit func(n string)
	visit = func(n string) {
		color[n] = grey
		stack = append(stack, n)
		if s := e.Systems[n]; s != nil {
			for _, v := range s.Via {
				if e.Systems[v] == nil {
					continue
				}
				switch color[v] {
				case white:
					visit(v)
				case grey:
					i := len(stack) - 1
					for i >= 0 && stack[i] != v {
						i--
					}
					cyc := append(append([]string(nil), stack[i:]...), v)
					out = append(out, cyc)
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
	}
	names := append([]string(nil), e.SystemOrder...)
	sort.Strings(names)
	for _, n := range names {
		if color[n] == white {
			visit(n)
		}
	}
	return out
}

// ViaChain returns the full ordered hop list for system (hops' own via first),
// or an error on cycles/unknown hops.
func (c *Config) ViaChain(env, system string) ([]*System, error) {
	e := c.Envs[env]
	if e == nil {
		return nil, fmt.Errorf("unknown env %s", env)
	}
	var out []*System
	seen := map[string]bool{}
	onPath := map[string]bool{}
	var walk func(n string, depth int) error
	walk = func(n string, depth int) error {
		if depth > 16 {
			return fmt.Errorf("via chain too deep at %s", n)
		}
		s := e.Systems[n]
		if s == nil {
			return fmt.Errorf("via %q: no such system in env %s", n, env)
		}
		onPath[n] = true
		defer delete(onPath, n)
		for i, v := range s.Via {
			if ch, ok := ParseContainerHop(v); ok {
				if err := ch.Validate(); err != nil {
					return err
				}
				if i == 0 || e.Systems[s.Via[i-1]] == nil || e.Systems[s.Via[i-1]].Type != "ssh" {
					return fmt.Errorf("via %s: a container hop must directly follow an ssh hop", v)
				}
				out = append(out, ch.System(env))
				continue
			}
			if onPath[v] {
				return fmt.Errorf("via cycle at %s", v)
			}
			if err := walk(v, depth+1); err != nil {
				return err
			}
			if !seen[v] {
				seen[v] = true
				out = append(out, e.Systems[v])
			}
		}
		return nil
	}
	if err := walk(system, 0); err != nil {
		return nil, err
	}
	return out, nil
}

// LiteralSecret is the literal-secret check `validate` runs on config fields
// (secret-ish key with a literal value, URL with user:password@). Returns a
// reason, or "" when v is fine. Used by `tmvp skill lint` on asset files.
func LiteralSecret(key, v string) string { return literalSecret(key, v) }
