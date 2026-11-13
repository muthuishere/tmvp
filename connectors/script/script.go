// Package script is the tmvp connector that runs the skill's own scripts
// (type: script) from assets/<system>/scripts/.
//
// A script whose first lines carry the header
//
//	# tmvp: read        (also // , -- , ; , rem , :: comment styles)
//
// is a read and runs with `tmvp <env> <system> run @name [args]`. Every other
// script is a write: `tmvp write <env> <system> @name [args] --reason "…"`.
//
// Config fields:
//
//	env:        { NAME: value, … }  environment for every script (values may be
//	            "${secrets.x}"; "${file:secrets.x}" becomes a per-call file path)
//	read.env / write.env  extra variables for that mode (override env:)
//
// tmvp also sets TMVP_PRODUCT, TMVP_ENV, TMVP_SYSTEM, TMVP_MODE. Interpreter
// by extension: .sh/.bash → bash|sh, .py → python3|python, .js/.mjs → node,
// .ps1 → pwsh|powershell, .cmd/.bat → cmd /c, .rb → ruby, none → executed
// directly (shebang). Output is streamed line by line (bounded) and passes
// the core redactor.
package script

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/muthuishere/tmvp/connectors/internal/cli"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func init() { connector.Register("script", New) }

// New returns an unopened script connector.
func New() connector.Connector { return &Script{} }

// Script runs scripts for one env/system/mode.
type Script struct{ t *connector.Target }

var verbs = []connector.Verb{
	{Name: "ls", Mode: connector.Read, Help: "ls — saved scripts across all asset roots: name, read|write, source file"},
	{Name: "list", Mode: connector.Read, Help: "list — alias of ls"},
	{Name: "run", Mode: connector.Read, Saved: true, Help: "run @name [args] — only scripts declared `# tmvp: read`"},
	{Name: "exec", Mode: connector.Write, Default: true, Help: "exec @name [args] — any script (tmvp write; --preview shows it)"},
}

func (s *Script) Type() string            { return "script" }
func (s *Script) Verbs() []connector.Verb { return verbs }

// Guard validates the script name before Open.
func (s *Script) Guard(c connector.Call) error {
	if c.Verb == "list" || c.Verb == "ls" {
		return nil
	}
	if len(c.Args) == 0 {
		return errs.New(errs.Usage, "%s needs a script name", c.Verb).WithHint(c.Verb + " @name")
	}
	n := strings.TrimPrefix(c.Args[0], "@")
	if n == "" || strings.ContainsAny(n, `/\`) || strings.Contains(n, "..") || strings.HasPrefix(n, ".") {
		return errs.New(errs.Usage, "bad script name %q", c.Args[0])
	}
	return nil
}

func (s *Script) Open(_ context.Context, t *connector.Target) error { s.t = t; return nil }
func (s *Script) Close() error                                      { return nil }

// dirs are <root>/<system>/scripts for every asset root, in lookup order.
func (s *Script) dirs() []string {
	ds := s.t.AssetDirs()
	for i := range ds {
		ds[i] = filepath.Join(ds[i], "scripts")
	}
	return ds
}

// Do runs one verb.
func (s *Script) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	if err := s.Guard(c); err != nil {
		return err
	}
	if c.Verb == "list" || c.Verb == "ls" {
		return s.list(e)
	}
	path, err := s.find(c.Args[0])
	if err != nil {
		return err
	}
	read := IsRead(path)
	switch c.Verb {
	case "run":
		if !read {
			return errs.New(errs.PolicyBlocked, "script %s is not declared `# tmvp: read`; it may mutate", c.Args[0]).
				WithHint(`tmvp write <env> ` + s.t.System + " " + c.Args[0] + ` --reason "why"`)
		}
	case "exec":
		if c.Preview {
			return cli.IgnoreLimit(e.Row(connector.Row{"preview": true, "script": filepath.Base(path), "declared_read": read,
				"args": strings.Join(c.Args[1:], " ")}))
		}
	default:
		return errs.New(errs.Usage, "script has no verb %q", c.Verb)
	}
	return s.run(ctx, path, c.Args[1:], connector.CallTimeout(c, s.t.Limits), e)
}

func (s *Script) list(e connector.Emit) error {
	seen := map[string]bool{}
	for _, d := range s.dirs() {
		ents, err := os.ReadDir(d)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return errs.Wrap(errs.Runtime, err, "")
		}
		sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
		for _, en := range ents {
			if en.IsDir() || strings.HasPrefix(en.Name(), ".") {
				continue
			}
			name := strings.TrimSuffix(en.Name(), filepath.Ext(en.Name()))
			if seen[name] {
				continue // first asset root wins
			}
			seen[name] = true
			p := filepath.Join(d, en.Name())
			mode := "write"
			if IsRead(p) {
				mode = "read"
			}
			if err := e.Row(connector.Row{"script": "@" + name, "file": en.Name(), "mode": mode, "source": p}); err != nil {
				return cli.IgnoreLimit(err)
			}
		}
	}
	return nil
}

