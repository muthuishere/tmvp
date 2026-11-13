// Package config loads, interpolates, selects and validates a product's
// config.yaml (DESIGN §3).
//
// The config holds no secret values: secrets are commands ("$(cmd)",
// "$file(cmd)") that the secret store runs lazily. Interpolation happens AFTER
// YAML parsing, per string field, so a value can never change structure.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/muthuishere/tmvp/internal/errs"
)

// Config is one product's parsed config.
type Config struct {
	Version int               `yaml:"version"`
	Product string            `yaml:"product"`
	Logging Logging           `yaml:"logging"`
	Server  ServerConfig      `yaml:"server"`
	Vars    map[string]string `yaml:"vars"`
	// DefaultEnv is the env selected when nothing else selects one
	// (precedence: positional/--env > TMVP_ENV > `tmvp use` > default_env).
	DefaultEnv string `yaml:"default_env"`
	// Limits are product-wide guard defaults (timeout, cost calibration, ...);
	// env and system `limits:` override them key by key (see limits.go).
	Limits map[string]any `yaml:"limits"`
	// Assets are extra asset roots (relative to the config file), searched in
	// order for saved queries/scripts/pages/apis; default [<skill dir>/assets].
	Assets []string `yaml:"assets"`
	// Secrets: top-level named secrets (scope "").
	Secrets map[string]*SecretDef `yaml:"-"`
	Envs    map[string]*Env       `yaml:"-"`
	// EnvOrder is the env order as written in the file.
	EnvOrder []string `yaml:"-"`

	Path string `yaml:"-"` // absolute path of config.yaml
	Dir  string `yaml:"-"` // directory of config.yaml (skill root; assets/ lives here)
	Hash string `yaml:"-"` // sha256 of the file bytes
}

// Logging selects the audit sink.
type Logging struct {
	Sink string `yaml:"sink"` // "file" (default) | "s3" (later)
	Path string `yaml:"path"` // default ~/.tmvp/<product>/logs
}

// ServerConfig tunes the local server.
type ServerConfig struct {
	Idle string `yaml:"idle"` // e.g. "30m"
}

// IdleTimeout returns the configured idle timeout or 30m.
func (s ServerConfig) IdleTimeout() time.Duration {
	if d, err := time.ParseDuration(s.Idle); err == nil && d > 0 {
		return d
	}
	return 30 * time.Minute
}

// Env is one environment.
type Env struct {
	Name string
	// Type is the env's kind (NormalizeEnvType): prod, staging, dev, test,
	// local, or a custom word; "" when unset. Many envs may share a type
	// (prod-eu, prod-us, … are all "prod"); select them all with type:prod.
	Type        string
	Destructive bool
	Vars        map[string]string
	Limits      map[string]any // env-level `limits:` (between global and system)
	Secrets     map[string]*SecretDef
	Systems     map[string]*System
	SystemOrder []string
}

// Login is a system's manual-login declaration (DESIGN §8.1).
type Login struct {
	Manual string `yaml:"manual" json:"manual,omitempty"` // command the USER runs, or "browser"
	Check  string `yaml:"check" json:"check,omitempty"`   // non-interactive command; non-zero exit = not logged in
}

// System is one named system in an env: a type plus fields.
type System struct {
	Name        string
	Env         string
	Type        string
	Via         []string
	Labels      map[string]string
	Login       *Login
	Destructive *bool // system-level override of env destructive
	// Fields holds every other key (read/write sub-maps, limits, host, dsn, ...)
	// exactly as parsed. Inline $(cmd) strings have been rewritten to
	// ${secrets.<anon>} references.
	Fields map[string]any
}

// SecretDef is one named secret slot.
type SecretDef struct {
	Name    string // name as referenced by ${secrets.name}
	Scope   string // "" for top-level, else env name
	Command string // inner command text of $(...) / $file(...)
	File    bool   // $file(cmd): command writes ${out}
	Expr    string // non-command value (e.g. "${VAR}"); validate rejects plain literals
	// DotenvFile/DotenvKey: "$dotenv(.env.prod, PGPASSWORD)" — one key read from
	// a dotenv file (path relative to the repo root holding the config) by the
	// server; the value never reaches the client or the agent.
	DotenvFile string
	DotenvKey  string
	Anonymous  bool   // created from an inline $(cmd) in a field
	Origin     string // config path where it was defined, for messages
}

