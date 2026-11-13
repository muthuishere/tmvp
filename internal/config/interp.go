package config

import (
	"os"
	"regexp"
	"strings"

	"github.com/muthuishere/tmvp/internal/errs"
)

// RefKind classifies a ${...} reference.
type RefKind int

const (
	RefEnvVar     RefKind = iota // ${NAME} / ${NAME:-default}
	RefVar                       // ${vars.x}
	RefSecret                    // ${secrets.x}
	RefFileSecret                // ${file:secrets.x}
	RefUser                      // ${user.x} — left verbatim for the web connector
	RefOut                       // ${out} — only inside $file(cmd)
	RefInvalid
)

// Ref is one parsed ${...} reference.
type Ref struct {
	Kind    RefKind
	Name    string
	Default string
	HasDef  bool
	Raw     string // text between ${ and }
}

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ParseRef classifies the text between ${ and }.
func ParseRef(expr string) Ref {
	r := Ref{Raw: expr}
	switch {
	case strings.HasPrefix(expr, "vars."):
		r.Kind, r.Name = RefVar, expr[5:]
	case strings.HasPrefix(expr, "secrets."):
		r.Kind, r.Name = RefSecret, expr[8:]
	case strings.HasPrefix(expr, "file:secrets."):
		r.Kind, r.Name = RefFileSecret, expr[13:]
	case strings.HasPrefix(expr, "user."):
		r.Kind, r.Name = RefUser, expr[5:]
	case expr == "out":
		r.Kind, r.Name = RefOut, "out"
	default:
		name := expr
		if i := strings.Index(expr, ":-"); i >= 0 {
			name, r.Default, r.HasDef = expr[:i], expr[i+2:], true
		}
		r.Name = name
		r.Kind = RefEnvVar
		if !envNameRe.MatchString(name) {
			r.Kind = RefInvalid
		}
	}
	if r.Kind != RefEnvVar && r.Kind != RefInvalid && r.Name == "" {
		r.Kind = RefInvalid
	}
	return r
}

// Expand substitutes ${...} in s using f. "$$" becomes "$". When f returns
// keep=true the reference is left verbatim. Unterminated "${" is an error.
func Expand(s string, f func(Ref) (repl string, keep bool, err error)) (string, error) {
	if !strings.Contains(s, "$") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch != '$' || i+1 >= len(s) {
			b.WriteByte(ch)
			continue
		}
		switch s[i+1] {
		case '$':
			b.WriteByte('$')
			i++
		case '{':
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return "", errs.New(errs.Config, "unterminated ${ in %q", redactHint(s))
			}
			expr := s[i+2 : i+2+end]
			repl, keep, err := f(ParseRef(expr))
			if err != nil {
				return "", err
			}
			if keep {
				b.WriteString("${" + expr + "}")
			} else {
				b.WriteString(repl)
			}
			i += 2 + end
		default:
			b.WriteByte(ch)
		}
	}
	return b.String(), nil
}

// Refs lists every ${...} reference in s (honouring $$ escapes).
func Refs(s string) []Ref {
	var out []Ref
	_, _ = Expand(s, func(r Ref) (string, bool, error) { out = append(out, r); return "", true, nil })
	return out
}

func redactHint(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

// findCommand finds the next $(...) or $file(...) at or after i, skipping $$.
// Returns start, end (exclusive, after ')'), inner command, file flag.
func findCommand(s string, from int) (start, end int, cmd string, file bool, ok bool) {
	for i := from; i < len(s); i++ {
		if s[i] != '$' {
			continue
		}
		if i+1 < len(s) && s[i+1] == '$' {
			i++
			continue
		}
		var open int
		switch {
		case strings.HasPrefix(s[i:], "$("):
			open = i + 1
		case strings.HasPrefix(s[i:], "$file("):
			open, file = i+5, true
		default:
			continue
		}
		depth, q := 0, byte(0)
		for j := open; j < len(s); j++ {
			c := s[j]
			if q != 0 {
				if c == q {
					q = 0
				}
				continue
			}
			switch c {
			case '\'', '"':
				q = c
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					return i, j + 1, s[open+1 : j], file, true
				}
			}
		}
		return 0, 0, "", false, false // unterminated: leave verbatim
	}
	return 0, 0, "", false, false
}