// find resolves @name to a file in the first scripts dir that has it
// (exact, or name.<ext>).
func (s *Script) find(arg string) (string, error) {
	name := strings.TrimPrefix(arg, "@")
	for _, d := range s.dirs() {
		if st, err := os.Stat(filepath.Join(d, name)); err == nil && !st.IsDir() {
			return filepath.Join(d, name), nil
		}
		m, _ := filepath.Glob(filepath.Join(d, globEscape(name)+".*"))
		sort.Strings(m)
		if len(m) > 1 {
			return "", errs.New(errs.Usage, "script %q is ambiguous (%d files); pass the file name with extension", name, len(m))
		}
		if len(m) == 1 {
			return m[0], nil
		}
	}
	return "", errs.New(errs.NotFound, "no script %q in assets/%s/scripts", name, s.t.System).WithHint("tmvp <env> " + s.t.System + " ls")
}

func globEscape(s string) string {
	return strings.NewReplacer("*", `\*`, "?", `\?`, "[", `\[`).Replace(s)
}

var readHeader = regexp.MustCompile(`(?i)^\s*(#|//|--|;|rem\b|::|<#)\s*tmvp:\s*read\b`)

// IsRead reports whether the script declares `tmvp: read` in its first 15 lines.
func IsRead(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for i := 0; i < 15 && sc.Scan(); i++ {
		if readHeader.MatchString(sc.Text()) {
			return true
		}
	}
	return false
}

// Command returns the argv to run path.
func Command(path string) ([]string, error) {
	first := func(names ...string) (string, error) {
		var last error
		for _, n := range names {
			p, err := cli.Find(n)
			if err == nil {
				return p, nil
			}
			last = err
		}
		return "", last
	}
	ext := strings.ToLower(filepath.Ext(path))
	var interp []string
	switch ext {
	case ".sh", ".bash":
		p, err := first("bash", "sh")
		if err != nil {
			return nil, err
		}
		interp = []string{p}
	case ".py":
		p, err := first("python3", "python")
		if err != nil {
			return nil, err
		}
		interp = []string{p}
	case ".js", ".mjs", ".cjs":
		p, err := first("node")
		if err != nil {
			return nil, err
		}
		interp = []string{p}
	case ".rb":
		p, err := first("ruby")
		if err != nil {
			return nil, err
		}
		interp = []string{p}
	case ".ps1":
		p, err := first("pwsh", "powershell")
		if err != nil {
			return nil, err
		}
		interp = []string{p, "-NoProfile", "-NonInteractive", "-File"}
	case ".cmd", ".bat":
		if runtime.GOOS != "windows" {
			return nil, errs.New(errs.Unsupported, "%s scripts run only on Windows", ext)
		}
		interp = []string{"cmd.exe", "/d", "/c"}
	}
	return append(interp, path), nil
}

func (s *Script) env(ctx context.Context) (map[string]string, func(), error) {
	t := s.t
	var cleanups []func()
	cleanup := func() {
		for _, f := range cleanups {
			f()
		}
	}
	env := map[string]string{"TMVP_PRODUCT": t.Product, "TMVP_ENV": t.Env, "TMVP_SYSTEM": t.System, "TMVP_MODE": string(t.Mode)}
	for _, base := range []string{"env", string(t.Mode) + ".env"} {
		raw, ok := t.Raw(base)
		m, isMap := raw.(map[string]any)
		if !ok || !isMap {
			continue
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := base + "." + k
			if cli.IsFileRef(t, p) {
				fp, c, err := t.FilePath(ctx, p)
				if err != nil {
					return nil, cleanup, err
				}
				cleanups = append(cleanups, c)
				env[k] = fp
				continue
			}
			v, err := t.String(ctx, p)
			if err != nil {
				return nil, cleanup, err
			}
			env[k] = v
		}
	}
	return env, cleanup, nil
}

func (s *Script) run(ctx context.Context, path string, args []string, timeout time.Duration, e connector.Emit) error {
	argv, err := Command(path)
	if err != nil {
		return err
	}
	env, cleanup, err := s.env(ctx)
	defer cleanup()
	if err != nil {
		return err
	}
	err = cli.Lines(ctx, cli.Cmd{Path: argv[0], Args: append(argv[1:], args...), Env: env, Dir: filepath.Dir(path), Timeout: timeout},
		e, s.t.Limits.Lines, s.t.Limits.Bytes)
	// A script's stderr is free text: never report auth_error, because the
	// server would re-run secrets and retry — i.e. run a write script twice.
	if xe := errs.From(err); xe != nil && xe.Code == errs.Auth {
		return errs.New(errs.Runtime, "script failed: %s", xe.Message)
	}
	return err
}