// Key is the store key of this slot ("scope/name").
func (d *SecretDef) Key() string { return d.Scope + "/" + d.Name }

// Fingerprint changes whenever the definition changes.
func (d *SecretDef) Fingerprint() string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%v|%s|%s|%s|%s", d.File, d.Command, d.Expr, d.DotenvFile, d.DotenvKey)))
	return hex.EncodeToString(h[:8])
}

// IsDestructive reports whether destructive operations are allowed on s.
func (c *Config) IsDestructive(env, system string) bool {
	e := c.Envs[env]
	if e == nil {
		return false
	}
	if s := e.Systems[system]; s != nil && s.Destructive != nil {
		return *s.Destructive
	}
	return e.Destructive
}

// NormalizeEnvType maps common spellings to one type word:
// production/prod/prd/live → prod; staging/stage/stg/uat/preprod/qa → staging;
// development/develop/dev → dev; test/testing/ci → test; local → local.
// Anything else is kept (lower-cased) as a custom type.
func NormalizeEnvType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	switch t {
	case "production", "prod", "prd", "live":
		return "prod"
	case "staging", "stage", "stg", "uat", "preprod", "pre-prod", "qa":
		return "staging"
	case "development", "develop", "dev":
		return "dev"
	case "test", "testing", "ci":
		return "test"
	}
	return t
}

// IsProtected reports whether env is protected (env-level destructive: false,
// the default). Any write to it needs --force in addition to --reason,
// however the env was chosen.
func (c *Config) IsProtected(env string) bool {
	e := c.Envs[env]
	return e == nil || !e.Destructive
}

// System returns env/system or nil.
func (c *Config) System(env, name string) *System {
	if e := c.Envs[env]; e != nil {
		return e.Systems[name]
	}
	return nil
}

// LookupSecret returns the effective secret def for env (env overrides top-level).
func (c *Config) LookupSecret(env, name string) *SecretDef {
	if e := c.Envs[env]; e != nil {
		if d := e.Secrets[name]; d != nil {
			return d
		}
	}
	return c.Secrets[name]
}

// AllSecrets lists every secret def (top-level, env-level, anonymous), sorted by key.
func (c *Config) AllSecrets() []*SecretDef {
	var out []*SecretDef
	for _, d := range c.Secrets {
		out = append(out, d)
	}
	for _, e := range c.Envs {
		for _, d := range e.Secrets {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// LogDir returns the expanded audit log directory.
func (c *Config) LogDir() string {
	p := c.Logging.Path
	if p == "" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".tmvp", c.Product, "logs")
	}
	p = ExpandHome(p)
	if !filepath.IsAbs(p) && c.Dir != "" {
		p = filepath.Join(c.Dir, p)
	}
	return p
}

// AssetRoots returns the asset roots in lookup order: each `assets:` entry
// (~ expanded, relative to the config dir), or [<config dir>/assets] when
// none are configured. Each root holds <system>/<kind>/<name>.<ext>.
func (c *Config) AssetRoots() []string {
	if len(c.Assets) == 0 {
		return []string{filepath.Join(c.Dir, "assets")}
	}
	out := make([]string, 0, len(c.Assets))
	for _, a := range c.Assets {
		p := ExpandHome(a)
		if !filepath.IsAbs(p) && c.Dir != "" {
			p = filepath.Join(c.Dir, p)
		}
		out = append(out, filepath.Clean(p))
	}
	return out
}

// ExpandHome expands a leading ~.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[1:])
	}
	return p
}

// Load reads and parses a config file.
func Load(path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, errs.Wrap(errs.Config, err, "config path")
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, errs.Wrap(errs.Config, err, "read config")
	}
	c, err := Parse(b)
	if err != nil {
		return nil, err
	}
	c.Path = abs
	c.Dir = filepath.Dir(abs)
	return c, nil
}

// HashFile returns the sha256 of a file (used by the server to detect changes).
func HashFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

type rawConfig struct {
	Version int               `yaml:"version"`
	Product string            `yaml:"product"`
	Logging Logging           `yaml:"logging"`
	Server  ServerConfig      `yaml:"server"`
	Vars    map[string]string `yaml:"vars"`
	Assets  []string          `yaml:"assets"`
	Secrets map[string]string `yaml:"secrets"`
	DefEnv  string            `yaml:"default_env"`
	Limits  map[string]any    `yaml:"limits"`
	Envs    yaml.Node         `yaml:"envs"`
}

