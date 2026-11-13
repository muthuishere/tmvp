// Package skill scaffolds a product skill (`tmvp new skill`), adds systems to
// it (`tmvp skill add`), regenerates its SKILL.md from config.yaml
// (`tmvp skill sync`), lints it (`tmvp skill lint`) and renders client
// snippets (`tmvp export`).
// It never resolves or writes a secret value: config slots are $(…) commands.
package skill

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/muthuishere/tmvp/internal/config"
	"github.com/muthuishere/tmvp/internal/errs"
)

//go:embed templates/*.tmpl
var tmplFS embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{"join": strings.Join}).ParseFS(tmplFS, "templates/*.tmpl"))

// SystemSpec is one `name:type` from --systems.
type SystemSpec struct{ Name, Type string }

// Options for New.
type Options struct {
	Product string
	Path    string // parent dir, default .agents/skills
	Envs    []string
	Systems []SystemSpec
	// From is an existing config.yaml to adopt instead of generating one
	// (e.g. ~/.tmvp/<product>/config.yaml). It must load and name Product.
	From string
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// ParseSystems parses "db:postgres,cluster:k8s".
func ParseSystems(s string) ([]SystemSpec, error) {
	var out []SystemSpec
	seen := map[string]bool{}
	for _, part := range splitList(s) {
		n, t, ok := strings.Cut(part, ":")
		if !ok || n == "" || t == "" {
			return nil, errs.New(errs.Usage, "--systems: want name:type, got %q", part)
		}
		if err := checkName("system", n); err != nil {
			return nil, err
		}
		if seen[n] {
			return nil, errs.New(errs.Usage, "--systems: %q given twice", n)
		}
		seen[n] = true
		out = append(out, SystemSpec{n, strings.ToLower(t)})
	}
	return out, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// SplitList splits a comma list, dropping blanks.
func SplitList(s string) []string { return splitList(s) }

func checkName(kind, n string) error {
	if !nameRE.MatchString(n) {
		return errs.New(errs.Usage, "%s name %q: use lowercase letters, digits, - or _", kind, n)
	}
	return nil
}

// New writes <path>/<product>-tmvp and returns its path. It refuses to
// overwrite an existing directory.
func New(o Options) (string, error) {
	if err := checkName("product", o.Product); err != nil {
		return "", err
	}
	if o.Path == "" {
		o.Path = filepath.Join(".agents", "skills")
	}
	if len(o.Envs) == 0 {
		o.Envs = []string{"dev", "staging", "prod"}
	}
	for _, e := range o.Envs {
		if err := checkName("env", e); err != nil {
			return "", err
		}
	}
	dir := filepath.Join(o.Path, o.Product+"-tmvp")
	if _, err := os.Stat(dir); err == nil {
		return "", errs.New(errs.Usage, "%s already exists; refusing to overwrite", dir).
			WithHint("use `tmvp --skill " + dir + " skill add <name> <type>` to add systems")
	}
	if o.From != "" { // check before creating anything
		if _, _, err := adopt(o.From, o.Product); err != nil {
			return "", err
		}
	}
	err := os.MkdirAll(dir, 0o755)
	if err != nil {
		return "", errs.Wrap(errs.Runtime, err, "create %s", dir)
	}
	var cfg []byte
	if o.From != "" {
		if cfg, o.Systems, err = adopt(o.From, o.Product); err != nil {
			return "", err
		}
	} else if cfg, err = marshal(configDoc(o)); err != nil {
		return "", err
	}
	if err := writeNew(filepath.Join(dir, "config.yaml"), cfg); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		return "", errs.Wrap(errs.Runtime, err, "create assets")
	}
	for _, s := range o.Systems {
		if err := writeAssets(dir, s); err != nil {
			return "", err
		}
	}
	if err := Sync(filepath.Join(dir, "config.yaml")); err != nil {
		return "", err
	}
	return dir, nil
}

// adopt reads an existing config, checks it loads and belongs to product,
// and returns its bytes unchanged plus its systems (for assets/ folders).
func adopt(from, product string) ([]byte, []SystemSpec, error) {
	c, err := config.Load(from)
	if err != nil {
		return nil, nil, err
	}
	if c.Product != product {
		return nil, nil, errs.New(errs.Usage, "%s is for product %q, not %q", from, c.Product, product)
	}
	if len(c.Assets) > 0 {
		return nil, nil, errs.New(errs.Usage, "%s has assets: roots relative to its folder; move them by hand", from)
	}
	b, err := os.ReadFile(from)
	if err != nil {
		return nil, nil, errs.Wrap(errs.Runtime, err, "read %s", from)
	}
	seen := map[string]bool{}
	var specs []SystemSpec
	for _, e := range c.EnvOrder {
		for _, n := range c.Envs[e].SystemOrder {
			if !seen[n] {
				seen[n] = true
				specs = append(specs, SystemSpec{Name: n, Type: c.Envs[e].Systems[n].Type})
			}
		}
	}
	return b, specs, nil
}

// LinkClaude makes the skill discoverable by Claude Code, which only scans
// .claude/skills: it adds a relative symlink .claude/skills/<name> -> dir
// next to the .agents folder that holds dir. An existing entry is left alone.
// It returns the link path, or "" when dir is not under .agents/skills.
func LinkClaude(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(abs)
	if filepath.Base(parent) != "skills" || filepath.Base(filepath.Dir(parent)) != ".agents" {
		return "", nil
	}
	root := filepath.Dir(filepath.Dir(parent))
	link := filepath.Join(root, ".claude", "skills", filepath.Base(abs))
	if _, err := os.Lstat(link); err == nil {
		return link, nil
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return "", errs.Wrap(errs.Runtime, err, "mkdir")
	}
	rel, err := filepath.Rel(filepath.Dir(link), abs)
	if err != nil {
		return "", err
	}
	if err := os.Symlink(rel, link); err != nil {
		return "", errs.Wrap(errs.Runtime, err, "link %s", link).
			WithHint("on Windows enable Developer Mode for symlinks, or copy the folder into .claude/skills")
	}
	return link, nil
}

func render(name string, data any) ([]byte, error) {
	var b bytes.Buffer
	if err := tmpl.ExecuteTemplate(&b, name, data); err != nil {
		return nil, errs.Wrap(errs.Runtime, err, "render %s", name)
	}
	return b.Bytes(), nil
}

func marshal(n *yaml.Node) ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return nil, errs.Wrap(errs.Runtime, err, "encode config")
	}
	_ = enc.Close()
	return b.Bytes(), nil
}