// wholeCommand reports whether t is exactly one $(cmd) / $file(cmd).
func wholeCommand(t string) (cmd string, file bool, ok bool) {
	st, end, c, f, found := findCommand(t, 0)
	if found && st == 0 && end == len(t) {
		return strings.TrimSpace(c), f, true
	}
	return "", false, false
}

// replaceCommands replaces each inline command with repl(i, cmd, file).
func replaceCommands(s string, repl func(i int, cmd string, file bool) string) string {
	var b strings.Builder
	pos, n := 0, 0
	for {
		st, end, cmd, file, ok := findCommand(s, pos)
		if !ok {
			break
		}
		b.WriteString(s[pos:st])
		b.WriteString(repl(n, strings.TrimSpace(cmd), file))
		n++
		pos = end
	}
	if n == 0 {
		return s
	}
	b.WriteString(s[pos:])
	return b.String()
}

// FileRef reports whether a field value is exactly "${file:secrets.name}".
func FileRef(v string) (name string, ok bool) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "${file:secrets.") || !strings.HasSuffix(v, "}") || strings.Count(v, "${") != 1 {
		return "", false
	}
	return v[len("${file:secrets.") : len(v)-1], true
}

// SecretFunc returns the value of a secret slot (run lazily by the caller).
type SecretFunc func(def *SecretDef) (string, error)

// Getenv is the process-env lookup (swappable in tests).
var Getenv = os.LookupEnv

// ExpandField interpolates one field value for env. secret may be nil, in
// which case ${secrets.x} is an error. ${user.x} is kept. ${file:secrets.x}
// is an error here (use the Target's File/FilePath accessors).
func (c *Config) ExpandField(env, s string, secret SecretFunc) (string, error) {
	return c.expand(env, s, secret, 0)
}

func (c *Config) expand(env, s string, secret SecretFunc, depth int) (string, error) {
	if depth > 8 {
		return "", errs.New(errs.Config, "vars nest too deep (cycle?)")
	}
	return Expand(s, func(r Ref) (string, bool, error) {
		switch r.Kind {
		case RefVar:
			v, ok := c.lookupVar(env, r.Name)
			if !ok {
				return "", false, errs.New(errs.Config, "unknown var %q", r.Name).At(env, "")
			}
			out, err := c.expand(env, v, nil, depth+1)
			return out, false, err
		case RefSecret:
			d := c.LookupSecret(env, r.Name)
			if d == nil {
				return "", false, errs.New(errs.Config, "unknown secret %q", r.Name).At(env, "")
			}
			if secret == nil {
				return "", false, errs.New(errs.Config, "secret %q not allowed here", r.Name)
			}
			v, err := secret(d)
			return v, false, err
		case RefFileSecret:
			return "", false, errs.New(errs.Config, "${file:secrets.%s} must be the whole field; read it with Target.File/FilePath", r.Name)
		case RefUser, RefOut:
			return "", true, nil
		case RefEnvVar:
			if v, ok := Getenv(r.Name); ok && (v != "" || !r.HasDef) {
				return v, false, nil
			}
			if r.HasDef {
				return r.Default, false, nil
			}
			return "", false, errs.New(errs.Config, "environment variable %s is not set", r.Name).WithHint("set it or use ${" + r.Name + ":-default}")
		}
		return "", false, errs.New(errs.Config, "invalid reference ${%s}", r.Raw)
	})
}

func (c *Config) lookupVar(env, name string) (string, bool) {
	if e := c.Envs[env]; e != nil {
		if v, ok := e.Vars[name]; ok {
			return v, true
		}
	}
	v, ok := c.Vars[name]
	return v, ok
}

// CommandText returns the command of def with vars/env expanded and ${out} kept.
// Secrets inside commands are not allowed.
func (c *Config) CommandText(def *SecretDef) (string, error) {
	return c.expand(def.Scope, def.Command, nil, 0)
}

// ExprValue resolves a non-command secret (e.g. "${DB_URL}").
func (c *Config) ExprValue(def *SecretDef) (string, error) {
	return c.expand(def.Scope, def.Expr, nil, 0)
}