type rawEnv struct {
	Type        string            `yaml:"type"`
	Destructive *bool             `yaml:"destructive"`
	Vars        map[string]string `yaml:"vars"`
	Limits      map[string]any    `yaml:"limits"`
	Secrets     map[string]string `yaml:"secrets"`
	Systems     yaml.Node         `yaml:"systems"`
}

// Parse parses config bytes (no file context).
func Parse(b []byte) (*Config, error) {
	var raw rawConfig
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, errs.Wrap(errs.Config, err, "parse config")
	}
	h := sha256.Sum256(b)
	c := &Config{
		Version: raw.Version, Product: raw.Product, Logging: raw.Logging, Server: raw.Server,
		Vars: nz(raw.Vars), Assets: raw.Assets, DefaultEnv: strings.TrimSpace(raw.DefEnv), Limits: raw.Limits, Secrets: map[string]*SecretDef{}, Envs: map[string]*Env{},
		Hash: hex.EncodeToString(h[:]),
	}
	if c.Product == "" {
		return nil, errs.New(errs.Config, "config: product is required")
	}
	for n, v := range raw.Secrets {
		c.Secrets[n] = parseSecretDef(n, "", v, "secrets."+n)
	}
	if raw.Envs.Kind != 0 {
		if raw.Envs.Kind != yaml.MappingNode {
			return nil, errs.New(errs.Config, "config: envs must be a mapping")
		}
		for i := 0; i+1 < len(raw.Envs.Content); i += 2 {
			name := raw.Envs.Content[i].Value
			var re rawEnv
			if err := raw.Envs.Content[i+1].Decode(&re); err != nil {
				return nil, errs.Wrap(errs.Config, err, "envs.%s", name)
			}
			typ := NormalizeEnvType(re.Type)
			destructive := typ == "dev" || typ == "test" || typ == "local" // default by type; untyped/prod/staging stay protected
			if re.Destructive != nil {
				if *re.Destructive && typ == "prod" {
					return nil, errs.New(errs.Config, "envs.%s: type %s envs cannot be destructive: true", name, typ)
				}
				destructive = *re.Destructive
			}
			e := &Env{Name: name, Type: typ, Destructive: destructive, Vars: nz(re.Vars), Limits: re.Limits,
				Secrets: map[string]*SecretDef{}, Systems: map[string]*System{}}
			for n, v := range re.Secrets {
				e.Secrets[n] = parseSecretDef(n, name, v, "envs."+name+".secrets."+n)
			}
			if re.Systems.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(re.Systems.Content); j += 2 {
					sname := re.Systems.Content[j].Value
					var m map[string]any
					if err := re.Systems.Content[j+1].Decode(&m); err != nil {
						return nil, errs.Wrap(errs.Config, err, "envs.%s.systems.%s", name, sname)
					}
					s, err := buildSystem(name, sname, m)
					if err != nil {
						return nil, err
					}
					e.Systems[sname] = s
					e.SystemOrder = append(e.SystemOrder, sname)
					// rewrite inline $(cmd) into anonymous env-scoped secrets
					s.Fields = rewriteInline(s.Fields, "", func(path, cmd string, file bool) string {
						an := anonName(sname, path)
						e.Secrets[an] = &SecretDef{Name: an, Scope: name, Command: cmd, File: file, Anonymous: true,
							Origin: "envs." + name + ".systems." + sname + "." + path}
						return "${secrets." + an + "}"
					})
				}
			}
			c.Envs[name] = e
			c.EnvOrder = append(c.EnvOrder, name)
		}
	}
	return c, nil
}

func anonName(system, path string) string {
	return "_" + system + "." + path
}

