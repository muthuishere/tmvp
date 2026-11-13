package connector

import (
	"context"
	"errors"
	"fmt"
	"github.com/muthuishere/tmvp/internal/secure"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Mode is the credential mode a connector is opened with.
type Mode string

const (
	ModeRead  Mode = "read"
	ModeWrite Mode = "write"
)

// Redactor registers derived secret values for output redaction.
type Redactor interface {
	AddBasicAuth(user, pass string)
}

// LoginSpec is a system's manual-login declaration (config `login:`).
type LoginSpec struct {
	Manual string // command the user runs, or "browser"
	Check  string // non-interactive check command (run by tmvp, not the connector)
}

// Limits are the guard defaults for a target (config `limits:` overrides).
type Limits struct {
	Rows             int           // max rows per call (default 1000)
	Lines            int           // max text lines per call (default 500)
	Bytes            int           // max output bytes per call (default 1 MiB)
	Timeout          time.Duration // per-call wall time from config (system > env > global > 10s)
	StatementTimeout time.Duration // server-side statement timeout when the call carries none (default 15s)

	// Cost-guard calibration (0 = the connector's documented default). These
	// are ROUGH: they turn a planner cost / doc count / byte count into a
	// wall-time estimate. See `tmvp --llmhelp cost-and-timeouts`.
	CostPerSecond   float64 // SQL planner cost units per second (postgres, mysql)
	RowsPerSecond   float64 // rows/docs scanned per second (mongo, elasticsearch, cloud logs)
	BytesPerSecond  float64 // bytes scanned per second (loki)
	PointsPerSecond float64 // samples per second (prometheus, grafana)
	SeqScanRows     float64 // postgres: flag a Seq Scan on a table with more rows than this
}

// DefaultLimits are used when the config sets none.
var DefaultLimits = Limits{Rows: 1000, Lines: 500, Bytes: 1 << 20, Timeout: 10 * time.Second, StatementTimeout: 15 * time.Second}

// Fields resolves a system's config fields. Implemented by the server (lazy
// secret resolution, recording which secrets were used) and by StaticFields
// for tests. Paths are dotted ("read.dsn", "users.ops.password").
type Fields interface {
	// Raw returns the unresolved value (string, map[string]any, []any, number, bool).
	Raw(path string) (any, bool)
	// String interpolates a string field (vars, env, secrets). Secrets are
	// fetched on first use. Errors are *errs.Error (secret_missing, config_error).
	String(ctx context.Context, path string) (string, error)
	// File returns the bytes of a "${file:secrets.x}" field (in memory, never on disk).
	File(ctx context.Context, path string) ([]byte, error)
	// FilePath materialises a file secret for an external CLI; call cleanup when done.
	FilePath(ctx context.Context, path string) (p string, cleanup func(), err error)
}

// Target is everything a connector gets in Open. Never log Target fields that
// came from String/File — they may be secrets (the redactor is a backstop,
// not a licence).
type Target struct {
	Product     string
	Env         string
	System      string
	Type        string
	Mode        Mode
	Destructive bool
	Labels      map[string]string
	Via         []string
	// Login is the system's `login: {manual, check}` (nil when absent). tmvp
	// runs Check itself before Open; connectors read Manual (e.g. web
	// `manual: browser` → the human logs in with `tmvp login`).
	Login  *LoginSpec
	Limits Limits
	Dir    string // skill directory (config.yaml's dir)
	// AssetRoots are the asset roots in lookup order (config `assets:`,
	// resolved against Dir). Each root holds <System>/<kind>/<name>.<ext>.
	// Empty means [Dir/assets]. Saved items are looked up across all of
	// them; the first match wins.
	AssetRoots []string
	// Redactor, when set (by the server), learns derived secret forms a
	// connector builds, e.g. base64(user:pass) for HTTP basic auth.
	Redactor Redactor
	// Dialer honours `via:`. Direct net.Dialer when there is no via. Use it
	// for every network connection the connector makes.
	Dialer Dialer

	fields Fields
}

// NewTarget attaches a Fields resolver to t.
func NewTarget(t Target, f Fields) *Target {
	if t.Dialer == nil {
		t.Dialer = DirectDialer()
	}
	if t.Limits == (Limits{}) {
		t.Limits = DefaultLimits
	}
	t.fields = f
	return &t
}

// Raw returns an unresolved field.
func (t *Target) Raw(path string) (any, bool) {
	if t.fields == nil {
		return nil, false
	}
	return t.fields.Raw(path)
}

// Has reports whether a field exists.
func (t *Target) Has(path string) bool { _, ok := t.Raw(path); return ok }

// String resolves a string field. Missing field → error.
func (t *Target) String(ctx context.Context, path string) (string, error) {
	if t.fields == nil {
		return "", fmt.Errorf("no fields")
	}
	return t.fields.String(ctx, path)
}

// StringOr resolves a string field or returns def when absent.
func (t *Target) StringOr(ctx context.Context, path, def string) (string, error) {
	if !t.Has(path) {
		return def, nil
	}
	return t.String(ctx, path)
}

// ModeString resolves "<mode>.<key>" (e.g. read.dsn / write.dsn), falling
// back to the top-level "<key>". In write mode it never falls back to read.*.
func (t *Target) ModeString(ctx context.Context, key string) (string, error) {
	p := string(t.Mode) + "." + key
	if t.Has(p) {
		return t.String(ctx, p)
	}
	return t.String(ctx, key)
}

// File returns the in-memory bytes of a ${file:secrets.x} field.
func (t *Target) File(ctx context.Context, path string) ([]byte, error) {
	return t.fields.File(ctx, path)
}

// FilePath materialises a file secret for an external CLI.
func (t *Target) FilePath(ctx context.Context, path string) (string, func(), error) {
	return t.fields.FilePath(ctx, path)
}

// Dial dials through the target's via chain.
func (t *Target) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return t.Dialer.DialContext(ctx, network, addr)
}