const configHeader = `# tmvp config for %s — holds NO secret values, only $(command)s that fetch them.
# Fill every "$file(sec get REPLACE_ME_… --out ${out})" slot with your secret manager's command, e.g.
#   "$file(sec get NAME --out ${out})"  "$(op read op://Vault/item/field)"  "$(gcloud secrets versions access latest --secret=NAME)"
# and every REPLACE_ME field with the real host/namespace/URL. Then: tmvp --skill <dir> validate && tmvp --skill <dir> check`

func configDoc(o Options) *yaml.Node {
	envs := mapping()
	for _, e := range o.Envs {
		envs.Content = append(envs.Content, str(e), envNode(o.Product, e, o.Systems))
	}
	root := mapping(
		pair{K: "version", V: &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: "1"}},
		pair{K: "product", V: str(o.Product)},
		pair{K: "logging", V: mapping(pair{K: "sink", V: str("file")}, pair{K: "path", V: str("~/.tmvp/" + o.Product + "/logs")})},
		pair{K: "assets", V: flowSeq("assets"), Comment: "asset roots searched in order (relative to this file); add folders from other repos, e.g. ../ops-repo/tmvp-assets"},
		pair{K: "vars", V: emptyMap(), Comment: "plain values, reusable as ${vars.x}"},
		pair{K: "secrets", V: emptyMap(), Comment: "named $(command)s shared by all envs, used as ${secrets.x}"},
		pair{K: "envs", V: envs},
	)
	root.HeadComment = fmt.Sprintf(configHeader, o.Product)
	return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
}

func flowSeq(items ...string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	for _, i := range items {
		n.Content = append(n.Content, str(i))
	}
	return n
}

func emptyMap() *yaml.Node { n := mapping(); n.Style = yaml.FlowStyle; return n }

func envNode(product, env string, systems []SystemSpec) *yaml.Node {
	secrets := emptyMap()
	sys := mapping()
	for _, s := range systems {
		b := buildSystem(slotCtx{product, env, s.Name, s.Type})
		sys.Content = append(sys.Content, str(s.Name), b.Node)
		addPairs(secrets, b.Secrets)
	}
	if len(sys.Content) == 0 {
		sys.Style = yaml.FlowStyle
	}
	typ := config.NormalizeEnvType(env)
	switch typ {
	case "prod", "staging", "dev", "test", "local":
	default:
		typ = "REPLACE_ME" // prod | staging | dev | test | local
	}
	return mapping(
		pair{K: "type", V: str(typ)},
		pair{K: "destructive", V: boolean(typ == "dev" || typ == "local")},
		pair{K: "vars", V: emptyMap()},
		pair{K: "secrets", V: secrets},
		pair{K: "systems", V: sys},
	)
}