func nz(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

var dotenvRe = regexp.MustCompile(`^\$dotenv\(\s*([^,()]+?)\s*,\s*([A-Za-z_][A-Za-z0-9_.]*)\s*\)$`)

func parseSecretDef(name, scope, v, origin string) *SecretDef {
	d := &SecretDef{Name: name, Scope: scope, Origin: origin}
	t := strings.TrimSpace(v)
	if m := dotenvRe.FindStringSubmatch(t); m != nil {
		d.DotenvFile, d.DotenvKey = strings.Trim(m[1], `"' `), m[2]
		return d
	}
	if cmd, file, ok := wholeCommand(t); ok {
		d.Command, d.File = cmd, file
	} else {
		d.Expr = v
	}
	return d
}

func buildSystem(env, name string, m map[string]any) (*System, error) {
	s := &System{Name: name, Env: env, Labels: map[string]string{}, Fields: map[string]any{}}
	for k, v := range m {
		switch k {
		case "type":
			s.Type = fmt.Sprint(v)
		case "via":
			switch vv := v.(type) {
			case string:
				s.Via = []string{vv}
			case []any:
				for _, x := range vv {
					tok, err := viaEntry(x)
					if err != nil {
						return nil, errs.New(errs.Config, "envs.%s.systems.%s.via: %v", env, name, err)
					}
					s.Via = append(s.Via, tok)
				}
			default:
				return nil, errs.New(errs.Config, "envs.%s.systems.%s.via: want string or list", env, name)
			}
		case "labels":
			mm, ok := v.(map[string]any)
			if !ok {
				return nil, errs.New(errs.Config, "envs.%s.systems.%s.labels: want mapping", env, name)
			}
			for lk, lv := range mm {
				s.Labels[lk] = fmt.Sprint(lv)
			}
		case "login":
			var l Login
			if err := remarshal(v, &l); err != nil {
				return nil, errs.Wrap(errs.Config, err, "envs.%s.systems.%s.login", env, name)
			}
			s.Login = &l
		case "destructive":
			b, ok := v.(bool)
			if !ok {
				return nil, errs.New(errs.Config, "envs.%s.systems.%s.destructive: want bool", env, name)
			}
			s.Destructive = &b
		default:
			s.Fields[k] = v
		}
	}
	if s.Type == "" {
		return nil, errs.New(errs.Config, "envs.%s.systems.%s: type is required", env, name)
	}
	return s, nil
}

func remarshal(in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// rewriteInline walks fields and replaces every inline $(cmd)/$file(cmd) with
// the reference returned by anon.
func rewriteInline(v map[string]any, prefix string, anon func(path, cmd string, file bool) string) map[string]any {
	for k, x := range v {
		p := k
		if prefix != "" {
			p = prefix + "." + k
		}
		v[k] = rewriteValue(x, p, anon)
	}
	return v
}

func rewriteValue(x any, p string, anon func(path, cmd string, file bool) string) any {
	switch t := x.(type) {
	case string:
		return replaceCommands(t, func(i int, cmd string, file bool) string {
			path := p
			if i > 0 {
				path = fmt.Sprintf("%s#%d", p, i)
			}
			return anon(path, cmd, file)
		})
	case map[string]any:
		return rewriteInline(t, p, anon)
	case []any:
		for i := range t {
			t[i] = rewriteValue(t[i], fmt.Sprintf("%s.%d", p, i), anon)
		}
		return t
	}
	return x
}

// Get returns a field by dotted path ("read.dsn", "users.ops.password").
func (s *System) Get(path string) (any, bool) {
	var cur any = s.Fields
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// Fingerprint changes when anything that affects a connection to s changes:
// its fields, env vars/destructive, top vars, and the secret defs it references.
func (c *Config) Fingerprint(env, system string) string {
	e := c.Envs[env]
	s := c.System(env, system)
	if s == nil {
		return ""
	}
	b, _ := json.Marshal(s)
	h := sha256.New()
	h.Write(b)
	vb, _ := json.Marshal([]any{c.Vars, e.Vars, e.Destructive, c.Limits, e.Limits})
	h.Write(vb)
	for _, n := range ReferencedSecrets(s) {
		if d := c.LookupSecret(env, n); d != nil {
			h.Write([]byte(d.Key() + d.Fingerprint()))
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// ReferencedSecrets lists secret names referenced by s's fields (sorted, unique).
func ReferencedSecrets(s *System) []string {
	set := map[string]bool{}
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case string:
			for _, r := range Refs(t) {
				if r.Kind == RefSecret || r.Kind == RefFileSecret {
					set[r.Name] = true
				}
			}
		case map[string]any:
			for _, v := range t {
				walk(v)
			}
		case []any:
			for _, v := range t {
				walk(v)
			}
		}
	}
	walk(s.Fields)
	if s.Login != nil {
		walk(s.Login.Check)
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