// Roots returns the asset roots in lookup order (default Dir/assets).
func (t *Target) Roots() []string {
	if len(t.AssetRoots) > 0 {
		return t.AssetRoots
	}
	return []string{filepath.Join(t.Dir, "assets")}
}

// AssetDirs is <root>/<System> for every asset root, in lookup order.
func (t *Target) AssetDirs() []string {
	rs := t.Roots()
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = filepath.Join(r, t.System)
	}
	return out
}

// AssetsDir is the first asset dir (<first root>/<System>).
func (t *Target) AssetsDir() string { return t.AssetDirs()[0] }

// Saved is one saved item (<root>/<system>/<kind>/<name>.<ext>).
type Saved struct {
	Name        string
	Path        string // file it came from (shows which asset repo)
	FrontMatter string // raw YAML between leading --- lines (or a leading /* --- */ block)
	Body        string
}

// Description returns front-matter `description:` (empty when absent/unparsable).
func (s *Saved) Description() string {
	var m struct {
		Description string `yaml:"description"`
	}
	_ = yaml.Unmarshal([]byte(s.FrontMatter), &m)
	return m.Description
}

// LoadSaved finds <root>/<system>/<kind>/<name><ext> across the asset roots
// (first match wins). A name may be passed with or without the leading "@".
// Front-matter is a leading block "---\n...\n---\n" (or "/*---\n...\n---*/"
// for SQL); the body is the rest.
func (t *Target) LoadSaved(kind, name, ext string) (*Saved, error) {
	name = strings.TrimPrefix(name, "@")
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return nil, fmt.Errorf("bad saved item name %q", name)
	}
	var tried []string
	for _, d := range t.AssetDirs() {
		p := filepath.Join(d, kind, name+ext)
		b, err := os.ReadFile(p)
		if err != nil {
			tried = append(tried, p)
			continue
		}
		fm, body := SplitFrontMatter(string(b))
		return &Saved{Name: name, Path: p, FrontMatter: fm, Body: body}, nil
	}
	return nil, fmt.Errorf("saved %s %q not found (looked in %s)", kind, name, strings.Join(tried, ", "))
}

// ListSaved lists every <kind>/*<ext> across the asset roots, sorted by
// name; when two roots hold the same name the first root wins.
func (t *Target) ListSaved(kind, ext string) ([]*Saved, error) {
	seen := map[string]bool{}
	var out []*Saved
	for _, d := range t.AssetDirs() {
		files, _ := filepath.Glob(filepath.Join(d, kind, "*"+ext))
		sort.Strings(files)
		for _, f := range files {
			n := strings.TrimSuffix(filepath.Base(f), ext)
			if seen[n] || strings.HasPrefix(n, ".") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				return nil, fmt.Errorf("saved %s %q: %w", kind, n, err)
			}
			seen[n] = true
			fm, body := SplitFrontMatter(string(b))
			out = append(out, &Saved{Name: n, Path: f, FrontMatter: fm, Body: body})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// EmitSaved emits one `ls` row per saved item: kind, name, description,
// source (the file, so people see which asset repo it came from).
func (t *Target) EmitSaved(e Emit, kind, ext string) error {
	items, err := t.ListSaved(kind, ext)
	if err != nil {
		return err
	}
	for _, s := range items {
		if err := e.Row(Row{"kind": kind, "name": "@" + s.Name, "description": s.Description(), "source": s.Path}); err != nil {
			if errors.Is(err, ErrLimit) {
				return nil
			}
			return err
		}
	}
	return nil
}

// SplitFrontMatter splits "---\nyaml\n---\nbody" (also "/*---\n...\n---*/").
func SplitFrontMatter(s string) (fm, body string) {
	s = strings.TrimPrefix(s, "\uFEFF")
	norm := strings.ReplaceAll(s, "\r\n", "\n")
	for _, d := range [][2]string{{"---\n", "\n---"}, {"/*---\n", "\n---*/"}} {
		if strings.HasPrefix(norm, d[0]) {
			rest := norm[len(d[0]):]
			if i := strings.Index(rest, d[1]); i >= 0 {
				body := rest[i+len(d[1]):]
				body = strings.TrimPrefix(body, "\n")
				return rest[:i], body
			}
		}
	}
	return "", norm
}

// StaticFields is a Fields over literal values (tests, no secrets).
// "${file:secrets.x}" is not special: File returns the literal string bytes.
type StaticFields map[string]any

func (s StaticFields) Raw(path string) (any, bool) {
	var cur any = map[string]any(s)
	for _, p := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[p]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func (s StaticFields) String(_ context.Context, path string) (string, error) {
	v, ok := s.Raw(path)
	if !ok {
		return "", fmt.Errorf("field %q not set", path)
	}
	return fmt.Sprint(v), nil
}

func (s StaticFields) File(ctx context.Context, path string) ([]byte, error) {
	v, err := s.String(ctx, path)
	return []byte(v), err
}

func (s StaticFields) FilePath(ctx context.Context, path string) (string, func(), error) {
	b, err := s.File(ctx, path)
	if err != nil {
		return "", nil, err
	}
	dir, err := secure.MkdirTemp("", "tmvp-test-")
	if err != nil {
		return "", nil, err
	}
	p := filepath.Join(dir, "f")
	if err := secure.WriteFile(p, b); err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	return p, func() { os.RemoveAll(dir) }, nil
}