func addPairs(m *yaml.Node, ps []pair) {
	for _, p := range ps {
		m.Style = 0
		m.Content = append(m.Content, mapping(p).Content...)
	}
}

func writeNew(p string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return errs.Wrap(errs.Runtime, err, "mkdir")
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil // never overwrite hand-written files
		}
		return errs.Wrap(errs.Runtime, err, "create %s", p)
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		return errs.Wrap(errs.Runtime, err, "write %s", p)
	}
	return nil
}

// writeAssets creates the empty folders people fill for one system:
// assets/<name>/<kind>/ (queries, scripts, pages or apis) and notes/.
// Nothing is pre-written: what exists is discovered live with `ls`.
func writeAssets(dir string, s SystemSpec) error {
	ad := filepath.Join(dir, "assets", s.Name)
	for _, k := range []string{savedDir(s.Type), "notes"} {
		if k == "" {
			continue
		}
		if err := writeNew(filepath.Join(ad, k, ".gitkeep"), nil); err != nil {
			return err
		}
	}
	return nil
}

// AddSystem adds system name:type to the config at cfgPath (every env, or
// only envs) keeping comments and order, and creates its asset folder.
func AddSystem(cfgPath, name, typ string, envs []string) ([]string, error) {
	if err := checkName("system", name); err != nil {
		return nil, err
	}
	typ = strings.ToLower(typ)
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, errs.Wrap(errs.Config, err, "read config")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, errs.Wrap(errs.Config, err, "parse config")
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errs.New(errs.Config, "config: top level must be a mapping")
	}
	root := doc.Content[0]
	product := ""
	if p := get(root, "product"); p != nil {
		product = p.Value
	}
	envsN := get(root, "envs")
	if envsN == nil || envsN.Kind != yaml.MappingNode || len(envsN.Content) == 0 {
		return nil, errs.New(errs.Config, "config has no envs")
	}
	want := map[string]bool{}
	for _, e := range envs {
		want[e] = true
	}
	var touched, all []string
	for i := 0; i+1 < len(envsN.Content); i += 2 {
		en, ev := envsN.Content[i].Value, envsN.Content[i+1]
		all = append(all, en)
		if len(want) > 0 && !want[en] {
			continue
		}
		delete(want, en)
		if ev.Kind != yaml.MappingNode {
			return nil, errs.New(errs.Config, "envs.%s must be a mapping", en)
		}
		sys := ensureMap(ev, "systems")
		if get(sys, name) != nil {
			return nil, errs.New(errs.Usage, "envs.%s.systems.%s already exists", en, name)
		}
		bs := buildSystem(slotCtx{product, en, name, typ})
		sys.Style = 0
		sys.Content = append(sys.Content, str(name), bs.Node)
		if len(bs.Secrets) > 0 {
			sec := ensureMap(ev, "secrets")
			for _, p := range bs.Secrets {
				if get(sec, p.K) != nil {
					return nil, errs.New(errs.Usage, "envs.%s.secrets.%s already exists", en, p.K)
				}
			}
			addPairs(sec, bs.Secrets)
		}
		touched = append(touched, en)
	}
	for e := range want {
		return nil, errs.New(errs.Usage, "no env %q in config (have %s)", e, strings.Join(all, ", "))
	}
	out, err := marshal(&doc)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(cfgPath, out, 0o644); err != nil {
		return nil, errs.Wrap(errs.Runtime, err, "write config")
	}
	if err := writeAssets(filepath.Dir(cfgPath), SystemSpec{name, typ}); err != nil {
		return nil, err
	}
	if err := Sync(cfgPath); err != nil {
		return nil, err
	}
	return touched, nil
}

func get(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func ensureMap(m *yaml.Node, key string) *yaml.Node {
	if n := get(m, key); n != nil {
		if n.Kind == yaml.MappingNode {
			return n
		}
		// `systems:` with null value → turn into a mapping in place
		n.Kind, n.Tag, n.Value = yaml.MappingNode, "!!map", ""
		return n
	}
	n := mapping()
	m.Content = append(m.Content, str(key), n)
	return n
}
